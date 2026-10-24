//go:build integration
// +build integration

package manager

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stashapp/stash/internal/manager/config"
	"github.com/stashapp/stash/pkg/file"
	"github.com/stashapp/stash/pkg/models"
	"github.com/stashapp/stash/pkg/renamer"
	"github.com/stashapp/stash/pkg/sqlite"
	"github.com/stashapp/stash/pkg/txn"

	_ "github.com/golang-migrate/migrate/v4/database/sqlite3"
	_ "github.com/golang-migrate/migrate/v4/source/file"

	// necessary to register custom migrations
	_ "github.com/stashapp/stash/pkg/sqlite/migrations"
)

// memFS is an in-memory library file system for the renamer.
type memFS struct {
	file.LibraryFS // other methods are not used

	mu         sync.Mutex
	files      map[string]bool
	dirs       map[string]bool
	failRename map[string]error
}

type memInfo struct {
	name string
	dir  bool
}

func (i memInfo) Name() string       { return filepath.Base(i.name) }
func (i memInfo) Size() int64        { return 0 }
func (i memInfo) Mode() fs.FileMode  { return 0o644 }
func (i memInfo) ModTime() time.Time { return time.Time{} }
func (i memInfo) IsDir() bool        { return i.dir }
func (i memInfo) Sys() any           { return nil }

func (m *memFS) Stat(name string) (fs.FileInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	switch {
	case m.files[name]:
		return memInfo{name, false}, nil
	case m.dirs[name]:
		return memInfo{name, true}, nil
	}
	return nil, &fs.PathError{Op: "stat", Path: name, Err: fs.ErrNotExist}
}

func (m *memFS) MkdirAll(p string, _ fs.FileMode) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for ; strings.Contains(p, "/"); p = p[:strings.LastIndex(p, "/")] {
		m.dirs[p] = true
	}
	return nil
}

func (m *memFS) Rename(oldpath, newpath string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.failRename[oldpath]; err != nil {
		return err
	}
	if !m.files[oldpath] {
		return &fs.PathError{Op: "rename", Path: oldpath, Err: fs.ErrNotExist}
	}
	if m.files[newpath] {
		return &fs.PathError{Op: "rename", Path: newpath, Err: fs.ErrExist}
	}
	if !m.dirs[newpath[:strings.LastIndex(newpath, "/")]] {
		return &fs.PathError{Op: "rename", Path: newpath, Err: fs.ErrNotExist}
	}
	delete(m.files, oldpath)
	m.files[newpath] = true
	return nil
}

func (m *memFS) Remove(p string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for f := range m.files {
		if strings.HasPrefix(f, p+"/") {
			return errors.New("directory not empty")
		}
	}
	for d := range m.dirs {
		if strings.HasPrefix(d, p+"/") {
			return errors.New("directory not empty")
		}
	}
	delete(m.dirs, p)
	delete(m.files, p)
	return nil
}

const integrationRenamerConfig = `
enabled: true
default_template: "{$studio_family }{$performer }$title $resolution{ $tags}"
use_default_template: true
p_tag_templates:
  MOVE_D: "D:/media/videos/$performer"
p_use_default_template: true
p_default_template: "^*/$performer"
p_tag_option:
  MOVE_D: [clean_tag]
associated_extension: [srt, funscript]
filename_splitchar: "_"
field_whitespaceSeperator: "-"
performer_splitchar: "&"
performer_limit: 3
duplicate_suffix: ["", "-001"]
prevent_title_performer: true
prevent_consecutive: true
remove_emptyfolder: true
path_one_performer: true
path_keep_alrperf: true
squeeze_studio_names: true
rating_format: "{}"
tags_splitchar: "&"
tags_whitelist: [Anal]
`

func TestRenameServiceBatch(t *testing.T) {
	ctx := context.Background()

	_ = config.InitializeEmpty()
	db := sqlite.NewDatabase()
	if err := db.Open(filepath.Join(t.TempDir(), "stash.sqlite")); err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	repo := db.Repository()

	mem := &memFS{files: map[string]bool{}, dirs: map[string]bool{}, failRename: map[string]error{}}
	prev := file.Library()
	file.SetLibrary(mem)
	defer file.SetLibrary(prev)

	cfgPath := filepath.Join(t.TempDir(), "renamer.yml")
	if err := os.WriteFile(cfgPath, []byte(integrationRenamerConfig), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := renamer.LoadConfig(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	rn, err := renamer.New(*cfg)
	if err != nil {
		t.Fatal(err)
	}

	type fixture struct {
		sceneID int
		fileID  models.FileID
		path    string
	}
	var moveTag, analTag int
	var moved, taken, locked, unorganized fixture

	now := time.Now()
	err = txn.WithTxn(ctx, repo.TxnManager, func(ctx context.Context) error {
		folder := func(p string, parent *models.Folder) (*models.Folder, error) {
			f := &models.Folder{Path: p, CreatedAt: now, UpdatedAt: now}
			if parent != nil {
				f.ParentFolderID = &parent.ID
			}
			mem.dirs[p] = true
			return f, repo.Folder.Create(ctx, f)
		}
		root, err := folder("U:/Videos", nil)
		if err != nil {
			return err
		}
		inbox, err := folder("U:/Videos/inbox", root)
		if err != nil {
			return err
		}
		if _, err := folder("D:/media", nil); err != nil {
			return err
		}
		janeDir, err := folder("U:/Videos/inbox/Jane-Doe", inbox)
		if err != nil {
			return err
		}

		videoFile := func(dir *models.Folder, name string) (*models.VideoFile, error) {
			f := &models.VideoFile{
				BaseFile: &models.BaseFile{Basename: name, ParentFolderID: dir.ID, CreatedAt: now, UpdatedAt: now},
				Format:   "mp4", Width: 1920, Height: 1080, VideoCodec: "h264", AudioCodec: "aac", Duration: 60,
			}
			mem.files[dir.Path+"/"+name] = true
			return f, repo.File.Create(ctx, f)
		}

		studio := &models.CreateStudioInput{Studio: &models.Studio{Name: "Big Studio", CreatedAt: now, UpdatedAt: now}}
		if err := repo.Studio.Create(ctx, studio); err != nil {
			return err
		}
		performer := &models.CreatePerformerInput{Performer: &models.Performer{Name: "Jane Doe", CreatedAt: now, UpdatedAt: now}}
		if err := repo.Performer.Create(ctx, performer); err != nil {
			return err
		}
		tag := func(name string) (int, error) {
			t := &models.CreateTagInput{Tag: &models.Tag{Name: name, CreatedAt: now, UpdatedAt: now}}
			return t.ID, repo.Tag.Create(ctx, t)
		}
		if analTag, err = tag("Anal"); err != nil {
			return err
		}
		if moveTag, err = tag("MOVE_D"); err != nil {
			return err
		}

		scene := func(title string, organized bool, f *models.VideoFile, tagIDs ...int) (fixture, error) {
			s := &models.Scene{
				Title: title, Organized: organized, StudioID: &studio.ID,
				TagIDs:       models.NewRelatedIDs(tagIDs),
				PerformerIDs: models.NewRelatedIDs([]int{performer.ID}),
				CreatedAt:    now, UpdatedAt: now,
			}
			err := repo.Scene.Create(ctx, s, []models.FileID{f.ID})
			return fixture{s.ID, f.ID, f.Path}, err
		}

		// moved to another drive by its move tag, with a caption
		f, err := videoFile(inbox, "old.mp4")
		if err != nil {
			return err
		}
		f.Path = "U:/Videos/inbox/old.mp4"
		if moved, err = scene("Title", true, f, analTag, moveTag); err != nil {
			return err
		}
		if err := repo.File.UpdateCaptions(ctx, f.ID, []*models.VideoCaption{{LanguageCode: "en", Filename: "old.en.srt", CaptionType: "srt"}}); err != nil {
			return err
		}
		mem.files["U:/Videos/inbox/old.en.srt"] = true
		mem.files["U:/Videos/inbox/old.funscript"] = true

		// its target is taken by another file
		f, err = videoFile(inbox, "two.mp4")
		if err != nil {
			return err
		}
		f.Path = "U:/Videos/inbox/two.mp4"
		if taken, err = scene("Taken", true, f, analTag); err != nil {
			return err
		}
		if _, err := videoFile(janeDir, "BigStudio_Jane-Doe_Taken_HD_Anal.mp4"); err != nil {
			return err
		}

		// in use
		f, err = videoFile(inbox, "locked.mp4")
		if err != nil {
			return err
		}
		f.Path = "U:/Videos/inbox/locked.mp4"
		if locked, err = scene("Locked", true, f); err != nil {
			return err
		}
		mem.failRename[f.Path] = errors.New("the file is used by another process")

		// not organized
		f, err = videoFile(inbox, "raw.mp4")
		if err != nil {
			return err
		}
		f.Path = "U:/Videos/inbox/raw.mp4"
		unorganized, err = scene("Raw", false, f)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}

	rs := &RenameService{
		renamer: rn,
		repo:    repo,
		roots:   func() []string { return []string{"U:/Videos", "D:/media"} },
		busy:    func() bool { return false },
		pending: map[int]struct{}{},
		wake:    make(chan struct{}, 1),
		done:    make(chan struct{}),
	}
	rs.processBatch(ctx, []int{moved.sceneID, taken.sceneID, locked.sceneID, unorganized.sceneID})

	const target = "D:/media/videos/Jane-Doe/BigStudio_Jane-Doe_Title_HD_Anal.mp4"
	for p, want := range map[string]bool{
		target: true, "U:/Videos/inbox/old.mp4": false,
		"D:/media/videos/Jane-Doe/BigStudio_Jane-Doe_Title_HD_Anal.en.srt":    true,
		"D:/media/videos/Jane-Doe/BigStudio_Jane-Doe_Title_HD_Anal.funscript": true,
		"U:/Videos/inbox/two.mp4": true, "U:/Videos/inbox/locked.mp4": true, "U:/Videos/inbox/raw.mp4": true,
	} {
		if mem.files[p] != want {
			t.Errorf("file system: %s exists = %v, want %v", p, mem.files[p], want)
		}
	}
	if !mem.dirs["U:/Videos/inbox"] {
		t.Error("non-empty folder removed")
	}

	err = txn.WithReadTxn(ctx, repo.TxnManager, func(ctx context.Context) error {
		path := func(id models.FileID) string {
			fs, err := repo.File.Find(ctx, id)
			if err != nil || len(fs) == 0 {
				t.Fatalf("file %d: %v", id, err)
			}
			return fs[0].Base().Path
		}
		if got := path(moved.fileID); got != target {
			t.Errorf("moved file in the database: %s", got)
		}
		for _, f := range []fixture{taken, locked, unorganized} {
			if got := path(f.fileID); got != f.path {
				t.Errorf("file %d changed to %s", f.fileID, got)
			}
		}

		folder, err := repo.Folder.FindByPath(ctx, "D:/media/videos/Jane-Doe", true)
		if err != nil || folder == nil || folder.ParentFolderID == nil {
			t.Fatalf("target folder: %+v %v", folder, err)
		}
		parent, err := repo.Folder.Find(ctx, *folder.ParentFolderID)
		if err != nil || parent == nil || parent.Path != "D:/media/videos" {
			t.Errorf("target folder parent: %+v %v", parent, err)
		}

		captions, err := repo.File.GetCaptions(ctx, moved.fileID)
		if err != nil || len(captions) != 1 || captions[0].Filename != "BigStudio_Jane-Doe_Title_HD_Anal.en.srt" {
			t.Errorf("captions: %+v %v", captions, err)
		}

		s, err := repo.Scene.Find(ctx, moved.sceneID)
		if err != nil {
			return err
		}
		if err := s.LoadTagIDs(ctx, repo.Scene); err != nil {
			return err
		}
		if slices.Contains(s.TagIDs.List(), moveTag) || !slices.Contains(s.TagIDs.List(), analTag) {
			t.Errorf("tags after the move: %v (move tag %d)", s.TagIDs.List(), moveTag)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	// a second run finds everything in place
	before := len(mem.files)
	rs.processBatch(ctx, []int{moved.sceneID})
	if len(mem.files) != before || !mem.files[target] {
		t.Error("second run changed files")
	}
}

func TestRenameServiceNotifyFilter(t *testing.T) {
	rs := &RenameService{pending: map[int]struct{}{}, wake: make(chan struct{}, 1)}
	rs.notify(1, "Scene.Update.Post", []string{"id", "custom_fields"})
	rs.notify(2, "Scene.Update.Post", []string{"id", "title"})
	rs.notify(3, "Scene.Update.Post", nil)
	rs.notify(4, "Performer.Update.Post", []string{"name"})
	if got := rs.take(10); !slices.Equal(got, []int{2, 3}) {
		t.Errorf("queued %v, want [2 3]", got)
	}
	if libraryVolume("UNC:/nas/share/a/b.mp4") != "unc:/nas/share" || libraryVolume("U:/a/b") != "u:" {
		t.Error("libraryVolume")
	}
}
