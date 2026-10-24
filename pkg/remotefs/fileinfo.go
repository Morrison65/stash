package remotefs

import (
	"io/fs"
	"time"

	pb "github.com/stashapp/stash/pkg/remotefs/stashfsv1"
)

// fileInfo implements fs.FileInfo from the wire representation.
type fileInfo struct {
	name    string
	path    string
	size    int64
	mode    fs.FileMode
	modTime time.Time
}

func newFileInfo(i *pb.FileInfo) *fileInfo {
	mode := fs.FileMode(i.Mode)
	// be defensive in case the server only sets the booleans
	if i.IsDir {
		mode |= fs.ModeDir
	}
	if i.IsSymlink {
		mode |= fs.ModeSymlink
	}

	return &fileInfo{
		name:    i.Name,
		path:    i.Path,
		size:    i.Size,
		mode:    mode,
		modTime: time.Unix(0, i.ModTimeUnixNano),
	}
}

func (i *fileInfo) Name() string       { return i.name }
func (i *fileInfo) Size() int64        { return i.size }
func (i *fileInfo) Mode() fs.FileMode  { return i.mode }
func (i *fileInfo) ModTime() time.Time { return i.modTime }
func (i *fileInfo) IsDir() bool        { return i.mode.IsDir() }
func (i *fileInfo) Sys() any           { return nil }

// Path returns the full path of the entry (forward-slash Windows form).
func (i *fileInfo) Path() string { return i.path }

// dirEntry implements fs.DirEntry.
type dirEntry struct {
	info *fileInfo
}

func (d dirEntry) Name() string               { return d.info.name }
func (d dirEntry) IsDir() bool                { return d.info.IsDir() }
func (d dirEntry) Type() fs.FileMode          { return d.info.mode.Type() }
func (d dirEntry) Info() (fs.FileInfo, error) { return d.info, nil }
