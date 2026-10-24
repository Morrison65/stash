package main

// stash-libfs: read-only access to media library files for plugin code in
// the API container (used by deploy/python/sitecustomize.py):
//
//	stash-libfs stat <path>   JSON {"dir", "size", "mtime", "mode"}
//	stash-libfs ls <path>     JSON [{"name", "dir", "size", "mtime", "mode"}, ...]
//	stash-libfs cat <path>    the file's content on stdout
//
// Exit code 2: the path does not exist; 1: any other error (on stderr).

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"sort"

	"github.com/stashapp/stash/pkg/file"
	"github.com/stashapp/stash/pkg/remotefs"
)

type libfsEntry struct {
	Name  string  `json:"name,omitempty"`
	Dir   bool    `json:"dir"`
	Size  int64   `json:"size"`
	MTime float64 `json:"mtime"`
	Mode  uint32  `json:"mode"`
}

func entryOf(info fs.FileInfo) libfsEntry {
	mode := uint32(0o100644) // S_IFREG
	if info.IsDir() {
		mode = 0o040755 // S_IFDIR
	}
	return libfsEntry{
		Dir:   info.IsDir(),
		Size:  info.Size(),
		MTime: float64(info.ModTime().UnixNano()) / 1e9,
		Mode:  mode,
	}
}

func libfsMain(args []string) int {
	if len(args) != 2 || (args[0] != "stat" && args[0] != "ls" && args[0] != "cat") {
		fmt.Fprintln(os.Stderr, "usage: stash-libfs stat|ls|cat <library path>")
		return 1
	}
	cmd, path := args[0], file.NormalizeLibraryPath(args[1])

	client, err := dialFsd()
	if err != nil {
		fmt.Fprintf(os.Stderr, "stash-libfs: connecting to stash-fsd: %v\n", err)
		return 1
	}
	defer client.Close()
	lib := remotefs.NewFS(client)

	if err := libfsRun(lib, cmd, path, os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "stash-libfs: %v\n", err)
		if errors.Is(err, fs.ErrNotExist) {
			return 2
		}
		return 1
	}
	return 0
}

func libfsRun(lib *remotefs.FS, cmd, path string, w io.Writer) error {
	switch cmd {
	case "stat":
		info, err := lib.Stat(path)
		if err != nil {
			return err
		}
		return json.NewEncoder(w).Encode(entryOf(info))

	case "ls":
		f, err := lib.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()
		entries, err := f.ReadDir(-1)
		if err != nil {
			return err
		}
		ret := make([]libfsEntry, 0, len(entries))
		for _, e := range entries {
			info, err := e.Info()
			if err != nil {
				continue // vanished meanwhile
			}
			le := entryOf(info)
			le.Name = e.Name()
			ret = append(ret, le)
		}
		sort.Slice(ret, func(a, b int) bool { return ret[a].Name < ret[b].Name })
		return json.NewEncoder(w).Encode(ret)

	default: // cat
		f, err := lib.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()
		info, err := f.Stat()
		if err != nil {
			return err
		}
		if info.IsDir() {
			return fmt.Errorf("%s is a directory", path)
		}
		_, err = io.Copy(w, f)
		return err
	}
}
