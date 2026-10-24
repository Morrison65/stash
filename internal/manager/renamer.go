package manager

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/stashapp/stash/pkg/file"
	"github.com/stashapp/stash/pkg/job"
	"github.com/stashapp/stash/pkg/logger"
	"github.com/stashapp/stash/pkg/models"
	"github.com/stashapp/stash/pkg/plugin/hook"
	"github.com/stashapp/stash/pkg/renamer"
	"github.com/stashapp/stash/pkg/txn"
)

// envRenamerConfig overrides the location of renamer.yml (default: next to
// the config file).
const envRenamerConfig = "STASH_RENAMER_CONFIG"

const (
	// renameDebounce collects the updates of a bulk edit into one batch
	renameDebounce = 2 * time.Second
	// renameBatchSize is the most scenes computed in one read transaction
	renameBatchSize = 200
	// renameCommitSize is the most renames recorded in one write transaction
	renameCommitSize = 50
	// renameBusyPoll is how often a waiting batch checks for scan/clean
	renameBusyPoll = 10 * time.Second
)

// renameIgnoredFields are scene update fields that do not affect file
// names. An update of only these fields is not checked (e.g. the custom
// fields written by CountForPerformers, or play counts).
var renameIgnoredFields = map[string]bool{
	"id": true, "ids": true, "clientMutationId": true, "client_mutation_id": true,
	"details": true, "director": true, "url": true, "urls": true,
	"o_counter": true, "gallery_ids": true, "cover_image": true,
	"resume_time": true, "play_duration": true, "play_count": true,
	"custom_fields": true,
}

// RenameService renames and moves the files of updated scenes to where the
// templates of renamer.yml say they belong (pkg/renamer). It replaces the
// renamerOnUpdate plugin and never blocks the update that triggered it:
//
//   - a scene update only records the scene id;
//   - a background worker collects the updates of a few seconds, computes the
//     targets of the whole batch in one read transaction, lets stash-fsd
//     rename the files one after the other (renames on the same drive first,
//     copies to another drive after them), and records the successful moves
//     in one write transaction per batch;
//   - a file that cannot be renamed (in use, target exists) is logged and
//     left alone. The next update of the scene tries again.
//
// While a scan or clean job runs, batches wait: a scan walking a folder
// between the rename and the database update would see the file as new.
type RenameService struct {
	renamer *renamer.Renamer
	repo    models.Repository
	roots   func() []string
	busy    func() bool

	mu      sync.Mutex
	pending map[int]struct{}
	wake    chan struct{}

	cancel context.CancelFunc
	done   chan struct{}
}

type renameItem struct {
	sceneID     int
	fileID      models.FileID
	index       int
	current     string
	target      string
	dryRun      bool
	cleanTagIDs []int
	captions    []*models.VideoCaption

	// set by move: caption file names that were renamed (old -> new)
	captionRenames map[string]string
}

func (it *renameItem) crossVolume() bool {
	return libraryVolume(it.current) != libraryVolume(it.target)
}

// libraryVolume is the drive (U:) or share (UNC:/server/share) of a
// canonical library path.
func libraryVolume(p string) string {
	parts := strings.SplitN(p, "/", 4)
	if parts[0] == strings.TrimSuffix(file.UNCPrefix, "/") && len(parts) >= 3 {
		return strings.ToLower(strings.Join(parts[:3], "/"))
	}
	return strings.ToLower(parts[0])
}

// startRenameService starts the renamer when renamer.yml enables it.
func (s *Manager) startRenameService() {
	if s.RenameService != nil {
		return
	}

	cfgPath := os.Getenv(envRenamerConfig)
	if cfgPath == "" {
		cfgPath = filepath.Join(filepath.Dir(s.Config.GetConfigFile()), "renamer.yml")
	}
	c, err := renamer.LoadConfig(cfgPath)
	if errors.Is(err, fs.ErrNotExist) {
		logger.Debugf("[renamer] %s not found: files are not renamed on update", cfgPath)
		return
	}
	if err != nil {
		logger.Errorf("[renamer] %v: files are not renamed on update", err)
		return
	}
	if !c.Enabled {
		logger.Infof("[renamer] disabled in %s", cfgPath)
		return
	}
	r, err := renamer.New(*c)
	if err != nil {
		logger.Errorf("[renamer] %s: %v: files are not renamed on update", cfgPath, err)
		return
	}

	rs := &RenameService{
		renamer: r,
		repo:    s.Repository,
		roots:   func() []string { return s.Config.GetStashPaths().Paths() },
		busy:    func() bool { return scanOrCleanRunning(s.JobManager) },
		pending: make(map[int]struct{}),
		wake:    make(chan struct{}, 1),
		done:    make(chan struct{}),
	}
	ctx, cancel := context.WithCancel(context.Background())
	rs.cancel = cancel
	go rs.run(ctx)

	s.PluginCache.AddPostHookListener(rs.notify)
	s.RenameService = rs

	mode := ""
	if c.DryRun {
		mode = " (dry run)"
	}
	logger.Infof("[renamer] renaming files of updated organized scenes with a title%s, config %s", mode, cfgPath)
}

// scanOrCleanRunning reports whether a scan or clean job is running.
func scanOrCleanRunning(m *job.Manager) bool {
	for _, j := range m.GetQueue() {
		if j.Status == job.StatusRunning && (j.Description == "Scanning..." || j.Description == "Cleaning...") {
			return true
		}
	}
	return false
}

// notify records a scene update. It runs inside the update's request.
func (s *RenameService) notify(id int, hookType hook.TriggerEnum, inputFields []string) {
	if hookType != hook.SceneUpdatePost {
		return
	}
	if len(inputFields) > 0 {
		relevant := false
		for _, f := range inputFields {
			if !renameIgnoredFields[f] {
				relevant = true
				break
			}
		}
		if !relevant {
			return
		}
	}

	s.mu.Lock()
	s.pending[id] = struct{}{}
	s.mu.Unlock()

	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// Stop ends the worker after the file it is renaming, waiting at most
// timeout. Moves already done are recorded first.
func (s *RenameService) Stop(timeout time.Duration) {
	s.cancel()
	select {
	case <-s.done:
	case <-time.After(timeout):
		logger.Warn("[renamer] shutting down while a file is being moved; the next update of its scene records it")
	}
}

func (s *RenameService) take(n int) []int {
	s.mu.Lock()
	defer s.mu.Unlock()
	var ids []int
	for id := range s.pending {
		ids = append(ids, id)
	}
	sort.Ints(ids)
	if len(ids) > n {
		ids = ids[:n]
	}
	for _, id := range ids {
		delete(s.pending, id)
	}
	return ids
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

func (s *RenameService) run(ctx context.Context) {
	defer close(s.done)
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.wake:
		}
		if !sleepCtx(ctx, renameDebounce) {
			return
		}

		waiting := false
		for {
			if s.busy() {
				if !waiting {
					logger.Info("[renamer] waiting for the running scan/clean job")
					waiting = true
				}
				if !sleepCtx(ctx, renameBusyPoll) {
					return
				}
				continue
			}
			ids := s.take(renameBatchSize)
			if len(ids) == 0 {
				break
			}
			s.processBatch(ctx, ids)
			if ctx.Err() != nil {
				return
			}
		}
	}
}

func (s *RenameService) processBatch(ctx context.Context, ids []int) {
	items := s.plan(ctx, ids)
	if len(items) == 0 {
		return
	}

	// renames on the same drive are instant: do them before the copies
	sort.SliceStable(items, func(a, b int) bool {
		return !items[a].crossVolume() && items[b].crossVolume()
	})

	var done []*renameItem
	for _, it := range items {
		if ctx.Err() != nil {
			break
		}
		if !s.move(it) {
			continue
		}
		done = append(done, it)
		// record a copy to another drive at once: it took long already
		if it.crossVolume() || len(done) >= renameCommitSize {
			s.commit(ctx, done)
			done = nil
		}
	}
	s.commit(ctx, done)
}

// plan computes the targets of the scenes in one read transaction and
// returns the files to rename.
func (s *RenameService) plan(ctx context.Context, ids []int) []*renameItem {
	var items []*renameItem
	cfg := s.renamer.Config()
	roots := s.roots()

	err := txn.WithReadTxn(ctx, s.repo.TxnManager, func(ctx context.Context) error {
		l := newRenameLoader(s.repo)
		planned := map[string]bool{}
		for _, id := range ids {
			sc, refs, err := l.scene(ctx, id)
			if err != nil {
				return err
			}
			if sc == nil {
				continue // deleted meanwhile
			}

			res := s.renamer.Compute(*sc, renamer.Options{})
			if res.Skip != "" {
				continue
			}
			if res.Err != nil {
				logger.Errorf("[renamer] scene %d: %v; not renamed", id, res.Err)
				continue
			}
			for _, f := range res.Files {
				if f.Err != nil {
					logger.Errorf("[renamer] scene %d, %s: %v; not renamed", id, f.Current, f.Err)
					continue
				}
				if !f.Changed() {
					continue
				}
				ref := refs[f.Index]
				if ref.inZip {
					logger.Warnf("[renamer] scene %d: %s is inside a zip file; not renamed", id, f.Current)
					continue
				}
				if !withinRoots(f.Target, roots) {
					logger.Errorf("[renamer] scene %d: %s is not inside a library folder; not renamed", id, f.Target)
					continue
				}
				other, err := s.repo.File.FindByPath(ctx, f.Target, true)
				if err != nil {
					return err
				}
				if other != nil && other.Base().ID != ref.id {
					if s.renamer.HasDuplicateSuffix(f.Current, f.Target, f.Index) {
						continue // named with a suffix because the target was taken
					}
					logger.Errorf("[renamer] scene %d: %s already exists in the library; not renamed", id, f.Target)
					continue
				}
				key := strings.ToLower(f.Target)
				if planned[key] {
					logger.Errorf("[renamer] scene %d: another scene of this batch is renamed to %s; not renamed", id, f.Target)
					continue
				}
				planned[key] = true

				items = append(items, &renameItem{
					sceneID:     id,
					fileID:      ref.id,
					index:       f.Index,
					current:     f.Current,
					target:      f.Target,
					dryRun:      f.DryRun || cfg.DryRun,
					cleanTagIDs: res.CleanTagIDs,
					captions:    ref.captions,
				})
			}
		}
		return nil
	})
	if err != nil {
		logger.Errorf("[renamer] reading %d scenes: %v; they are checked again on their next update", len(ids), err)
		return nil
	}
	return items
}

func withinRoots(p string, roots []string) bool {
	for _, r := range roots {
		if strings.HasPrefix(p, strings.TrimSuffix(r, "/")+"/") {
			return true
		}
	}
	return false
}

// move renames the file (and its associated files) through the library file
// system, i.e. stash-fsd for library paths. It reports whether the database
// has to be updated.
func (s *RenameService) move(it *renameItem) bool {
	lib := file.Library()
	if it.dryRun {
		logger.Infof("[renamer] dry run: scene %d: %s -> %s", it.sceneID, it.current, it.target)
		return false
	}

	if _, err := lib.Stat(it.current); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			if _, terr := lib.Stat(it.target); terr == nil {
				// an earlier move that was not recorded (e.g. restart)
				logger.Infof("[renamer] scene %d: %s is already at %s; recording it", it.sceneID, it.current, it.target)
				return true
			}
		}
		logger.Warnf("[renamer] scene %d: %s: %v; not renamed", it.sceneID, it.current, err)
		return false
	}

	newDir, oldDir := path.Dir(it.target), path.Dir(it.current)
	if err := lib.MkdirAll(newDir, 0755); err != nil {
		logger.Warnf("[renamer] scene %d: creating %s: %v; not renamed", it.sceneID, newDir, err)
		return false
	}
	start := time.Now()
	if err := lib.Rename(it.current, it.target); err != nil {
		logger.Warnf("[renamer] scene %d: could not rename %s to %s: %v", it.sceneID, it.current, it.target, err)
		if newDir != oldDir && !withinRootsExact(newDir, s.roots()) {
			_ = lib.Remove(newDir) // only if it was created for this file and is empty
		}
		return false
	}
	logger.Infof("[renamer] scene %d: %s -> %s (%s)", it.sceneID, it.current, it.target, time.Since(start).Round(time.Millisecond))

	s.moveAssociated(lib, it)

	if newDir != oldDir && s.renamer.Config().RemoveEmptyFolder && !withinRootsExact(oldDir, s.roots()) {
		// fails unless the folder is empty
		_ = lib.Remove(oldDir)
	}
	return true
}

func withinRootsExact(p string, roots []string) bool {
	for _, r := range roots {
		if strings.TrimSuffix(r, "/") == p {
			return true
		}
	}
	return false
}

// moveAssociated renames the files that belong to the video: those with the
// configured extensions (video.srt, video.funscript) and the captions Stash
// knows (video.en.srt).
func (s *RenameService) moveAssociated(lib file.LibraryFS, it *renameItem) {
	oldStem := strings.TrimSuffix(it.current, path.Ext(it.current))
	newStem := strings.TrimSuffix(it.target, path.Ext(it.target))
	oldDir, newDir := path.Dir(it.current), path.Dir(it.target)

	moved := map[string]bool{}
	rename := func(from, to string) bool {
		if moved[from] {
			return true
		}
		if err := lib.Rename(from, to); err != nil {
			if !errors.Is(err, fs.ErrNotExist) {
				logger.Warnf("[renamer] scene %d: could not rename %s to %s: %v", it.sceneID, from, to, err)
			}
			return false
		}
		moved[from] = true
		return true
	}

	for _, ext := range s.renamer.Config().AssociatedExtension {
		rename(oldStem+"."+ext, newStem+"."+ext)
	}

	oldBase, newBase := path.Base(oldStem), path.Base(newStem)
	for _, c := range it.captions {
		rest, ok := strings.CutPrefix(c.Filename, oldBase+".")
		if !ok {
			continue
		}
		newName := newBase + "." + rest
		if rename(oldDir+"/"+c.Filename, newDir+"/"+newName) {
			if it.captionRenames == nil {
				it.captionRenames = map[string]string{}
			}
			it.captionRenames[c.Filename] = newName
		}
	}
}

// commit records the renamed files in one write transaction. If that fails
// the files are moved back.
func (s *RenameService) commit(ctx context.Context, done []*renameItem) {
	if len(done) == 0 {
		return
	}
	// moved files must be recorded even when shutting down
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
	defer cancel()

	roots := s.roots()
	err := txn.WithTxn(ctx, s.repo.TxnManager, func(ctx context.Context) error {
		cleaned := map[int]bool{}
		for _, it := range done {
			folder, err := file.GetOrCreateFolderHierarchy(ctx, s.repo.Folder, path.Dir(it.target), roots)
			if err != nil {
				return err
			}
			if folder == nil {
				return fmt.Errorf("no folder for %s", it.target)
			}
			files, err := s.repo.File.Find(ctx, it.fileID)
			if err != nil {
				return err
			}
			if len(files) == 0 {
				return fmt.Errorf("file %d of scene %d not found", it.fileID, it.sceneID)
			}
			f := files[0]
			b := f.Base()
			b.ParentFolderID = folder.ID
			b.Basename = path.Base(it.target)
			b.UpdatedAt = time.Now()
			if err := s.repo.File.Update(ctx, f); err != nil {
				return err
			}

			if len(it.captionRenames) > 0 {
				for _, c := range it.captions {
					if n, ok := it.captionRenames[c.Filename]; ok {
						c.Filename = n
					}
				}
				if err := s.repo.File.UpdateCaptions(ctx, it.fileID, it.captions); err != nil {
					return err
				}
			}

			if len(it.cleanTagIDs) > 0 && !cleaned[it.sceneID] {
				cleaned[it.sceneID] = true
				p := models.NewScenePartial()
				p.TagIDs = &models.UpdateIDs{IDs: it.cleanTagIDs, Mode: models.RelationshipUpdateModeRemove}
				if _, err := s.repo.Scene.UpdatePartial(ctx, it.sceneID, p); err != nil {
					return err
				}
			}
		}
		return nil
	})
	if err == nil {
		return
	}

	logger.Errorf("[renamer] recording %d renamed files: %v; moving them back", len(done), err)
	lib := file.Library()
	for _, it := range done {
		if rerr := lib.Rename(it.target, it.current); rerr != nil {
			logger.Errorf("[renamer] scene %d: could not move %s back to %s: %v", it.sceneID, it.target, it.current, rerr)
		}
	}
}

// renameLoader loads scenes for the renamer, caching shared objects for one
// batch.
type renameLoader struct {
	repo       models.Repository
	tags       map[int]*renamer.Tag
	performers map[int]*renamer.Performer
	studios    map[int]*renamer.Studio
	groups     map[int]*models.Group
}

type renameFileRef struct {
	id       models.FileID
	inZip    bool
	captions []*models.VideoCaption
}

func newRenameLoader(repo models.Repository) *renameLoader {
	return &renameLoader{
		repo:       repo,
		tags:       map[int]*renamer.Tag{},
		performers: map[int]*renamer.Performer{},
		studios:    map[int]*renamer.Studio{},
		groups:     map[int]*models.Group{},
	}
}

func (l *renameLoader) scene(ctx context.Context, id int) (*renamer.Scene, []renameFileRef, error) {
	r := l.repo
	sc, err := r.Scene.Find(ctx, id)
	if err != nil || sc == nil {
		return nil, nil, err
	}
	// cheap checks first: most updated scenes are not renamed
	if !sc.Organized || sc.Title == "" {
		return &renamer.Scene{ID: id, Organized: sc.Organized, Title: sc.Title}, nil, nil
	}

	if err := sc.LoadFiles(ctx, r.Scene); err != nil {
		return nil, nil, err
	}
	if err := sc.LoadTagIDs(ctx, r.Scene); err != nil {
		return nil, nil, err
	}
	if err := sc.LoadPerformerIDs(ctx, r.Scene); err != nil {
		return nil, nil, err
	}
	if err := sc.LoadStashIDs(ctx, r.Scene); err != nil {
		return nil, nil, err
	}
	if err := sc.LoadGroups(ctx, r.Scene); err != nil {
		return nil, nil, err
	}

	ret := &renamer.Scene{ID: id, Title: sc.Title, Organized: sc.Organized, Code: sc.Code}
	if sc.Date != nil {
		ret.Date = sc.Date.String()
	}
	if sc.Rating != nil {
		ret.Rating100 = *sc.Rating
	}
	for _, sid := range sc.StashIDs.List() {
		ret.StashIDs = append(ret.StashIDs, sid.StashID)
	}

	var refs []renameFileRef
	for _, f := range sc.Files.List() {
		ret.Files = append(ret.Files, renamer.File{
			Path:       f.Path,
			VideoCodec: f.VideoCodec,
			AudioCodec: f.AudioCodec,
			Width:      f.Width,
			Height:     f.Height,
			Duration:   f.Duration,
			BitRate:    f.BitRate,
			Oshash:     f.Fingerprints.GetString(models.FingerprintTypeOshash),
			Checksum:   f.Fingerprints.GetString(models.FingerprintTypeMD5),
		})
		captions, err := r.File.GetCaptions(ctx, f.ID)
		if err != nil {
			return nil, nil, err
		}
		refs = append(refs, renameFileRef{id: f.ID, inZip: f.ZipFileID != nil, captions: captions})
	}

	if sc.StudioID != nil {
		if ret.Studio, err = l.studio(ctx, *sc.StudioID, 0); err != nil {
			return nil, nil, err
		}
	}
	for _, tid := range sc.TagIDs.List() {
		t, err := l.tag(ctx, tid)
		if err != nil {
			return nil, nil, err
		}
		if t != nil {
			ret.Tags = append(ret.Tags, *t)
		}
	}
	for _, pid := range sc.PerformerIDs.List() {
		p, err := l.performer(ctx, pid)
		if err != nil {
			return nil, nil, err
		}
		if p != nil {
			ret.Performers = append(ret.Performers, *p)
		}
	}
	for _, g := range sc.Groups.List() {
		grp, ok := l.groups[g.GroupID]
		if !ok {
			if grp, err = r.Group.Find(ctx, g.GroupID); err != nil {
				return nil, nil, err
			}
			l.groups[g.GroupID] = grp
		}
		if grp == nil {
			continue
		}
		rg := renamer.Group{Name: grp.Name}
		if grp.Date != nil {
			rg.Date = grp.Date.String()
		}
		if g.SceneIndex != nil {
			rg.SceneIndex = *g.SceneIndex
		}
		ret.Groups = append(ret.Groups, rg)
	}
	return ret, refs, nil
}

func (l *renameLoader) studio(ctx context.Context, id int, depth int) (*renamer.Studio, error) {
	if s, ok := l.studios[id]; ok {
		return s, nil
	}
	if depth > 50 {
		return nil, fmt.Errorf("studio %d: parent loop", id)
	}
	st, err := l.repo.Studio.Find(ctx, id)
	if err != nil || st == nil {
		return nil, err
	}
	ret := &renamer.Studio{Name: st.Name}
	if st.ParentID != nil {
		if ret.Parent, err = l.studio(ctx, *st.ParentID, depth+1); err != nil {
			return nil, err
		}
	}
	l.studios[id] = ret
	return ret, nil
}

func (l *renameLoader) tag(ctx context.Context, id int) (*renamer.Tag, error) {
	if t, ok := l.tags[id]; ok {
		return t, nil
	}
	t, err := l.repo.Tag.Find(ctx, id)
	if err != nil || t == nil {
		return nil, err
	}
	if err := t.LoadAliases(ctx, l.repo.Tag); err != nil {
		return nil, err
	}
	ret := &renamer.Tag{ID: t.ID, Name: t.Name, SortName: t.SortName, Aliases: t.Aliases.List()}
	l.tags[id] = ret
	return ret, nil
}

func (l *renameLoader) performer(ctx context.Context, id int) (*renamer.Performer, error) {
	if p, ok := l.performers[id]; ok {
		return p, nil
	}
	p, err := l.repo.Performer.Find(ctx, id)
	if err != nil || p == nil {
		return nil, err
	}
	if err := p.LoadStashIDs(ctx, l.repo.Performer); err != nil {
		return nil, err
	}
	ret := &renamer.Performer{Name: p.Name, Favorite: p.Favorite}
	if p.Gender != nil {
		ret.Gender = string(*p.Gender)
	}
	if p.Rating != nil {
		ret.Rating100 = *p.Rating
	}
	for _, sid := range p.StashIDs.List() {
		ret.StashIDs = append(ret.StashIDs, sid.StashID)
	}
	l.performers[id] = ret
	return ret, nil
}
