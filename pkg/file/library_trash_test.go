package file

import (
	"io/fs"
	"reflect"
	"testing"
	"time"
)

type dirInfo struct{ name string }

func (d dirInfo) Name() string       { return d.name }
func (d dirInfo) Size() int64        { return 0 }
func (d dirInfo) Mode() fs.FileMode  { return fs.ModeDir | 0755 }
func (d dirInfo) ModTime() time.Time { return time.Time{} }
func (d dirInfo) IsDir() bool        { return true }
func (d dirInfo) Sys() any           { return nil }

// trashFS knows a set of directories and records MoveToTrash calls.
type trashFS struct {
	OsFS
	dirs  map[string]bool
	moved []string
}

func (t *trashFS) Stat(name string) (fs.FileInfo, error) {
	if t.dirs[name] {
		return dirInfo{name}, nil
	}
	return nil, fs.ErrNotExist
}

func (t *trashFS) MoveToTrash(path string, trashDir string) (string, error) {
	t.moved = append(t.moved, path+" -> "+trashDir)
	return trashDir + "/x", nil
}

func TestLibraryAncestors(t *testing.T) {
	for p, want := range map[string][]string{
		"U:/Media/Clips/a.mp4":         {"U:/Media/Clips", "U:/Media", "U:"},
		"UNC:/nas/share/Library/a.mp4": {"UNC:/nas/share/Library", "UNC:/nas/share"},
		"UNC:/nas/share/a.mp4":         {"UNC:/nas/share"},
	} {
		if got := libraryAncestors(p); !reflect.DeepEqual(got, want) {
			t.Errorf("libraryAncestors(%q) = %v, want %v", p, got, want)
		}
	}
}

func TestRoutingFSMoveToTrashPrefersLibraryTrash(t *testing.T) {
	remote := &trashFS{dirs: map[string]bool{
		"U:/Media/Clips/.stash-trash":         true,
		"UNC:/nas/share/Library/.stash-trash": true,
	}}
	local := &trashFS{dirs: map[string]bool{"/generated/.stash-trash": true}}
	r := &RoutingFS{Remote: remote, Local: local}

	for _, p := range []string{"U:/Media/Clips/Sub/a.mp4", `\\nas\share\Library\b.mp4`, "F:/Other/c.mp4"} {
		if _, err := r.MoveToTrash(p, "C:/Trash"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := r.MoveToTrash("/generated/.x/d.jpg", "/trash"); err != nil {
		t.Fatal(err)
	}

	want := []string{
		"U:/Media/Clips/Sub/a.mp4 -> U:/Media/Clips/.stash-trash",
		`\\nas\share\Library\b.mp4 -> UNC:/nas/share/Library/.stash-trash`,
		"F:/Other/c.mp4 -> C:/Trash",
	}
	if !reflect.DeepEqual(remote.moved, want) {
		t.Errorf("remote moves = %v, want %v", remote.moved, want)
	}
	// Container paths keep the configured trash path (the classic patch handles local mounts).
	if !reflect.DeepEqual(local.moved, []string{"/generated/.x/d.jpg -> /trash"}) {
		t.Errorf("local moves = %v", local.moved)
	}
}
