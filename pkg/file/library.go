package file

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"

	"github.com/stashapp/stash/pkg/fsutil"
	"github.com/stashapp/stash/pkg/models"
)

// LibraryFS is the file system that holds the media library. In the
// microservice deployment it is backed by stash-fsd (pkg/remotefs); OsFS is
// the local implementation used for tests and single-host setups.
//
// All media-library access (scan, clean, delete, move, captions, funscripts,
// .stashignore, serving) must go through this interface rather than the os
// package, because the library is not mounted in the containers.
type LibraryFS interface {
	models.FS

	EvalSymlinks(path string) (string, error)
	ReadFile(path string) ([]byte, error)
	Mkdir(path string, perm fs.FileMode) error
	MkdirAll(path string, perm fs.FileMode) error
	// Rename moves a file or directory, copying across volumes if needed
	// (fsutil.SafeMove semantics).
	Rename(oldpath, newpath string) error
	Remove(path string) error
	RemoveAll(path string) error
	// MoveToTrash moves path into trashDir and returns the new path.
	MoveToTrash(path string, trashDir string) (string, error)
}

// FingerprintResult holds fingerprints computed by a Fingerprinter.
type FingerprintResult struct {
	MD5    string
	Oshash string
}

// Fingerprinter is implemented by library file systems that can compute
// I/O-bound fingerprints next to the data instead of streaming the file.
type Fingerprinter interface {
	Fingerprint(ctx context.Context, path string, zipEntry string, md5, oshash bool) (*FingerprintResult, error)
}

var (
	libraryMu sync.RWMutex
	library   LibraryFS = &OsFS{}
)

// Library returns the configured media library file system.
func Library() LibraryFS {
	libraryMu.RLock()
	defer libraryMu.RUnlock()
	return library
}

// SetLibrary replaces the media library file system. Call once at startup.
func SetLibrary(l LibraryFS) {
	libraryMu.Lock()
	defer libraryMu.Unlock()
	library = l
}

// IsWindowsPath reports whether p is an absolute Windows path in either
// slash form ("U:/x", "U:\x", "//server/share", `\\server\share`). In the
// container deployment these are exactly the media library paths; container
// paths (generated, cache, blobs, metadata) are Unix paths.
func IsWindowsPath(p string) bool {
	if len(p) >= 3 && p[1] == ':' && (p[2] == '/' || p[2] == '\\') {
		c := p[0] | 0x20
		return c >= 'a' && c <= 'z'
	}
	if len(p) == 2 && p[1] == ':' {
		c := p[0] | 0x20
		return c >= 'a' && c <= 'z'
	}
	return len(p) >= 2 && (p[:2] == "//" || p[:2] == `\\`)
}

// RoutingFS sends library paths to Remote and every other path to Local, so
// that code handling both library files and container files (e.g. the
// Deleter, which also removes generated files) works unchanged.
type RoutingFS struct {
	Remote LibraryFS
	Local  LibraryFS
	// IsRemote decides where a path lives. Defaults to IsWindowsPath.
	IsRemote func(path string) bool
}

func (r *RoutingFS) pick(path string) LibraryFS {
	isRemote := r.IsRemote
	if isRemote == nil {
		isRemote = IsWindowsPath
	}
	if isRemote(path) {
		return r.Remote
	}
	return r.Local
}

func (r *RoutingFS) Stat(name string) (fs.FileInfo, error) { return r.pick(name).Stat(name) }
func (r *RoutingFS) Lstat(name string) (fs.FileInfo, error) {
	return r.pick(name).Lstat(name)
}
func (r *RoutingFS) Open(name string) (fs.ReadDirFile, error) { return r.pick(name).Open(name) }
func (r *RoutingFS) OpenZip(name string, size int64) (models.ZipFS, error) {
	return r.pick(name).OpenZip(name, size)
}
func (r *RoutingFS) IsPathCaseSensitive(path string) (bool, error) {
	return r.pick(path).IsPathCaseSensitive(path)
}
func (r *RoutingFS) EvalSymlinks(path string) (string, error) {
	return r.pick(path).EvalSymlinks(path)
}
func (r *RoutingFS) ReadFile(path string) ([]byte, error) { return r.pick(path).ReadFile(path) }
func (r *RoutingFS) Mkdir(path string, perm fs.FileMode) error {
	return r.pick(path).Mkdir(path, perm)
}
func (r *RoutingFS) MkdirAll(path string, perm fs.FileMode) error {
	return r.pick(path).MkdirAll(path, perm)
}
func (r *RoutingFS) Rename(oldpath, newpath string) error {
	from, to := r.pick(oldpath), r.pick(newpath)
	if from != to {
		return fmt.Errorf("rename %s -> %s: cannot move between the media library and container storage", oldpath, newpath)
	}
	return from.Rename(oldpath, newpath)
}
func (r *RoutingFS) Remove(path string) error    { return r.pick(path).Remove(path) }
func (r *RoutingFS) RemoveAll(path string) error { return r.pick(path).RemoveAll(path) }
func (r *RoutingFS) MoveToTrash(path string, trashDir string) (string, error) {
	return r.pick(path).MoveToTrash(path, trashDir)
}

// Fingerprint delegates to the backing file system if it supports remote
// fingerprinting.
func (r *RoutingFS) Fingerprint(ctx context.Context, path string, zipEntry string, md5, oshash bool) (*FingerprintResult, error) {
	fp, ok := r.pick(path).(Fingerprinter)
	if !ok {
		return nil, fmt.Errorf("fingerprint %s: %w", path, errors.ErrUnsupported)
	}
	return fp.Fingerprint(ctx, path, zipEntry, md5, oshash)
}

// SupportsFingerprint reports whether Fingerprint can be used for path.
func (r *RoutingFS) SupportsFingerprint(path string) bool {
	_, ok := r.pick(path).(Fingerprinter)
	return ok
}

var _ LibraryFS = (*RoutingFS)(nil)

// NewZipFS opens the zip file at path within f as a file system.
func NewZipFS(f models.FS, path string, size int64) (models.ZipFS, error) {
	return newZipFS(f, path, size)
}

func (f *OsFS) EvalSymlinks(path string) (string, error) {
	return filepath.EvalSymlinks(path)
}

func (f *OsFS) ReadFile(path string) ([]byte, error) {
	return os.ReadFile(path)
}

func (f *OsFS) Mkdir(path string, perm fs.FileMode) error {
	return os.Mkdir(path, perm)
}

func (f *OsFS) MoveToTrash(path string, trashDir string) (string, error) {
	return fsutil.MoveToTrash(path, trashDir)
}

var _ LibraryFS = (*OsFS)(nil)

// NormalizeLibraryPath converts a Windows library path to the canonical
// forward-slash form with an upper-case drive letter ("u:\Media\" ->
// "U:/Media"). Other paths are only cleaned.
func NormalizeLibraryPath(p string) string {
	if !IsWindowsPath(p) {
		return filepath.Clean(p)
	}

	p = strings.ReplaceAll(p, `\`, "/")
	unc := strings.HasPrefix(p, "//")
	if unc {
		p = "/" + path.Clean(p[1:])
	} else {
		p = strings.ToUpper(p[:1]) + p[1:]
		if len(p) == 2 {
			// bare drive: "U:" -> "U:/"
			return p + "/"
		}
		p = path.Clean(p)
		if len(p) == 2 {
			p += "/"
		}
	}
	return p
}

// LibraryFileExists mirrors fsutil.FileExists for the library file system.
func LibraryFileExists(path string) (bool, error) {
	info, err := Library().Stat(path)
	if err == nil {
		return !info.IsDir(), nil
	}
	return false, err
}

// LibraryDirExists mirrors fsutil.DirExists for the library file system.
func LibraryDirExists(path string) (bool, error) {
	info, err := Library().Stat(path)
	if err != nil {
		return false, fs.ErrNotExist
	}
	if !info.IsDir() {
		return false, fmt.Errorf("path is not a directory <%s>", path)
	}
	return true, nil
}

// ServeLibraryFile serves a file from the library file system with range,
// conditional request and content type handling (like http.ServeFile).
func ServeLibraryFile(w http.ResponseWriter, r *http.Request, name string) {
	f, err := Library().Open(name)
	if err != nil {
		switch {
		case errors.Is(err, fs.ErrNotExist):
			http.Error(w, "404 page not found", http.StatusNotFound)
		case errors.Is(err, fs.ErrPermission):
			http.Error(w, "403 Forbidden", http.StatusForbidden)
		default:
			http.Error(w, "500 Internal Server Error", http.StatusInternalServerError)
		}
		return
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil || info.IsDir() {
		http.Error(w, "404 page not found", http.StatusNotFound)
		return
	}

	rs, ok := f.(io.ReadSeeker)
	if !ok {
		http.Error(w, "500 Internal Server Error", http.StatusInternalServerError)
		return
	}

	http.ServeContent(w, r, info.Name(), info.ModTime(), rs)
}
