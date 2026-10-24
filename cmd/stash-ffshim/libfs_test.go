package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"

	"github.com/stashapp/stash/pkg/remotefs"
	"github.com/stashapp/stash/pkg/remotefs/localserver"
)

type tokenCreds string

func (t tokenCreds) GetRequestMetadata(context.Context, ...string) (map[string]string, error) {
	return map[string]string{"authorization": "Bearer " + string(t)}, nil
}
func (t tokenCreds) RequireTransportSecurity() bool { return false }

func testLibrary(t *testing.T) (*remotefs.FS, string) {
	t.Helper()
	root := t.TempDir()
	srv := localserver.New(localserver.Config{Secret: "s", Roots: []string{root}, WorkspaceDir: filepath.Join(t.TempDir(), "ws")})
	lis := bufconn.Listen(1 << 20)
	g := grpc.NewServer(srv.ServerOptions()...)
	srv.Register(g)
	go func() { _ = g.Serve(lis) }()
	t.Cleanup(g.Stop)
	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithPerRPCCredentials(tokenCreds("s")),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return remotefs.NewFS(remotefs.NewClient(conn, 0)), root
}

func TestLibfs(t *testing.T) {
	lib, root := testLibrary(t)
	if err := os.MkdirAll(filepath.Join(root, "dir"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "dir", "a.nfo"), []byte("<movie/>"), 0o644); err != nil {
		t.Fatal(err)
	}
	dir := filepath.ToSlash(filepath.Join(root, "dir"))

	var out bytes.Buffer
	if err := libfsRun(lib, "cat", dir+"/a.nfo", &out); err != nil || out.String() != "<movie/>" {
		t.Errorf("cat = %q, %v", out.String(), err)
	}

	out.Reset()
	if err := libfsRun(lib, "stat", dir, &out); err != nil {
		t.Fatal(err)
	}
	var st libfsEntry
	if err := json.Unmarshal(out.Bytes(), &st); err != nil || !st.Dir || st.Mode != 0o040755 {
		t.Errorf("stat = %+v, %v", st, err)
	}

	out.Reset()
	if err := libfsRun(lib, "ls", dir, &out); err != nil {
		t.Fatal(err)
	}
	var ls []libfsEntry
	if err := json.Unmarshal(out.Bytes(), &ls); err != nil || len(ls) != 1 || ls[0].Name != "a.nfo" || ls[0].Size != 8 || ls[0].Dir {
		t.Errorf("ls = %+v, %v", ls, err)
	}

	if err := libfsRun(lib, "stat", dir+"/missing.nfo", &out); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("missing file: %v", err)
	}
	if err := libfsRun(lib, "cat", dir, &out); err == nil {
		t.Error("cat of a directory succeeded")
	}
}
