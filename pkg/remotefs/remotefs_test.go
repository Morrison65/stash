package remotefs_test

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/md5"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"math/rand"
	"net"
	"os"
	"path/filepath"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"

	"github.com/stashapp/stash/pkg/file"
	"github.com/stashapp/stash/pkg/hash/oshash"
	"github.com/stashapp/stash/pkg/remotefs"
	"github.com/stashapp/stash/pkg/remotefs/localserver"
)

const secret = "test-secret"

type env struct {
	root string
	fs   *remotefs.FS
}

func newEnv(t *testing.T, clientSecret string) *env {
	t.Helper()

	root := t.TempDir()
	srv := localserver.New(localserver.Config{
		Secret:       secret,
		Roots:        []string{root},
		WorkspaceDir: filepath.Join(t.TempDir(), "ws"),
	})

	lis := bufconn.Listen(1 << 20)
	g := grpc.NewServer(srv.ServerOptions()...)
	srv.Register(g)
	go func() { _ = g.Serve(lis) }()
	t.Cleanup(g.Stop)

	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithPerRPCCredentials(tokenCreds(clientSecret)),
		grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(16<<20)),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })

	return &env{root: root, fs: remotefs.NewFS(remotefs.NewClient(conn, 0))}
}

type tokenCreds string

func (t tokenCreds) GetRequestMetadata(context.Context, ...string) (map[string]string, error) {
	return map[string]string{"authorization": "Bearer " + string(t)}, nil
}
func (t tokenCreds) RequireTransportSecurity() bool { return false }

func writeRandom(t *testing.T, path string, size int) []byte {
	t.Helper()
	data := make([]byte, size)
	rand.New(rand.NewSource(int64(size))).Read(data)
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0644); err != nil {
		t.Fatal(err)
	}
	return data
}

func TestStatAndErrors(t *testing.T) {
	e := newEnv(t, secret)
	writeRandom(t, filepath.Join(e.root, "a.mp4"), 1234)

	fi, err := e.fs.Stat(filepath.Join(e.root, "a.mp4"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Size() != 1234 || fi.IsDir() || fi.Name() != "a.mp4" {
		t.Errorf("unexpected info %v %v %v", fi.Size(), fi.IsDir(), fi.Name())
	}

	if _, err := e.fs.Lstat(filepath.Join(e.root, "missing")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("want ErrNotExist, got %v", err)
	}
	if _, err := e.fs.Open(filepath.Join(e.root, "missing")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("open: want ErrNotExist, got %v", err)
	}
	if _, err := e.fs.Stat("/etc/passwd"); !errors.Is(err, fs.ErrPermission) {
		t.Errorf("outside root: want ErrPermission, got %v", err)
	}
}

func TestUnauthenticated(t *testing.T) {
	e := newEnv(t, "wrong")
	if _, err := e.fs.Stat(e.root); !errors.Is(err, remotefs.ErrUnauthenticated) {
		t.Errorf("want ErrUnauthenticated, got %v", err)
	}
}

func TestReadSeekReadAt(t *testing.T) {
	e := newEnv(t, secret)
	p := filepath.Join(e.root, "big.bin")
	// spans several cache blocks and is not block aligned
	data := writeRandom(t, p, 3*1024*1024+517)

	f, err := e.fs.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	// sequential read with an odd buffer size
	got, err := io.ReadAll(io.LimitReader(struct{ io.Reader }{f}, int64(len(data))))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, data) {
		t.Fatal("sequential read mismatch")
	}

	ra := f.(io.ReaderAt)
	for _, c := range []struct{ off, n int }{{0, 10}, {1<<20 - 5, 10}, {len(data) - 3, 3}, {2 << 20, 5 << 20}} {
		buf := make([]byte, c.n)
		n, err := ra.ReadAt(buf, int64(c.off))
		want := data[c.off:min(c.off+c.n, len(data))]
		if n != len(want) || !bytes.Equal(buf[:n], want) {
			t.Errorf("ReadAt(%d,%d): got %d bytes", c.off, c.n, n)
		}
		if c.off+c.n > len(data) && !errors.Is(err, io.EOF) {
			t.Errorf("ReadAt past end: want EOF, got %v", err)
		}
	}

	rs := f.(io.ReadSeeker)
	if _, err := rs.Seek(-100, io.SeekEnd); err != nil {
		t.Fatal(err)
	}
	tail, _ := io.ReadAll(rs)
	if !bytes.Equal(tail, data[len(data)-100:]) {
		t.Error("seek end read mismatch")
	}
}

func TestReadDirAndWalkMatchesLocal(t *testing.T) {
	e := newEnv(t, secret)
	for _, p := range []string{"b/2.mp4", "b/1.mp4", "a/x/y.jpg", "c.zip", "a/z.txt"} {
		writeRandom(t, filepath.Join(e.root, p), 10)
	}

	collect := func(f interface {
		Lstat(string) (fs.FileInfo, error)
	}, lib file.LibraryFS) []string {
		var ret []string
		err := file.SymWalk(lib, e.root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			rel, _ := filepath.Rel(e.root, path)
			ret = append(ret, rel)
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		return ret
	}

	local := collect(nil, &file.OsFS{})
	remote := collect(nil, e.fs)
	if len(local) != len(remote) {
		t.Fatalf("walk length mismatch: %v vs %v", local, remote)
	}
	for i := range local {
		if local[i] != remote[i] {
			t.Fatalf("walk order mismatch: %v vs %v", local, remote)
		}
	}

	// streaming Walk returns the same set in the same order
	var streamed []string
	err := e.fs.Walk(context.Background(), e.root, false, func(info fs.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(e.root, info.(interface{ Path() string }).Path())
		streamed = append(streamed, rel)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for i := range local {
		if local[i] != streamed[i] {
			t.Fatalf("streamed walk mismatch: %v vs %v", local, streamed)
		}
	}
}

func TestZipThroughRemote(t *testing.T) {
	e := newEnv(t, secret)
	p := filepath.Join(e.root, "gallery.zip")

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	contents := map[string][]byte{"01.jpg": bytes.Repeat([]byte("a"), 5000), "sub/02.png": []byte("png-data")}
	for name, data := range contents {
		w, _ := zw.Create(name)
		_, _ = w.Write(data)
	}
	_ = zw.Close()
	if err := os.WriteFile(p, buf.Bytes(), 0644); err != nil {
		t.Fatal(err)
	}

	zfs, err := e.fs.OpenZip(p, int64(buf.Len()))
	if err != nil {
		t.Fatal(err)
	}
	defer zfs.Close()

	r, err := zfs.OpenOnly(filepath.Join(p, "sub/02.png"))
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(r)
	if string(got) != "png-data" {
		t.Errorf("zip entry content mismatch: %q", got)
	}
}

func TestFingerprintMatchesGo(t *testing.T) {
	e := newEnv(t, secret)
	for _, size := range []int{200, 100 * 1024, 1 << 20} {
		p := filepath.Join(e.root, "f.bin")
		data := writeRandom(t, p, size)

		res, err := e.fs.Fingerprint(context.Background(), p, "", true, true)
		if err != nil {
			t.Fatal(err)
		}

		sum := md5.Sum(data)
		if res.MD5 != hex.EncodeToString(sum[:]) {
			t.Errorf("size %d: md5 mismatch", size)
		}

		want, err := oshash.FromReader(bytes.NewReader(data), int64(size))
		if err != nil {
			t.Fatal(err)
		}
		if res.Oshash != want {
			t.Errorf("size %d: oshash %s != %s", size, res.Oshash, want)
		}
	}
}

func TestMutations(t *testing.T) {
	e := newEnv(t, secret)
	dir := filepath.Join(e.root, "x", "y")
	if err := e.fs.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := e.fs.Mkdir(dir, 0755); !errors.Is(err, fs.ErrExist) {
		t.Errorf("mkdir existing: want ErrExist, got %v", err)
	}

	src := filepath.Join(dir, "a.mp4")
	writeRandom(t, src, 50)
	dst := filepath.Join(e.root, "a.mp4")
	if err := e.fs.Rename(src, dst); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dst); err != nil {
		t.Errorf("rename target missing: %v", err)
	}

	data, err := e.fs.ReadFile(dst)
	if err != nil || len(data) != 50 {
		t.Errorf("ReadFile: %d bytes, %v", len(data), err)
	}

	trashed, err := e.fs.MoveToTrash(dst, filepath.Join(e.root, ".trash"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(trashed); err != nil {
		t.Errorf("trashed file missing: %v", err)
	}

	if err := e.fs.RemoveAll(filepath.Join(e.root, "x")); err != nil {
		t.Fatal(err)
	}
	if err := e.fs.Remove(filepath.Join(e.root, "x")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("remove missing: want ErrNotExist, got %v", err)
	}
}
