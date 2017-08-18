// Package gcs defines the core functionality of the GCS. This includes all
// the code which manages container and their state, including interfacing with
// the container runtime, forwarding container stdio through
// transport.Connections, and configuring networking for a container.
package gcs

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"syscall"

	gcserr "github.com/Microsoft/opengcs/service/gcs/errors"
	"github.com/Microsoft/opengcs/service/gcs/oslayer"
	"github.com/Microsoft/opengcs/service/gcs/prot"
	"github.com/Microsoft/opengcs/service/gcs/runtime"
	"github.com/Microsoft/opengcs/service/gcs/stdio"
	shellwords "github.com/mattn/go-shellwords"
	oci "github.com/opencontainers/runtime-spec/specs-go"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
)

// gcsCore is an implementation of the Core interface, defining the
// functionality of the GCS.
type gcsCore struct {
	// Rtime is the Runtime interface used by the GCS core.
	Rtime runtime.Runtime

	// OS is the OS interface used by the GCS core.
	OS oslayer.OS

	containerCacheMutex sync.RWMutex
	// containerCache stores information about containers which persists
	// between calls into the gcsCore. It is structured as a map from container
	// ID to cache entry.
	containerCache map[string]*containerCacheEntry

	processCacheMutex sync.RWMutex
	// processCache stores information about processes which persists between calls
	// into the gcsCore. It is structured as a map from pid to cache entry.
	processCache map[int]*processCacheEntry
}

// NewGCSCore creates a new gcsCore struct initialized with the given Runtime.
func NewGCSCore(rtime runtime.Runtime, os oslayer.OS) *gcsCore {
	return &gcsCore{
		Rtime:          rtime,
		OS:             os,
		containerCache: make(map[string]*containerCacheEntry),
		processCache:   make(map[int]*processCacheEntry),
	}
}

// containerCacheEntry stores cached information for a single container.
type containerCacheEntry struct {
	ID                 string
	MappedVirtualDisks map[uint8]prot.MappedVirtualDisk
	MappedDirectories  map[uint32]prot.MappedDirectory
	NetworkAdapters    []prot.NetworkAdapter
	container          runtime.Container
	hasRunInitProcess  bool
	exitWg             sync.WaitGroup
	exitCode           int
}

func newContainerCacheEntry(id string) *containerCacheEntry {
	return &containerCacheEntry{
		ID:                 id,
		MappedVirtualDisks: make(map[uint8]prot.MappedVirtualDisk),
		MappedDirectories:  make(map[uint32]prot.MappedDirectory),
		exitCode:           -1,
	}
}
func (e *containerCacheEntry) AddNetworkAdapter(adapter prot.NetworkAdapter) {
	e.NetworkAdapters = append(e.NetworkAdapters, adapter)
}
func (e *containerCacheEntry) AddMappedVirtualDisk(disk prot.MappedVirtualDisk) error {
	if _, ok := e.MappedVirtualDisks[disk.Lun]; ok {
		return errors.Errorf("a mapped virtual disk with lun %d is already attached to container %s", disk.Lun, e.ID)
	}
	e.MappedVirtualDisks[disk.Lun] = disk
	return nil
}
func (e *containerCacheEntry) RemoveMappedVirtualDisk(disk prot.MappedVirtualDisk) {
	if _, ok := e.MappedVirtualDisks[disk.Lun]; !ok {
		logrus.Warnf("attempt to remove virtual disk with lun %d which is not attached to container %s", disk.Lun, e.ID)
		return
	}
	delete(e.MappedVirtualDisks, disk.Lun)
}
func (e *containerCacheEntry) AddMappedDirectory(dir prot.MappedDirectory) error {
	if _, ok := e.MappedDirectories[dir.Port]; ok {
		return errors.Errorf("a mapped directory with port %d is already attached to container %s", dir.Port, e.ID)
	}
	e.MappedDirectories[dir.Port] = dir
	return nil
}
func (e *containerCacheEntry) RemoveMappedDirectory(dir prot.MappedDirectory) {
	if _, ok := e.MappedDirectories[dir.Port]; !ok {
		logrus.Warnf("attempt to remove mapped directory with port %d which is not attached to container %s", dir.Port, e.ID)
		return
	}
	delete(e.MappedDirectories, dir.Port)
}

// processCacheEntry stores cached information for a single process.
type processCacheEntry struct {
	Tty         *stdio.TtyRelay
	ContainerID string // If "" a host process otherwise a container process.
	exitWg      sync.WaitGroup
	exitCode    int
}

func newProcessCacheEntry(containerID string) *processCacheEntry {
	return &processCacheEntry{ContainerID: containerID, exitCode: -1}
}

func (c *gcsCore) getContainer(id string) *containerCacheEntry {
	if entry, ok := c.containerCache[id]; ok {
		return entry
	}
	return nil
}

// CreateContainer creates all the infrastructure for a container, including
// setting up layers and networking, and then starts up its init process in a
// suspended state waiting for a call to StartContainer.
func (c *gcsCore) CreateContainer(id string, settings prot.VMHostedContainerSettings) error {
	c.containerCacheMutex.Lock()
	defer c.containerCacheMutex.Unlock()

	if c.getContainer(id) != nil {
		return errors.WithStack(gcserr.NewContainerExistsError(id))
	}

	containerEntry := newContainerCacheEntry(id)
	// We must add it here because we begin the wait for the init process before
	// returning to the HCS. This is safe if failures occur because we dont add to the
	// containerCache
	containerEntry.exitWg.Add(1)

	// Set up mapped virtual disks.
	if err := c.setupMappedVirtualDisks(id, settings.MappedVirtualDisks, containerEntry); err != nil {
		return errors.Wrapf(err, "failed to set up mapped virtual disks during create for container %s", id)
	}
	// Set up mapped directories.
	if err := c.setupMappedDirectories(id, settings.MappedDirectories, containerEntry); err != nil {
		return errors.Wrapf(err, "failed to set up mapped directories during create for container %s", id)
	}

	// Set up layers.
	scratch, layers, err := c.getLayerMounts(settings.SandboxDataPath, settings.Layers)
	if err != nil {
		return errors.Wrapf(err, "failed to get layer devices for container %s", id)
	}
	if err := c.mountLayers(id, scratch, layers); err != nil {
		return errors.Wrapf(err, "failed to mount layers for container %s", id)
	}

	// Stash network adapters away
	for _, adapter := range settings.NetworkAdapters {
		containerEntry.AddNetworkAdapter(adapter)
	}
	// Create the directory that will contain the resolv.conf file.
	//
	// TODO(rn): This isn't quite right but works. Basically, when
	// we do the network config in ExecProcess() the overlay for
	// the rootfs has already been created. When we then write
	// /etc/resolv.conf to the base layer it won't show up unless
	// /etc exists when the overlay is created. This is a bit
	// problematic as we basically later write to a what is
	// supposed to be read-only layer in the overlay...  Ideally,
	// dockerd would pass a runc config with a bind mount for
	// /etc/resolv.conf like it does on unix.
	if err := c.OS.MkdirAll(filepath.Join(baseFilesPath, "etc"), 0755); err != nil {
		return errors.Wrapf(err, "failed to create resolv.conf directory")
	}

	c.containerCache[id] = containerEntry

	return nil
}

// ExecProcess executes a new process in the container. It forwards the
// process's stdio through the members of the core.StdioSet provided.
func (c *gcsCore) ExecProcess(id string, params prot.ProcessParameters, stdioSet *stdio.ConnectionSet) (int, error) {
	c.containerCacheMutex.Lock()
	defer c.containerCacheMutex.Unlock()

	containerEntry := c.getContainer(id)
	if containerEntry == nil {
		return -1, errors.WithStack(gcserr.NewContainerDoesNotExistError(id))
	}
	processEntry := newProcessCacheEntry(id)

	var p runtime.Process
	if !containerEntry.hasRunInitProcess {
		containerEntry.hasRunInitProcess = true
		if err := c.writeConfigFile(id, params.OCISpecification); err != nil {
			containerEntry.exitWg.Done()
			return -1, err
		}

		container, err := c.Rtime.CreateContainer(id, c.getContainerStoragePath(id), stdioSet)
		if err != nil {
			containerEntry.exitWg.Done()
			return -1, err
		}

		containerEntry.container = container
		p = container
		processEntry.exitWg.Add(1)
		processEntry.Tty = p.Tty()

		// Configure network adapters in the namespace.
		for _, adapter := range containerEntry.NetworkAdapters {
			if err := c.configureAdapterInNamespace(container, adapter); err != nil {
				containerEntry.exitWg.Done()
				return -1, err
			}
		}

		go func() {
			state, err := container.Wait()
			c.containerCacheMutex.Lock()
			if err != nil {
				logrus.Error(err)
				if err := c.cleanupContainer(containerEntry); err != nil {
					logrus.Error(err)
				}
			}
			exitCode := state.ExitCode()
			logrus.Infof("container init process %d exited with exit status %d", p.Pid(), exitCode)

			if err := c.cleanupContainer(containerEntry); err != nil {
				logrus.Error(err)
			}
			c.containerCacheMutex.Unlock()

			// We are the only writer. Safe to do without a lock
			processEntry.exitCode = exitCode
			processEntry.exitWg.Done()

			// We are the only writer. Safe to do without a lock
			containerEntry.exitCode = exitCode
			containerEntry.exitWg.Done()

			c.containerCacheMutex.Lock()
			// This is safe because the init process WaitContainer has already
			// been initiated and thus removing from the map will not remove its
			// reference to the actual cacheEntry
			delete(c.containerCache, id)
			c.containerCacheMutex.Unlock()
		}()

		if err := container.Start(); err != nil {
			return -1, err
		}
	} else {
		ociProcess, err := processParametersToOCI(params)
		if err != nil {
			return -1, err
		}
		p, err = containerEntry.container.ExecProcess(ociProcess, stdioSet)
		if err != nil {
			return -1, err
		}
		processEntry.exitWg.Add(1)
		processEntry.Tty = p.Tty()

		go func() {
			state, err := p.Wait()
			if err != nil {
				logrus.Error(err)
			}
			exitCode := state.ExitCode()
			logrus.Infof("container process %d exited with exit status %d", p.Pid(), exitCode)

			processEntry.exitCode = exitCode
			processEntry.exitWg.Done()

			if err := p.Delete(); err != nil {
				logrus.Error(err)
			}
		}()
	}

	c.processCacheMutex.Lock()
	// If a processCacheEntry with the given pid already exists in the cache,
	// this will overwrite it. This behavior is expected. Processes are kept in
	// the cache even after they exit, which allows for exit hooks registered
	// on exited processed to still run. For example, if the HCS were to wait
	// on a process which had already exited (due to a race condition between
	// the wait call and the process exiting), the process's exit state would
	// still be available to send back to the HCS. However, when pids are
	// reused on the system, it makes sense to overwrite the old cache entry.
	// This is because registering an exit hook on the pid and expecting it to
	// apply to the old process no longer makes sense, so since the old
	// process's pid has been reused, its cache entry can also be reused.  This
	// applies to external processes as well.
	c.processCache[p.Pid()] = processEntry
	c.processCacheMutex.Unlock()
	return p.Pid(), nil
}

// SignalContainer sends the specified signal to the container's init process.
func (c *gcsCore) SignalContainer(id string, signal oslayer.Signal) error {
	c.containerCacheMutex.Lock()
	defer c.containerCacheMutex.Unlock()

	containerEntry := c.getContainer(id)
	if containerEntry == nil {
		return errors.WithStack(gcserr.NewContainerDoesNotExistError(id))
	}

	if containerEntry.container != nil {
		if err := containerEntry.container.Kill(signal); err != nil {
			return err
		}
	}

	return nil
}

// SignalProcess sends the signal specified in options to the given process.
func (c *gcsCore) SignalProcess(pid int, options prot.SignalProcessOptions) error {
	c.processCacheMutex.Lock()
	if _, ok := c.processCache[pid]; !ok {
		c.processCacheMutex.Unlock()
		return errors.WithStack(gcserr.NewProcessDoesNotExistError(pid))
	}
	c.processCacheMutex.Unlock()

	// Interpret signal value 0 as SIGKILL.
	// TODO: Remove this special casing when we are not worried about breaking
	// older Windows builds which don't support sending signals.
	var signal syscall.Signal
	if options.Signal == 0 {
		signal = syscall.SIGKILL
	} else {
		signal = syscall.Signal(options.Signal)
	}

	if err := c.OS.Kill(pid, signal); err != nil {
		return errors.Wrapf(err, "failed call to kill on process %d with signal %d", pid, options.Signal)
	}

	return nil
}

// ListProcesses returns all container processes, even zombies.
func (c *gcsCore) ListProcesses(id string) ([]runtime.ContainerProcessState, error) {
	c.containerCacheMutex.Lock()
	defer c.containerCacheMutex.Unlock()

	containerEntry := c.getContainer(id)
	if containerEntry == nil {
		return nil, errors.WithStack(gcserr.NewContainerDoesNotExistError(id))
	}

	if containerEntry.container == nil {
		return nil, nil
	}

	processes, err := containerEntry.container.GetAllProcesses()
	if err != nil {
		return nil, err
	}
	return processes, nil
}

// RunExternalProcess runs a process in the utility VM outside of a container's
// namespace.
// This can be used for things like debugging or diagnosing the utility VM's
// state.
func (c *gcsCore) RunExternalProcess(params prot.ProcessParameters, stdioSet *stdio.ConnectionSet) (pid int, err error) {
	ociProcess, err := processParametersToOCI(params)
	if err != nil {
		return -1, err
	}
	cmd := c.OS.Command(ociProcess.Args[0], ociProcess.Args[1:]...)
	cmd.SetDir(ociProcess.Cwd)
	cmd.SetEnv(ociProcess.Env)

	var relay *stdio.TtyRelay
	if params.EmulateConsole {
		// Allocate a console for the process.
		var (
			master      *os.File
			consolePath string
		)
		master, consolePath, err = stdio.NewConsole()
		if err != nil {
			return -1, errors.Wrap(err, "failed to create console for external process")
		}
		defer func() {
			if err != nil {
				master.Close()
			}
		}()

		console, err := c.OS.OpenFile(consolePath, os.O_RDWR, 0777)
		if err != nil {
			return -1, errors.Wrap(err, "failed to open console file for external process")
		}
		defer console.Close()

		relay = stdioSet.NewTtyRelay(master)
		cmd.SetStdin(console)
		cmd.SetStdout(console)
		cmd.SetStderr(console)
	} else {
		fileSet, err := stdioSet.Files()
		if err != nil {
			return -1, errors.Wrap(err, "failed to set cmd stdio")
		}
		defer fileSet.Close()
		defer stdioSet.Close()
		cmd.SetStdin(fileSet.In)
		cmd.SetStdout(fileSet.Out)
		cmd.SetStderr(fileSet.Err)
	}
	if err := cmd.Start(); err != nil {
		return -1, errors.Wrap(err, "failed call to Start for external process")
	}

	if relay != nil {
		relay.Start()
	}

	processEntry := newProcessCacheEntry("")
	processEntry.exitWg.Add(1)
	processEntry.Tty = relay
	go func() {
		if err := cmd.Wait(); err != nil {
			// TODO: When cmd is a shell, and last command in the shell
			// returned an error (e.g. typing a non-existing command gives
			// error 127), Wait also returns an error. We should find a way to
			// distinguish between these errors and ones which are actually
			// important.
			logrus.Error(errors.Wrap(err, "failed call to Wait for external process"))
		}
		exitCode := cmd.ExitState().ExitCode()
		logrus.Infof("external process %d exited with exit status %d", cmd.Process().Pid(), exitCode)

		if relay != nil {
			relay.Wait()
		}

		// We are the only writer safe to do without a lock.
		processEntry.exitCode = exitCode
		processEntry.exitWg.Done()
	}()

	pid = cmd.Process().Pid()
	c.processCacheMutex.Lock()
	c.processCache[pid] = processEntry
	c.processCacheMutex.Unlock()
	return pid, nil
}

// ModifySettings takes the given request and performs the modification it
// specifies. At the moment, this function only supports the request types Add
// and Remove, both for the resource type MappedVirtualDisk.
func (c *gcsCore) ModifySettings(id string, request prot.ResourceModificationRequestResponse) error {
	c.containerCacheMutex.Lock()
	defer c.containerCacheMutex.Unlock()

	containerEntry := c.getContainer(id)
	if containerEntry == nil {
		return errors.WithStack(gcserr.NewContainerDoesNotExistError(id))
	}

	settings, ok := request.Settings.(prot.ResourceModificationSettings)
	if !ok {
		return errors.New("the request's settings are not of type ResourceModificationSettings")
	}
	switch request.RequestType {
	case prot.RtAdd:
		switch request.ResourceType {
		case prot.PtMappedVirtualDisk:
			if err := c.setupMappedVirtualDisks(id, []prot.MappedVirtualDisk{*settings.MappedVirtualDisk}, containerEntry); err != nil {
				return errors.Wrapf(err, "failed to hot add mapped virtual disk for container %s", id)
			}
		case prot.PtMappedDirectory:
			if err := c.setupMappedDirectories(id, []prot.MappedDirectory{*settings.MappedDirectory}, containerEntry); err != nil {
				return errors.Wrapf(err, "failed to hot add mapped directory for container %s", id)
			}
		default:
			return errors.Errorf("the resource type \"%s\" is not supported for request type \"%s\"", request.ResourceType, request.RequestType)
		}
	case prot.RtRemove:
		switch request.ResourceType {
		case prot.PtMappedVirtualDisk:
			if err := c.removeMappedVirtualDisks(id, []prot.MappedVirtualDisk{*settings.MappedVirtualDisk}, containerEntry); err != nil {
				return errors.Wrapf(err, "failed to hot remove mapped virtual disk for container %s", id)
			}
		case prot.PtMappedDirectory:
			if err := c.removeMappedDirectories(id, []prot.MappedDirectory{*settings.MappedDirectory}, containerEntry); err != nil {
				return errors.Wrapf(err, "failed to hot remove mapped directory for container %s", id)
			}
		default:
			return errors.Errorf("the resource type \"%s\" is not supported for request type \"%s\"", request.ResourceType, request.RequestType)
		}
	default:
		return errors.Errorf("the request type \"%s\" is not supported", request.RequestType)
	}

	return nil
}

func (c *gcsCore) ResizeConsole(pid int, height, width uint16) error {
	c.processCacheMutex.Lock()
	var p *processCacheEntry
	var ok bool
	if p, ok = c.processCache[pid]; !ok {
		c.processCacheMutex.Unlock()
		return errors.WithStack(gcserr.NewProcessDoesNotExistError(pid))
	}
	c.processCacheMutex.Unlock()

	if p.Tty == nil {
		return fmt.Errorf("pid: %d, is not a tty and cannot be resized", pid)
	}

	return p.Tty.ResizeConsole(height, width)
}

// WaitContainer waits for a container to complete and returns its exist code.
func (c *gcsCore) WaitContainer(id string) (int, error) {
	c.containerCacheMutex.Lock()
	entry := c.getContainer(id)
	if entry == nil {
		c.containerCacheMutex.Unlock()
		return -1, errors.WithStack(gcserr.NewContainerDoesNotExistError(id))
	}
	c.containerCacheMutex.Unlock()

	entry.exitWg.Wait()
	return entry.exitCode, nil
}

// WaitProcess waits for a process to complete and returns its exist code.
func (c *gcsCore) WaitProcess(pid int) (int, error) {
	c.processCacheMutex.Lock()
	entry, ok := c.processCache[pid]
	if !ok {
		c.processCacheMutex.Unlock()
		return -1, errors.WithStack(gcserr.NewProcessDoesNotExistError(pid))
	}
	c.processCacheMutex.Unlock()

	entry.exitWg.Wait()
	return entry.exitCode, nil
}

// setupMappedVirtualDisks is a helper function which calls into the functions
// in storage.go to set up a set of mapped virtual disks for a given container.
// It then adds them to the container's cache entry.
// This function expects containerCacheMutex to be locked on entry.
func (c *gcsCore) setupMappedVirtualDisks(id string, disks []prot.MappedVirtualDisk, containerEntry *containerCacheEntry) error {
	mounts, err := c.getMappedVirtualDiskMounts(disks)
	if err != nil {
		return errors.Wrapf(err, "failed to get mapped virtual disk devices for container %s", id)
	}
	if err := c.mountMappedVirtualDisks(disks, mounts); err != nil {
		return errors.Wrapf(err, "failed to mount mapped virtual disks for container %s", id)
	}
	for _, disk := range disks {
		if err := containerEntry.AddMappedVirtualDisk(disk); err != nil {
			return err
		}
	}
	return nil
}

// setupMappedDirectories is a helper function which calls into the functions
// in storage.go to set up a set of mapped directories for a given container.
// It then adds them to the container's cache entry.
// This function expects containerCacheMutex to be locked on entry.
func (c *gcsCore) setupMappedDirectories(id string, dirs []prot.MappedDirectory, containerEntry *containerCacheEntry) error {
	if err := c.mountMappedDirectories(dirs); err != nil {
		return errors.Wrapf(err, "failed to mount mapped directories for container %s", id)
	}
	for _, dir := range dirs {
		if err := containerEntry.AddMappedDirectory(dir); err != nil {
			return err
		}
	}
	return nil
}

// removeMappedVirtualDisks is a helper function which calls into the functions
// in storage.go to unmount a set of mapped virtual disks for a given
// container. It then removes them from the container's cache entry.
// This function expects containerCacheMutex to be locked on entry.
func (c *gcsCore) removeMappedVirtualDisks(id string, disks []prot.MappedVirtualDisk, containerEntry *containerCacheEntry) error {
	if err := c.unmountMappedVirtualDisks(disks); err != nil {
		return errors.Wrapf(err, "failed to mount mapped virtual disks for container %s", id)
	}
	for _, disk := range disks {
		containerEntry.RemoveMappedVirtualDisk(disk)
	}
	return nil
}

// removeMappedDirectories is a helper function which calls into the functions
// in storage.go to unmount a set of mapped directories for a given container.
// It then removes them from the container's cache entry.
// This function expects containerCacheMutex to be locked on entry.
func (c *gcsCore) removeMappedDirectories(id string, dirs []prot.MappedDirectory, containerEntry *containerCacheEntry) error {
	if err := c.unmountMappedDirectories(dirs); err != nil {
		return errors.Wrapf(err, "failed to mount mapped directories for container %s", id)
	}
	for _, dir := range dirs {
		containerEntry.RemoveMappedDirectory(dir)
	}
	return nil
}

// processParametersToOCI converts the given ProcessParameters struct into an
// oci.Process struct for OCI version 1.0.0-rc5-dev. Since ProcessParameters
// doesn't include various fields which are available in oci.Process, default
// values for these fields are chosen.
func processParametersToOCI(params prot.ProcessParameters) (oci.Process, error) {
	var args []string
	if len(params.CommandArgs) == 0 {
		var err error
		args, err = processParamCommandLineToOCIArgs(params.CommandLine)
		if err != nil {
			return oci.Process{}, err
		}
	} else {
		args = params.CommandArgs
	}
	return oci.Process{
		Args:     args,
		Cwd:      params.WorkingDirectory,
		Env:      processParamEnvToOCIEnv(params.Environment),
		Terminal: params.EmulateConsole,

		// TODO: We might want to eventually choose alternate default values
		// for these.
		User: oci.User{UID: 0, GID: 0},
		Capabilities: &oci.LinuxCapabilities{
			Bounding: []string{
				"CAP_AUDIT_WRITE",
				"CAP_KILL",
				"CAP_NET_BIND_SERVICE",
				"CAP_SYS_ADMIN",
				"CAP_NET_ADMIN",
				"CAP_SETGID",
				"CAP_SETUID",
				"CAP_CHOWN",
				"CAP_FOWNER",
				"CAP_DAC_OVERRIDE",
				"CAP_NET_RAW",
			},
			Effective: []string{
				"CAP_AUDIT_WRITE",
				"CAP_KILL",
				"CAP_NET_BIND_SERVICE",
				"CAP_SYS_ADMIN",
				"CAP_NET_ADMIN",
				"CAP_SETGID",
				"CAP_SETUID",
				"CAP_CHOWN",
				"CAP_FOWNER",
				"CAP_DAC_OVERRIDE",
				"CAP_NET_RAW",
			},
			Inheritable: []string{
				"CAP_AUDIT_WRITE",
				"CAP_KILL",
				"CAP_NET_BIND_SERVICE",
				"CAP_SYS_ADMIN",
				"CAP_NET_ADMIN",
				"CAP_SETGID",
				"CAP_SETUID",
				"CAP_CHOWN",
				"CAP_FOWNER",
				"CAP_DAC_OVERRIDE",
				"CAP_NET_RAW",
			},
			Permitted: []string{
				"CAP_AUDIT_WRITE",
				"CAP_KILL",
				"CAP_NET_BIND_SERVICE",
				"CAP_SYS_ADMIN",
				"CAP_NET_ADMIN",
				"CAP_SETGID",
				"CAP_SETUID",
				"CAP_CHOWN",
				"CAP_FOWNER",
				"CAP_DAC_OVERRIDE",
				"CAP_NET_RAW",
			},
			Ambient: []string{
				"CAP_AUDIT_WRITE",
				"CAP_KILL",
				"CAP_NET_BIND_SERVICE",
				"CAP_SYS_ADMIN",
				"CAP_NET_ADMIN",
				"CAP_SETGID",
				"CAP_SETUID",
				"CAP_CHOWN",
				"CAP_FOWNER",
				"CAP_DAC_OVERRIDE",
				"CAP_NET_RAW",
			},
		},
		Rlimits: []oci.LinuxRlimit{
			oci.LinuxRlimit{Type: "RLIMIT_NOFILE", Hard: 1024, Soft: 1024},
		},
		NoNewPrivileges: true,
	}, nil
}

// processParamCommandLineToOCIArgs converts a CommandLine field from
// ProcessParameters (a space separate argument string) into an array of string
// arguments which can be used by an oci.Process.
func processParamCommandLineToOCIArgs(commandLine string) ([]string, error) {
	args, err := shellwords.Parse(commandLine)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to parse command line string \"%s\"", commandLine)
	}
	return args, nil
}

// processParamEnvToOCIEnv converts an Environment field from ProcessParameters
// (a map from environment variable to value) into an array of environment
// variable assignments (where each is in the form "<variable>=<value>") which
// can be used by an oci.Process.
func processParamEnvToOCIEnv(environment map[string]string) []string {
	environmentList := make([]string, 0, len(environment))
	for k, v := range environment {
		// TODO: Do we need to escape things like quotation marks in
		// environment variable values?
		environmentList = append(environmentList, fmt.Sprintf("%s=%s", k, v))
	}
	return environmentList
}
