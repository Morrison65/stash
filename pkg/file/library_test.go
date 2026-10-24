package file

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

func TestIsWindowsPath(t *testing.T) {
	for p, want := range map[string]bool{
		"U:/Media/a.mp4":   true,
		`U:\Media\a.mp4`:   true,
		"u:/":              true,
		"U:":               true,
		"UNC:/nas/share/x": true,
		"unc:/nas":         true,
		"UNCX/x":           false,
		"//nas/share/x":    true,
		`\\nas\share\x`:    true,
		"/generated/x.jpg": false,
		"relative/x":       false,
		"1:/x":             false,
		"":                 false,
	} {
		if got := IsWindowsPath(p); got != want {
			t.Errorf("IsWindowsPath(%q) = %v, want %v", p, got, want)
		}
	}
}

func TestNormalizeLibraryPath(t *testing.T) {
	for in, want := range map[string]string{
		`u:\Media\Clips\`:           "U:/Media/Clips",
		"U:/Media/./x/../Clips":     "U:/Media/Clips",
		"u:":                        "U:",
		`U:\`:                       "U:",
		`\\LIL_NAS_X\Pool\StashLib`: "UNC:/LIL_NAS_X/Pool/StashLib",
		"//nas/share/x/":            "UNC:/nas/share/x",
		"unc:/nas/share":            "UNC:/nas/share",
		"/data/clips/":              "/data/clips",
	} {
		if got := NormalizeLibraryPath(in); got != want {
			t.Errorf("NormalizeLibraryPath(%q) = %q, want %q", in, got, want)
		}
	}
}

// recordingFS records which backend received calls.
type recordingFS struct {
	OsFS
	name  string
	calls *[]string
}

func (r *recordingFS) Stat(name string) (fs.FileInfo, error) {
	*r.calls = append(*r.calls, r.name+":"+name)
	return nil, fs.ErrNotExist
}

func TestRoutingFS(t *testing.T) {
	var calls []string
	r := &RoutingFS{
		Remote: &recordingFS{name: "remote", calls: &calls},
		Local:  &recordingFS{name: "local", calls: &calls},
	}

	_, _ = r.Stat("U:/Media/a.mp4")
	_, _ = r.Stat("/generated/a.jpg")
	_, _ = r.Stat("UNC:/nas/share/a.mp4")

	want := []string{"remote:U:/Media/a.mp4", "local:/generated/a.jpg", "remote:UNC:/nas/share/a.mp4"}
	if len(calls) != 3 || calls[0] != want[0] || calls[1] != want[1] || calls[2] != want[2] {
		t.Errorf("calls = %v, want %v", calls, want)
	}

	if err := r.Rename("U:/Media/a.mp4", "/generated/a.mp4"); err == nil {
		t.Error("rename across backends must fail")
	}
}

func TestOsFSLibraryOps(t *testing.T) {
	dir := t.TempDir()
	lib := &OsFS{}

	p := filepath.Join(dir, "a.txt")
	if err := os.WriteFile(p, []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}

	trashed, err := lib.MoveToTrash(p, filepath.Join(dir, "trash"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(p); !errors.Is(err, fs.ErrNotExist) {
		t.Error("source still exists after MoveToTrash")
	}
	if data, err := lib.ReadFile(trashed); err != nil || string(data) != "x" {
		t.Errorf("trashed content = %q, %v", data, err)
	}
}
