package file

import (
	"strings"
)

// LibraryTrashDirName is the name of a per-library trash folder. When a folder
// with this name exists in an ancestor of a media library file, a deleted file
// is moved there instead of to the configured trash path: within one drive or
// share the move is a rename on the host instead of a copy to another drive.
// It is the same folder the classic stack's per-drive trash patch uses.
const LibraryTrashDirName = ".stash-trash"

// libraryTrashDir returns the nearest "<ancestor>/.stash-trash" folder of the
// library path p that exists in l, or "" if there is none. Ancestors outside
// the file system's roots fail Stat and are skipped.
func libraryTrashDir(l LibraryFS, p string) string {
	for _, dir := range libraryAncestors(NormalizeLibraryPath(p)) {
		candidate := dir + "/" + LibraryTrashDirName
		if info, err := l.Stat(candidate); err == nil && info.IsDir() {
			return candidate
		}
	}
	return ""
}

// libraryAncestors lists the folders that contain the canonical library path
// p, nearest first, ending with its drive ("U:") or share ("UNC:/server/share").
func libraryAncestors(p string) []string {
	segments := strings.Split(p, "/")
	root := 1 // "U:"
	if strings.EqualFold(segments[0], "UNC:") {
		root = 3 // "UNC:", server, share
	}
	var dirs []string
	for i := len(segments) - 1; i >= root; i-- {
		dirs = append(dirs, strings.Join(segments[:i], "/"))
	}
	return dirs
}
