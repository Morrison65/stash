package generate

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
}

func assertReplaced(t *testing.T, src, dst, want string) {
	t.Helper()
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Errorf("dst content = %q, want %q", got, want)
	}
	if _, err := os.Stat(src); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("src still exists after move (stat error: %v)", err)
	}
	entries, err := os.ReadDir(filepath.Dir(dst))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Errorf("dst directory has %d entries, want only dst (leftover temporary file?)", len(entries))
	}
}

func TestReplaceFile(t *testing.T) {
	tests := []struct {
		name      string
		dstExists bool
	}{
		{"new file", false},
		{"existing file", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			src := filepath.Join(t.TempDir(), "src.mp4")
			dst := filepath.Join(t.TempDir(), "dst.mp4")
			writeFile(t, src, "new")
			if tt.dstExists {
				writeFile(t, dst, "old")
			}

			if err := replaceFile(src, dst); err != nil {
				t.Fatalf("replaceFile() error = %v", err)
			}
			assertReplaced(t, src, dst, "new")
		})
	}
}

// Previews are generated in the generated volume and moved into a separately
// mounted screenshots directory, where a rename fails with EXDEV.
func TestReplaceFileAcrossMounts(t *testing.T) {
	srcDir, err := os.MkdirTemp("/dev/shm", "generate-test")
	if err != nil {
		t.Skipf("no /dev/shm: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(srcDir) })
	dstDir := t.TempDir()

	probe := filepath.Join(srcDir, "probe")
	writeFile(t, probe, "")
	if err := os.Rename(probe, filepath.Join(dstDir, "probe")); !errors.Is(err, syscall.EXDEV) {
		t.Skipf("/dev/shm and %s are not different mounts (rename error: %v)", dstDir, err)
	}
	_ = os.Remove(probe)

	src := filepath.Join(srcDir, "src.mp4")
	dst := filepath.Join(dstDir, "dst.mp4")
	writeFile(t, src, "new")
	writeFile(t, dst, "old")

	if err := replaceFile(src, dst); err != nil {
		t.Fatalf("replaceFile() error = %v", err)
	}
	assertReplaced(t, src, dst, "new")

	info, err := os.Stat(dst)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0644 {
		t.Errorf("dst permissions = %o, want 644 (the gateway reads generated files as another user)", perm)
	}
}
