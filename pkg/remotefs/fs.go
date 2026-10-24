package remotefs

import (
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"sync"
	"time"

	"github.com/stashapp/stash/pkg/file"
	"github.com/stashapp/stash/pkg/models"
	pb "github.com/stashapp/stash/pkg/remotefs/stashfsv1"
)

const (
	// maxReadAt is the server-side limit for a single ReadAt call.
	maxReadAt = 8 << 20
	// blockSize is the granularity of the per-file read cache. Sequential
	// readers (hashing fallbacks, image decoding) and the random reads of the
	// zip central directory both benefit from fetching whole blocks.
	blockSize = 1 << 20
	// maxCachedBlocks bounds memory per open file.
	maxCachedBlocks = 4
)

// FS is a models.FS / file.LibraryFS backed by stash-fsd.
type FS struct {
	c *Client

	caseSensitive sync.Map // path -> bool
}

var (
	_ models.FS          = (*FS)(nil)
	_ file.LibraryFS     = (*FS)(nil)
	_ file.Fingerprinter = (*FS)(nil)
)

// NewFS returns a file system that performs all operations through c.
func NewFS(c *Client) *FS {
	return &FS{c: c}
}

// Client returns the underlying client.
func (f *FS) Client() *Client {
	return f.c
}

func (f *FS) Stat(name string) (fs.FileInfo, error) {
	ctx, cancel := f.c.unaryCtx()
	defer cancel()
	ret, err := f.c.fs.Stat(ctx, &pb.PathRequest{Path: name})
	if err != nil {
		return nil, mapError("stat", name, err)
	}
	return newFileInfo(ret), nil
}

func (f *FS) Lstat(name string) (fs.FileInfo, error) {
	ctx, cancel := f.c.unaryCtx()
	defer cancel()
	ret, err := f.c.fs.Lstat(ctx, &pb.PathRequest{Path: name})
	if err != nil {
		return nil, mapError("lstat", name, err)
	}
	return newFileInfo(ret), nil
}

// Open stats the file (so that missing files fail here, like os.Open) and
// returns a handle that reads lazily through ReadAt calls.
func (f *FS) Open(name string) (fs.ReadDirFile, error) {
	info, err := f.Stat(name)
	if err != nil {
		var pe *fs.PathError
		if errors.As(err, &pe) {
			pe.Op = "open"
		}
		return nil, err
	}

	return &remoteFile{
		fs:   f,
		name: name,
		info: info.(*fileInfo),
	}, nil
}

func (f *FS) OpenZip(name string, size int64) (models.ZipFS, error) {
	return file.NewZipFS(f, name, size)
}

func (f *FS) IsPathCaseSensitive(path string) (bool, error) {
	if v, ok := f.caseSensitive.Load(path); ok {
		return v.(bool), nil
	}

	ctx, cancel := f.c.unaryCtx()
	defer cancel()
	ret, err := f.c.fs.IsPathCaseSensitive(ctx, &pb.PathRequest{Path: path})
	if err != nil {
		return false, mapError("casesensitive", path, err)
	}

	f.caseSensitive.Store(path, ret.Value)
	return ret.Value, nil
}

func (f *FS) EvalSymlinks(path string) (string, error) {
	ctx, cancel := f.c.unaryCtx()
	defer cancel()
	ret, err := f.c.fs.EvalSymlinks(ctx, &pb.PathRequest{Path: path})
	if err != nil {
		return "", mapError("evalsymlinks", path, err)
	}
	return ret.Path, nil
}

// ReadFile reads a whole (small) file, e.g. .stashignore, funscripts or
// caption files.
func (f *FS) ReadFile(path string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), f.c.callTimeout)
	defer cancel()

	stream, err := f.c.fs.ReadFile(ctx, &pb.ReadFileRequest{Path: path})
	if err != nil {
		return nil, mapError("read", path, err)
	}

	var buf bytes.Buffer
	for {
		chunk, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, mapError("read", path, err)
		}
		buf.Write(chunk.Data)
	}

	return buf.Bytes(), nil
}

func (f *FS) Mkdir(path string, _ fs.FileMode) error {
	return f.mkdir(path, false)
}

func (f *FS) MkdirAll(path string, _ fs.FileMode) error {
	return f.mkdir(path, true)
}

func (f *FS) mkdir(path string, all bool) error {
	ctx, cancel := f.c.unaryCtx()
	defer cancel()
	_, err := f.c.fs.Mkdir(ctx, &pb.MkdirRequest{Path: path, All: all})
	return mapError("mkdir", path, err)
}

// Rename moves oldpath to newpath, copying across volumes when needed
// (fsutil.SafeMove semantics, implemented server side).
func (f *FS) Rename(oldpath, newpath string) error {
	// cross-volume moves copy data and may take a long time
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Hour)
	defer cancel()
	_, err := f.c.fs.Rename(ctx, &pb.RenameRequest{OldPath: oldpath, NewPath: newpath})
	if err != nil {
		var pe *fs.PathError
		mapped := mapError("rename", oldpath, err)
		if errors.As(mapped, &pe) {
			return &osLinkError{Op: "rename", Old: oldpath, New: newpath, Err: pe.Err}
		}
		return mapped
	}
	return nil
}

func (f *FS) Remove(path string) error {
	ctx, cancel := f.c.unaryCtx()
	defer cancel()
	_, err := f.c.fs.Remove(ctx, &pb.PathRequest{Path: path})
	return mapError("remove", path, err)
}

func (f *FS) RemoveAll(path string) error {
	ctx, cancel := context.WithTimeout(context.Background(), time.Hour)
	defer cancel()
	_, err := f.c.fs.RemoveAll(ctx, &pb.PathRequest{Path: path})
	return mapError("removeall", path, err)
}

func (f *FS) MoveToTrash(path string, trashDir string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Hour)
	defer cancel()
	ret, err := f.c.fs.MoveToTrash(ctx, &pb.TrashRequest{Path: path, TrashDir: trashDir})
	if err != nil {
		return "", mapError("trash", path, err)
	}
	return ret.Path, nil
}

// Fingerprint computes md5 and/or oshash next to the data.
func (f *FS) Fingerprint(ctx context.Context, path string, zipEntry string, md5, oshash bool) (*file.FingerprintResult, error) {
	var algos []pb.HashAlgorithm
	if md5 {
		algos = append(algos, pb.HashAlgorithm_HASH_ALGORITHM_MD5)
	}
	if oshash {
		algos = append(algos, pb.HashAlgorithm_HASH_ALGORITHM_OSHASH)
	}

	ret, err := f.c.hasher.Fingerprint(ctx, &pb.FingerprintRequest{
		Path:       path,
		ZipEntry:   zipEntry,
		Algorithms: algos,
	})
	if err != nil {
		return nil, mapError("fingerprint", path, err)
	}

	return &file.FingerprintResult{
		MD5:    ret.Md5,
		Oshash: ret.Oshash,
	}, nil
}

// Walk streams the tree below root (root first, depth first, sorted). fn is
// called for every entry; returning fs.SkipDir from fn for a directory skips
// its contents, any other error stops the walk.
func (f *FS) Walk(ctx context.Context, root string, followSymlinks bool, fn func(info fs.FileInfo, err error) error) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	stream, err := f.c.fs.Walk(ctx, &pb.WalkRequest{Root: root, FollowSymlinks: followSymlinks})
	if err != nil {
		return mapError("walk", root, err)
	}

	var skipPrefix string
	for {
		batch, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return mapError("walk", root, err)
		}

		for _, we := range batch.Errors {
			if err := fn(nil, &fs.PathError{Op: "walk", Path: we.Path, Err: errors.New(we.Message)}); err != nil && !errors.Is(err, fs.SkipDir) {
				return err
			}
		}

		for _, e := range batch.Entries {
			if skipPrefix != "" && len(e.Path) > len(skipPrefix) && e.Path[:len(skipPrefix)] == skipPrefix {
				continue
			}
			skipPrefix = ""

			err := fn(newFileInfo(e), nil)
			if errors.Is(err, fs.SkipDir) {
				if e.IsDir {
					skipPrefix = e.Path + "/"
				}
				continue
			}
			if err != nil {
				return err
			}
		}
	}
}

// osLinkError mirrors os.LinkError without importing os semantics for paths.
type osLinkError struct {
	Op  string
	Old string
	New string
	Err error
}

func (e *osLinkError) Error() string {
	return e.Op + " " + e.Old + " " + e.New + ": " + e.Err.Error()
}

func (e *osLinkError) Unwrap() error { return e.Err }

// ---------------------------------------------------------------------------
// file handle
// ---------------------------------------------------------------------------

type block struct {
	off  int64
	data []byte
	eof  bool
}

type remoteFile struct {
	fs   *FS
	name string
	info *fileInfo

	mu     sync.Mutex
	offset int64
	blocks []*block // most recently used last

	dirEntries []fs.DirEntry
	dirLoaded  bool
	dirPos     int
}

func (r *remoteFile) Stat() (fs.FileInfo, error) {
	return r.info, nil
}

func (r *remoteFile) Close() error {
	r.mu.Lock()
	r.blocks = nil
	r.mu.Unlock()
	return nil
}

func (r *remoteFile) Read(p []byte) (int, error) {
	r.mu.Lock()
	off := r.offset
	r.mu.Unlock()

	n, err := r.ReadAt(p, off)

	r.mu.Lock()
	r.offset = off + int64(n)
	r.mu.Unlock()

	if n > 0 && errors.Is(err, io.EOF) {
		// io.Reader may return data together with EOF, but returning nil
		// here matches os.File and avoids surprising callers.
		err = nil
	}
	return n, err
}

func (r *remoteFile) Seek(offset int64, whence int) (int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	var abs int64
	switch whence {
	case io.SeekStart:
		abs = offset
	case io.SeekCurrent:
		abs = r.offset + offset
	case io.SeekEnd:
		abs = r.info.Size() + offset
	default:
		return 0, &fs.PathError{Op: "seek", Path: r.name, Err: fs.ErrInvalid}
	}
	if abs < 0 {
		return 0, &fs.PathError{Op: "seek", Path: r.name, Err: fs.ErrInvalid}
	}
	r.offset = abs
	return abs, nil
}

// ReadAt implements io.ReaderAt. It is safe for concurrent use.
func (r *remoteFile) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, &fs.PathError{Op: "readat", Path: r.name, Err: fs.ErrInvalid}
	}

	// Large reads bypass the block cache.
	if len(p) > blockSize*2 {
		return r.readDirect(p, off)
	}

	total := 0
	for total < len(p) {
		cur := off + int64(total)
		blockOff := cur - cur%blockSize

		b, err := r.getBlock(blockOff)
		if err != nil {
			return total, err
		}

		start := int(cur - b.off)
		if start >= len(b.data) {
			return total, io.EOF
		}

		n := copy(p[total:], b.data[start:])
		total += n

		if b.eof && start+n >= len(b.data) && total < len(p) {
			return total, io.EOF
		}
	}

	return total, nil
}

func (r *remoteFile) getBlock(off int64) (*block, error) {
	r.mu.Lock()
	for i, b := range r.blocks {
		if b.off == off {
			// move to MRU position
			r.blocks = append(append(r.blocks[:i:i], r.blocks[i+1:]...), b)
			r.mu.Unlock()
			return b, nil
		}
	}
	r.mu.Unlock()

	data, eof, err := r.fs.readAt(r.name, off, blockSize)
	if err != nil {
		return nil, err
	}

	b := &block{off: off, data: data, eof: eof || len(data) < blockSize}

	r.mu.Lock()
	r.blocks = append(r.blocks, b)
	if len(r.blocks) > maxCachedBlocks {
		r.blocks = r.blocks[len(r.blocks)-maxCachedBlocks:]
	}
	r.mu.Unlock()

	return b, nil
}

func (r *remoteFile) readDirect(p []byte, off int64) (int, error) {
	total := 0
	for total < len(p) {
		want := len(p) - total
		if want > maxReadAt {
			want = maxReadAt
		}

		data, eof, err := r.fs.readAt(r.name, off+int64(total), want)
		if err != nil {
			return total, err
		}

		n := copy(p[total:], data)
		total += n

		if eof || n < want {
			if total < len(p) {
				return total, io.EOF
			}
			break
		}
	}
	return total, nil
}

func (r *remoteFile) ReadDir(n int) ([]fs.DirEntry, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if !r.dirLoaded {
		ctx, cancel := r.fs.c.unaryCtx()
		defer cancel()
		ret, err := r.fs.c.fs.ReadDir(ctx, &pb.PathRequest{Path: r.name})
		if err != nil {
			return nil, mapError("readdir", r.name, err)
		}

		r.dirEntries = make([]fs.DirEntry, len(ret.Entries))
		for i, e := range ret.Entries {
			r.dirEntries[i] = dirEntry{newFileInfo(e)}
		}
		r.dirLoaded = true
	}

	remaining := r.dirEntries[r.dirPos:]
	if n <= 0 {
		r.dirPos = len(r.dirEntries)
		return remaining, nil
	}

	if len(remaining) == 0 {
		return nil, io.EOF
	}

	if n > len(remaining) {
		n = len(remaining)
	}
	r.dirPos += n
	return remaining[:n], nil
}

func (f *FS) readAt(name string, off int64, length int) ([]byte, bool, error) {
	ctx, cancel := f.c.unaryCtx()
	defer cancel()
	ret, err := f.c.fs.ReadAt(ctx, &pb.ReadAtRequest{Path: name, Offset: off, Length: int32(length)})
	if err != nil {
		return nil, false, mapError("read", name, err)
	}
	return ret.Data, ret.Eof, nil
}
