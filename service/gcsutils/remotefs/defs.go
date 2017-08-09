package remotefs

import (
	"errors"
	"os"
	"time"
)

// RemotefsCmd is the name of the remotefs meta command
const RemotefsCmd = "remotefs"

// Name of the commands when called from the cli context (remotefs <CMD> ...)
const (
	StatCmd           = "stat"
	LstatCmd          = "lstat"
	ReadlinkCmd       = "readlink"
	MkdirCmd          = "mkdir"
	MkdirAllCmd       = "mkdirall"
	RemoveCmd         = "remove"
	RemoveAllCmd      = "removeall"
	LinkCmd           = "link"
	SymlinkCmd        = "symlink"
	LchmodCmd         = "lchmod"
	LchownCmd         = "lchown"
	MknodCmd          = "mknod"
	MkfifoCmd         = "mkfifo"
	OpenFileCmd       = "openfile"
	ReadFileCmd       = "readfile"
	WriteFileCmd      = "writefile"
	ReadDirCmd        = "readdir"
	ResolvePathCmd    = "resolvepath"
	ExtractArchiveCmd = "extractarchive"
	ArchivePathCmd    = "archivepath"
)

// ErrInvalid is returned if the parameters are invalid
var ErrInvalid = errors.New("invalid arguments")

// ErrUnknown is returned for an unknown remotefs command
var ErrUnknown = errors.New("unkown command")

// ExportedError is the serialized version of the a Go error.
// It also provides a trivial implementation of the error interface.
type ExportedError struct {
	ErrString string
	ErrNum    int `json:",omitempty"`
}

func (ee *ExportedError) Error() string {
	return ee.ErrString
}

// FileInfo is the stat struct returned by the remotefs system. It
// fulfills the os.FileInfo interface.
type FileInfo struct {
	NameVar    string
	SizeVar    int64
	ModeVar    os.FileMode
	ModTimeVar int64 // Serialization of time.Time breaks in travis, so use an int
	IsDirVar   bool
}

var _ os.FileInfo = &FileInfo{}

func (f *FileInfo) Name() string       { return f.NameVar }
func (f *FileInfo) Size() int64        { return f.SizeVar }
func (f *FileInfo) Mode() os.FileMode  { return f.ModeVar }
func (f *FileInfo) ModTime() time.Time { return time.Unix(0, f.ModTimeVar) }
func (f *FileInfo) IsDir() bool        { return f.IsDirVar }
func (f *FileInfo) Sys() interface{}   { return nil }

// FileHeader is a header for remote *os.File operations for remotefs.OpenFile
type FileHeader struct {
	Cmd  uint32
	Size uint64
}

const (
	Read      uint32 = iota // Read request command
	Write                   // Write request command
	Seek                    // Seek request command
	Close                   // Close request command
	CmdOK                   // CmdOK is a response meaning request succeeded
	CmdFailed               // CmdFailed is a response meaning request failed.
)

// SeekHeader is header for the Seek operation for remotefs.OpenFile
type SeekHeader struct {
	Offset int64
	Whence int32
}
