package api

import (
	"io/fs"
	"path/filepath"
	"strings"

	"github.com/stashapp/stash/pkg/file"
	"golang.org/x/text/collate"
)

type dirLister []fs.DirEntry

func (s dirLister) Len() int {
	return len(s)
}

func (s dirLister) Swap(i, j int) {
	s[j], s[i] = s[i], s[j]
}

func (s dirLister) Bytes(i int) []byte {
	return []byte(s[i].Name())
}

// readLibraryDir lists a directory through the library file system, which
// routes media-library (Windows) paths to stash-fsd and container paths to
// the local OS.
func readLibraryDir(path string) ([]fs.DirEntry, error) {
	f, err := file.Library().Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return f.ReadDir(-1)
}

// listDir will return the contents of a given directory path as a string slice
func listDir(col *collate.Collator, path string) ([]string, error) {
	var dirPaths []string
	dirPath := path

	files, err := readLibraryDir(path)
	if err != nil {
		dirPath = filepath.Dir(path)
		dirFiles, err := readLibraryDir(dirPath)
		if err != nil {
			return dirPaths, err
		}

		// Filter dir contents by last path fragment if the dir isn't an exact match
		base := strings.ToLower(filepath.Base(path))
		if base != "." && base != string(filepath.Separator) {
			for _, file := range dirFiles {
				if strings.HasPrefix(strings.ToLower(file.Name()), base) {
					files = append(files, file)
				}
			}
		} else {
			files = dirFiles
		}
	}

	if col != nil {
		col.Sort(dirLister(files))
	}

	for _, file := range files {
		if !file.IsDir() {
			continue
		}
		dirPaths = append(dirPaths, filepath.Join(dirPath, file.Name()))
	}
	return dirPaths, nil
}
