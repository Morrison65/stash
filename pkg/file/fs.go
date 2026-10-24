package file

import (
	"io"
	"io/fs"
	"os"

	"github.com/stashapp/stash/pkg/fsutil"
	"github.com/stashapp/stash/pkg/models"
)

// Opener provides an interface to open a file.
type Opener interface {
	Open() (io.ReadCloser, error)
}

type fsOpener struct {
	fs   models.FS
	name string
}

func (o *fsOpener) Open() (io.ReadCloser, error) {
	return o.fs.Open(o.name)
}

// FS returns the file system the opener reads from.
func (o *fsOpener) FS() models.FS { return o.fs }

// Path returns the path of the file within FS.
func (o *fsOpener) Path() string { return o.name }

// PathOpener is an Opener that exposes its file system and path, allowing
// callers to use file system specific fast paths (e.g. remote hashing).
type PathOpener interface {
	Opener
	FS() models.FS
	Path() string
}

// OsFS is a file system backed by the OS.
type OsFS struct{}

func (f *OsFS) Create(name string) (*os.File, error) {
	return os.Create(name)
}

func (f *OsFS) MkdirAll(path string, perm fs.FileMode) error {
	return os.MkdirAll(path, perm)
}

func (f *OsFS) Remove(name string) error {
	return os.Remove(name)
}

// Rename moves oldpath to newpath, copying across file systems if needed.
func (f *OsFS) Rename(oldpath, newpath string) error {
	return fsutil.SafeMove(oldpath, newpath)
}

func (f *OsFS) RemoveAll(path string) error {
	return os.RemoveAll(path)
}

func (f *OsFS) Stat(name string) (fs.FileInfo, error) {
	return os.Stat(name)
}

func (f *OsFS) Lstat(name string) (fs.FileInfo, error) {
	return os.Lstat(name)
}

func (f *OsFS) Open(name string) (fs.ReadDirFile, error) {
	return os.Open(name)
}

func (f *OsFS) OpenZip(name string, size int64) (models.ZipFS, error) {
	return newZipFS(f, name, size)
}

func (f *OsFS) IsPathCaseSensitive(path string) (bool, error) {
	return fsutil.IsFsPathCaseSensitive(path)
}
