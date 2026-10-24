package ffmpeg

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

type fakeRunner struct {
	args      []string
	inputs    []string
	workspace string
	stdout    []byte
	file      []byte
	stderr    []byte
	deleted   []string
}

func (f *fakeRunner) Probe(context.Context, string, []string) ([]byte, int64, error) {
	return nil, 0, nil
}

func (f *fakeRunner) Run(_ context.Context, _ bool, args []string, inputs []string, _ []byte, workspace string, w io.Writer) ([]byte, error) {
	f.args, f.inputs, f.workspace = args, inputs, workspace
	if w != nil {
		_, _ = w.Write(f.stdout)
	}
	return f.stderr, nil
}

func (f *fakeRunner) FetchWorkspaceFile(_ context.Context, _, _ string, w io.Writer) error {
	_, err := w.Write(f.file)
	return err
}

func (f *fakeRunner) DeleteWorkspace(_ context.Context, ws string) error {
	f.deleted = append(f.deleted, ws)
	return nil
}

func isWin(p string) bool { return len(p) > 2 && p[1] == ':' }

func withRunner(t *testing.T, r RemoteRunner) {
	t.Helper()
	SetRemote(r, isWin)
	t.Cleanup(func() { SetRemote(nil, nil) })
}

func TestPlanRemoteLocalOnly(t *testing.T) {
	withRunner(t, &fakeRunner{})
	if inv := planRemote([]string{"-i", "/generated/tmp/a.txt", "-c", "copy", "/generated/out.mp4"}); inv != nil {
		t.Fatalf("local-only command must run locally, got %+v", inv)
	}
}

func TestRunRemoteOutputFile(t *testing.T) {
	r := &fakeRunner{file: []byte("jpegdata"), stderr: []byte("frame=1")}
	withRunner(t, r)

	out := filepath.Join(t.TempDir(), "shot.jpg")
	args := []string{"-v", "error", "-ss", "5", "-i", "U:/Media/a b.mp4", "-frames:v", "1", "-y", out}

	handled, stderr, err := RunRemoteIfNeeded(context.Background(), args, nil, nil)
	if !handled || err != nil {
		t.Fatalf("handled=%v err=%v", handled, err)
	}

	wantArgs := []string{"-v", "error", "-ss", "5", "-i", "{input:0}", "-frames:v", "1", "-y", "{workspace}/out.jpg"}
	if !reflect.DeepEqual(r.args, wantArgs) {
		t.Errorf("args = %v, want %v", r.args, wantArgs)
	}
	if !reflect.DeepEqual(r.inputs, []string{"U:/Media/a b.mp4"}) {
		t.Errorf("inputs = %v", r.inputs)
	}
	if !strings.HasPrefix(r.workspace, "gen-") {
		t.Errorf("workspace = %q", r.workspace)
	}
	if got, _ := os.ReadFile(out); string(got) != "jpegdata" {
		t.Errorf("output file = %q", got)
	}
	if string(stderr) != "frame=1" {
		t.Errorf("stderr = %q", stderr)
	}
	if len(r.deleted) != 1 || r.deleted[0] != r.workspace {
		t.Errorf("workspace not deleted: %v", r.deleted)
	}
}

func TestRunRemoteStdout(t *testing.T) {
	r := &fakeRunner{stdout: []byte("bmpframes")}
	withRunner(t, r)

	var stdout bytes.Buffer
	args := []string{"-i", "U:/x.mkv", "-f", "image2pipe", "-"}
	handled, _, err := RunRemoteIfNeeded(context.Background(), args, nil, &stdout)
	if !handled || err != nil {
		t.Fatalf("handled=%v err=%v", handled, err)
	}
	if r.workspace != "" {
		t.Errorf("stdout output must not use a workspace, got %q", r.workspace)
	}
	if r.args[len(r.args)-1] != "-" || stdout.String() != "bmpframes" {
		t.Errorf("args=%v stdout=%q", r.args, stdout.String())
	}
}
