package renamer

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

var epoch = time.Unix(0, 0).UTC()

// maxPathLength is the plugin's limit for the full path (Windows MAX_PATH
// minus a margin).
const maxPathLength = 240

// Scene is the data of a scene that the templates can use.
type Scene struct {
	ID        int
	Title     string
	Date      string // YYYY-MM-DD, empty when unset
	Rating100 int    // 0 when unset
	Organized bool
	Code      string
	StashIDs  []string // stash_id values
	// Files in Stash's order: the primary file first
	Files      []File
	Studio     *Studio
	Tags       []Tag
	Performers []Performer
	Groups     []Group
}

// File is a video file of a scene.
type File struct {
	Path       string // canonical library path
	VideoCodec string
	AudioCodec string
	Width      int
	Height     int
	Duration   float64
	BitRate    int64
	Oshash     string
	Checksum   string
}

// Studio is a studio with its parents.
type Studio struct {
	Name   string
	Parent *Studio
}

// Tag is a scene tag.
type Tag struct {
	ID       int
	Name     string
	SortName string
	Aliases  []string
}

// Performer is a scene performer.
type Performer struct {
	Name      string
	Gender    string // GraphQL enum value (FEMALE, ...), empty when unset
	Favorite  bool
	Rating100 int
	StashIDs  []string
}

// Group is a group (movie) the scene belongs to.
type Group struct {
	Name       string
	Date       string
	SceneIndex int
}

// Options change details of the computation.
type Options struct {
	// SortTags orders matching tags alphabetically. Otherwise the order of
	// the current file name is kept when it has the same tags, and new names
	// use the order of tags_whitelist. (The plugin used Python's set order,
	// which changes between runs.)
	SortTags bool
	// LegacyNesting compares folder names before removing characters, as the
	// plugin did. Then a performer folder whose name loses a character (e.g.
	// an apostrophe) is created again inside itself on every update.
	LegacyNesting bool
}

// FileResult is the computed location of one file of a scene.
type FileResult struct {
	Index   int
	Current string
	// Target is the computed path; equal to Current when the file is where
	// it belongs. Empty when Err is set.
	Target string
	// DryRun is set when a dry_run path option applies to this file.
	DryRun bool
	Err    error
}

// Changed reports whether the file has to be renamed or moved.
func (f FileResult) Changed() bool {
	return f.Err == nil && f.Target != f.Current
}

// SceneResult is the computed location of all files of a scene.
type SceneResult struct {
	SceneID int
	// Skip is the reason the scene is not handled at all (not organized,
	// no title, no template). Files is empty then.
	Skip string
	// Err is a scene-level error (several move tags). Files is empty then.
	Err   error
	Files []FileResult
	// CleanTagIDs are tags to remove from the scene once one of its files
	// was moved (p_tag_option clean_tag).
	CleanTagIDs []int
}

// ErrAmbiguousMoveTags is returned when a scene has several tags with a
// folder template.
var ErrAmbiguousMoveTags = errors.New("several move tags")

type pathTemplate struct {
	destination string
	options     []string
	cleanTagIDs []int
}

type templates struct {
	filename string
	path     *pathTemplate
}

// sceneState is the plugin's per-scene mutable state: it appends the scene's
// performers to the configured $title replacers on every use.
type sceneState struct {
	replacers  map[string][]Replacer
	performers []Performer
}

func (s *sceneState) clone() *sceneState {
	c := &sceneState{replacers: make(map[string][]Replacer, len(s.replacers)), performers: slices.Clone(s.performers)}
	for k, v := range s.replacers {
		c.replacers[k] = slices.Clone(v)
	}
	return c
}

// info is the plugin's scene_information for one file.
type info struct {
	str              map[string]string
	performerList    []string
	hasPerformerList bool
	studioHierarchy  []string
	hasHierarchy     bool
	currentPathSplit []string
	templateSplit    []string
}

func (in *info) truthy(name string) bool {
	switch name {
	case "performer_list":
		return len(in.performerList) > 0
	case "studio_hierarchy":
		return len(in.studioHierarchy) > 0
	}
	return in.str[name] != ""
}

func (in *info) remove(name string) {
	switch name {
	case "performer_list":
		in.performerList, in.hasPerformerList = nil, false
	case "studio_hierarchy":
		in.studioHierarchy, in.hasHierarchy = nil, false
	}
	delete(in.str, name)
}

// Compute returns where the files of s belong.
func (r *Renamer) Compute(s Scene, opts Options) SceneResult {
	res := SceneResult{SceneID: s.ID}

	// only organized scenes with a title are renamed
	if !s.Organized {
		res.Skip = "not organized"
		return res
	}
	if s.Title == "" {
		res.Skip = "no title"
		return res
	}

	st := &sceneState{replacers: make(map[string][]Replacer, len(r.fieldReplacers)), performers: slices.Clone(s.Performers)}
	for k, v := range r.fieldReplacers {
		st.replacers[k] = slices.Clone(v)
	}

	tags := r.filterTags(s.Tags)

	for i, f := range s.Files {
		tmpl, err := r.templates(s, f)
		if err != nil {
			res.Err = err
			res.Files = nil
			return res
		}
		if tmpl.filename == "" && tmpl.path == nil {
			res.Skip = "no template"
			res.Files = nil
			return res
		}
		if tmpl.path != nil {
			for _, id := range tmpl.path.cleanTagIDs {
				if !slices.Contains(res.CleanTagIDs, id) {
					res.CleanTagIDs = append(res.CleanTagIDs, id)
				}
			}
		}

		ordered := r.orderTags(tags, opts)
		before := st.clone()
		fr := r.computeFile(s, st, i, f, tmpl, ordered, opts)

		// keep the tag order of the current name instead of renaming the
		// file only to reorder its tags
		if !opts.SortTags && fr.Changed() && len(ordered) > 1 && len(ordered) <= 6 {
			permute(ordered, func(p []string) bool {
				try := before.clone()
				alt := r.computeFile(s, try, i, f, tmpl, p, opts)
				if alt.Err == nil && alt.Target == alt.Current {
					fr, st = alt, try
					return true
				}
				return false
			})
		}

		res.Files = append(res.Files, fr)
	}
	return res
}

// HasDuplicateSuffix reports whether current is target with a later
// duplicate suffix (target-001.mp4): the name the plugin gave a file when its
// target was taken. index is the file's index in the scene.
func (r *Renamer) HasDuplicateSuffix(current, target string, index int) bool {
	if index >= len(r.c.DuplicateSuffix) {
		return false
	}
	ext := pySplitExt(target)
	stem, ok := strings.CutSuffix(strings.TrimSuffix(target, ext), r.c.DuplicateSuffix[index])
	if !ok {
		return false
	}
	for _, suffix := range r.c.DuplicateSuffix[index+1:] {
		if current == stem+suffix+ext {
			return true
		}
	}
	return false
}

// templates is the plugin's get_template_filename / get_template_path plus
// the default template selection of renamer().
func (r *Renamer) templates(s Scene, f File) (templates, error) {
	var t templates

	if s.Studio != nil && len(r.c.StudioTemplates) > 0 {
		if v, ok := r.c.StudioTemplates[s.Studio.Name]; ok && v != "" {
			t.filename = v
		} else {
			for p := s.Studio.Parent; p != nil; p = p.Parent {
				if v, ok := r.c.StudioTemplates[p.Name]; ok && v != "" {
					t.filename = v
					break
				}
			}
		}
	}
	tagNames := make([]string, len(s.Tags))
	for i, tag := range s.Tags {
		tagNames[i] = tag.Name
	}
	if len(s.Tags) > 0 {
		for _, p := range r.tagTemplates {
			if slices.Contains(tagNames, p.key) {
				t.filename = p.value
				break
			}
		}
	}

	pt := &pathTemplate{}
	for _, p := range r.pPathTemplates {
		if strings.Contains(f.Path, p.key) {
			pt.destination = p.value
			break
		}
	}
	if s.Studio != nil {
		if v, ok := r.c.PStudioTemplates[s.Studio.Name]; ok && v != "" {
			pt.destination = v
		}
	}
	if len(s.Tags) > 0 {
		var matches []pair
		for _, p := range r.pTagTemplates {
			if slices.Contains(tagNames, p.key) {
				matches = append(matches, p)
			}
		}
		if len(matches) > 1 {
			keys := make([]string, len(matches))
			for i, m := range matches {
				keys[i] = m.key
			}
			return t, fmt.Errorf("%w (%s): keep only one", ErrAmbiguousMoveTags, strings.Join(keys, ", "))
		}
		if len(matches) == 1 {
			pt.destination = matches[0].value
		}
		for _, tag := range s.Tags {
			if opts := r.c.PTagOption[tag.Name]; len(opts) > 0 {
				pt.options = append(pt.options, opts...)
				if slices.Contains(opts, "clean_tag") {
					pt.cleanTagIDs = append(pt.cleanTagIDs, tag.ID)
				}
			}
		}
	}

	switch {
	case pt.destination != "":
		t.path = pt
	case r.c.PUseDefaultTemplate:
		t.path = &pathTemplate{destination: r.c.PDefaultTemplate}
	}

	if t.filename == "" && r.c.UseDefaultTemplate {
		t.filename = r.c.DefaultTemplate
	}
	return t, nil
}

// filterTags is the plugin's tags_as_list + filter_tags, without an order.
func (r *Renamer) filterTags(tags []Tag) []string {
	var all []string
	for _, t := range tags {
		for _, s := range append(slices.Clone(t.Aliases), t.Name, t.SortName) {
			if s = pyStripSpace(s); s != "" {
				all = append(all, s)
			}
		}
	}
	var ret []string
	for _, s := range all {
		if slices.Contains(ret, s) || slices.Contains(r.c.TagsBlacklist, s) {
			continue
		}
		if len(r.c.TagsWhitelist) > 0 && !slices.Contains(r.c.TagsWhitelist, s) {
			continue
		}
		ret = append(ret, s)
	}
	return ret
}

func (r *Renamer) orderTags(tags []string, opts Options) []string {
	ret := slices.Clone(tags)
	if opts.SortTags {
		sort.Strings(ret)
		return ret
	}
	if len(r.c.TagsWhitelist) > 0 {
		sort.SliceStable(ret, func(a, b int) bool {
			return slices.Index(r.c.TagsWhitelist, ret[a]) < slices.Index(r.c.TagsWhitelist, ret[b])
		})
	}
	return ret
}

// permute calls fn with the permutations of s (except s itself) until fn
// returns true.
func permute(s []string, fn func([]string) bool) {
	n := len(s)
	p := slices.Clone(s)
	c := make([]int, n)
	// Heap's algorithm
	for i := 0; i < n; {
		if c[i] < i {
			if i%2 == 0 {
				p[0], p[i] = p[i], p[0]
			} else {
				p[c[i]], p[i] = p[i], p[c[i]]
			}
			if fn(slices.Clone(p)) {
				return
			}
			c[i]++
			i = 0
		} else {
			c[i] = 0
			i++
		}
	}
}

func (r *Renamer) computeFile(s Scene, st *sceneState, index int, f File, tmpl templates, tags []string, opts Options) FileResult {
	fr := FileResult{Index: index, Current: f.Path}
	if tmpl.path != nil {
		fr.DryRun = slices.Contains(tmpl.path.options, "dry_run")
	}

	in, err := r.extractInfo(s, st, f, tmpl, tags)
	if err != nil {
		fr.Err = err
		return fr
	}

	var final string
	for _, removed := range r.orderField {
		if removed != "" {
			name := strings.ReplaceAll(removed, "$", "")
			if !in.truthy(name) {
				continue
			}
			in.remove(name)
		}

		newFilename := in.str["current_filename"]
		if tmpl.filename != "" {
			if newFilename, err = r.newFilename(in, st, tmpl.filename, index); err != nil {
				fr.Err = err
				return fr
			}
		}
		newDirectory := in.str["current_directory"]
		if tmpl.path != nil {
			newDirectory = r.newPath(in, st, opts)
		}
		final = pyJoin(newDirectory, newFilename)
		if r.c.IgnorePathLength || utf8.RuneCountInString(final) <= maxPathLength {
			break
		}
	}

	if !r.c.IgnorePathLength && utf8.RuneCountInString(final) > maxPathLength {
		fr.Err = fmt.Errorf("the path is too long (%d > %d): %s", utf8.RuneCountInString(final), maxPathLength, final)
		return fr
	}
	fr.Target = final
	return fr
}

// extractInfo is the plugin's extract_info.
func (r *Renamer) extractInfo(s Scene, st *sceneState, f File, tmpl templates, tags []string) (*info, error) {
	in := &info{str: make(map[string]string)}
	set := func(k, v string) { in.str[k] = v }

	set("current_path", f.Path)
	set("file_extension", pySplitExt(f.Path))
	set("current_filename", pyBasename(f.Path))
	set("current_directory", pyDirname(f.Path))
	if f.Oshash != "" {
		set("oshash", f.Oshash)
	}
	if f.Checksum != "" {
		set("checksum", f.Checksum)
	}
	if s.Code != "" {
		set("studio_code", s.Code)
	}
	if len(s.StashIDs) > 0 {
		set("stashid_scene", s.StashIDs[0])
	}

	if tmpl.path != nil {
		dest := strings.ReplaceAll(tmpl.path.destination, "^*", in.str["current_directory"])
		in.templateSplit = strings.Split(pyNormpath(dest), "/")
	}
	in.currentPathSplit = strings.Split(pyNormpath(f.Path), "/")

	// title, without the extension and leading prepositions
	title := s.Title
	extRe, err := regexp.Compile(in.str["file_extension"] + `$`)
	if err != nil {
		return nil, fmt.Errorf("file extension as pattern: %w", err)
	}
	if strings.HasSuffix(title, "\n") {
		// Python's $ also matches before a final newline
		title = extRe.ReplaceAllString(title[:len(title)-1], "") + "\n"
	} else {
		title = extRe.ReplaceAllString(title, "")
	}
	for _, re := range r.prepositions {
		title = re.ReplaceAllString(title, "")
	}
	set("title", title)

	if s.Date != "" {
		set("date", s.Date)
		d, err := time.Parse("2006-01-02", s.Date)
		if err != nil {
			return nil, fmt.Errorf("date %q: %w", s.Date, err)
		}
		v, err := strftime(r.c.DateFormat, d)
		if err != nil {
			return nil, err
		}
		set("date_format", v)
	}

	if r.c.DurationFormat != "" {
		v, err := strftime(r.c.DurationFormat, time.Unix(int64(f.Duration), 0).UTC())
		if err != nil {
			return nil, err
		}
		set("duration", v)
	} else {
		set("duration", pyFloat(f.Duration))
	}

	if s.Rating100 != 0 {
		v, err := pyFormat(r.c.RatingFormat, s.Rating100)
		if err != nil {
			return nil, err
		}
		set("rating", v)
	}

	r.performerInfo(in, st, tmpl)

	if s.Studio != nil {
		squeeze := func(n string) string {
			if r.c.SqueezeStudioNames {
				return strings.ReplaceAll(n, " ", "")
			}
			return n
		}
		set("studio", squeeze(s.Studio.Name))
		set("studio_family", in.str["studio"])
		hierarchy := []string{in.str["studio"]}
		if s.Studio.Parent != nil {
			set("parent_studio", squeeze(s.Studio.Parent.Name))
			set("studio_family", in.str["parent_studio"])
			for p := s.Studio.Parent; p != nil; p = p.Parent {
				hierarchy = append(hierarchy, squeeze(p.Name))
			}
			slices.Reverse(hierarchy)
		}
		in.studioHierarchy, in.hasHierarchy = hierarchy, true
	}

	if len(s.Tags) > 0 {
		set("tags", strings.Join(tags, r.c.TagsSplitchar))
	}

	set("bit_rate", pyFloat(pyRound2(float64(f.BitRate)/1000000)))
	resolution, height := "SD", strconv.Itoa(f.Height)+"p"
	if f.Height >= 720 {
		resolution = "HD"
	}
	if f.Height >= 2160 {
		height, resolution = "4k", "UHD"
	}
	if f.Height >= 2880 {
		height = "5k"
	}
	if f.Height >= 3384 {
		height = "6k"
	}
	if f.Height >= 4320 {
		height = "8k"
	}
	if f.Height > f.Width {
		resolution = "VERTICAL"
	}
	set("resolution", resolution)
	set("height", height)

	if len(s.Groups) > 0 {
		g := s.Groups[0]
		set("movie_title", g.Name)
		if g.Date != "" {
			set("movie_year", firstRunes(g.Date, 4))
		}
		if g.SceneIndex != 0 {
			set("movie_index", strconv.Itoa(g.SceneIndex))
			set("movie_scene", fmt.Sprintf("scene %d", g.SceneIndex))
		}
	}

	set("video_codec", strings.ToUpper(f.VideoCodec))
	set("audio_codec", strings.ToUpper(f.AudioCodec))

	if s.Date != "" {
		set("year", firstRunes(s.Date, 4))
	}

	if sep := r.c.FieldWhitespaceSeparator; sep != "" {
		for k, v := range in.str {
			switch k {
			case "current_path", "current_filename", "current_directory":
				continue
			}
			in.str[k] = strings.ReplaceAll(v, " ", sep)
		}
		for i, v := range in.performerList {
			in.performerList[i] = strings.ReplaceAll(v, " ", sep)
		}
		for i, v := range in.studioHierarchy {
			in.studioHierarchy[i] = strings.ReplaceAll(v, " ", sep)
		}
	}
	return in, nil
}

// performerInfo fills the performer fields of extract_info.
func (r *Renamer) performerInfo(in *info, st *sceneState, tmpl templates) {
	if len(st.performers) == 0 {
		if r.c.PathNoPerformerFolder {
			in.str["performer_path"] = "NoPerformer"
		}
		return
	}

	inverse := tmpl.path != nil && slices.Contains(tmpl.path.options, "inverse_performer")
	var (
		list           []string
		ratingKeys     = []string{"0"}
		rating         = map[string][]string{"0": nil}
		favorite       []string
		notFavorite    []string
		performerPath  string
		hasPerformPath bool
	)
	for i := range st.performers {
		p := &st.performers[i]
		if p.Gender != "" {
			if slices.Contains(r.c.PerformerIgnoreGender, p.Gender) {
				continue
			}
		} else if slices.Contains(r.c.PerformerIgnoreGender, "UNDEFINED") {
			continue
		}
		if inverse {
			// the plugin changes the name in the scene data, for all
			// following files too
			p.Name = inversePerformer.ReplaceAllString(p.Name, "$3 $1")
		}
		list = append(list, p.Name)
		key := "0"
		if p.Rating100 != 0 {
			key = strconv.Itoa(p.Rating100)
		}
		if _, ok := rating[key]; !ok {
			ratingKeys = append(ratingKeys, key)
		}
		rating[key] = append(rating[key], p.Name)
		if p.Favorite {
			favorite = append(favorite, p.Name)
		} else {
			notFavorite = append(notFavorite, p.Name)
		}
		if !hasPerformPath && r.c.PathKeepAlreadyPerformer && slices.Contains(in.currentPathSplit, p.Name) {
			performerPath, hasPerformPath = p.Name, true
		}
	}

	// rating groups, highest first (by their string value, like Python)
	sort.Sort(sort.Reverse(sort.StringSlice(ratingKeys)))
	appendUnique := func(dst []string, groups ...[]string) []string {
		for _, g := range groups {
			for _, n := range g {
				if !slices.Contains(dst, n) {
					dst = append(dst, n)
				}
			}
		}
		return dst
	}
	sortedGroups := func(groups ...[]string) [][]string {
		for _, g := range groups {
			sort.Strings(g)
		}
		return groups
	}
	ratingGroups := func() [][]string {
		var gs [][]string
		for _, k := range ratingKeys {
			gs = append(gs, rating[k])
		}
		return gs
	}

	switch r.c.PerformerSort {
	case "rating":
		list = appendUnique(nil, sortedGroups(ratingGroups()...)...)
	case "favorite":
		list = appendUnique(nil, sortedGroups(favorite, notFavorite)...)
	case "mix":
		sort.Strings(favorite)
		list = appendUnique(slices.Clone(favorite), sortedGroups(ratingGroups()...)...)
	case "mixid":
		list = appendUnique(slices.Clone(favorite), ratingGroups()...)
	case "name":
		sort.Strings(list)
	}

	if !hasPerformPath && len(list) > 0 {
		performerPath, hasPerformPath = list[0], true
	}
	if len(list) > r.c.PerformerLimit {
		if r.c.PerformerLimitKeep {
			list = list[:r.c.PerformerLimit]
		} else {
			list = nil
		}
	}
	in.str["performer"] = strings.Join(list, r.c.PerformerSplitchar)
	in.performerList, in.hasPerformerList = list, true
	if len(list) > 0 {
		var ids []string
		for _, name := range list {
			for _, p := range st.performers {
				if p.Name == name && len(p.StashIDs) > 0 {
					ids = append(ids, p.StashIDs[0])
					break
				}
			}
		}
		in.str["stashid_performer"] = strings.Join(ids, r.c.PerformerSplitchar)
	}
	if !r.c.PathOnePerformer {
		performerPath, hasPerformPath = strings.Join(list, r.c.PerformerSplitchar), true
	}
	if hasPerformPath {
		in.str["performer_path"] = performerPath
	}
}

var inversePerformer = regexp.MustCompile(`([a-zA-Z]+)(\s)([a-zA-Z]+)`)

var fieldPattern = regexp.MustCompile(`\$[\p{L}\p{N}_]+`)

// fieldReplace is the plugin's field_replacer. It returns the text with all
// fields but $title replaced, and the title value.
func (r *Renamer) fieldReplace(text string, in *info, st *sceneState) (string, string) {
	fields := fieldPattern.FindAllString(text, -1)
	sort.SliceStable(fields, func(a, b int) bool {
		return utf8.RuneCountInString(fields[a]) > utf8.RuneCountInString(fields[b])
	})

	result, title := text, ""
	for _, field := range fields {
		f := pyStrip(strings.ReplaceAll(field, "$", ""), "_")
		commands, configured := st.replacers["$"+f]
		commands = slices.Clone(commands)

		if f == "title" && r.c.PreventTitlePerformer {
			for _, p := range strings.Split(in.str["performer"], r.c.PerformerSplitchar) {
				commands = append(commands, Replacer{Replace: p})
			}
			// the plugin appends to the configured list itself
			if configured {
				st.replacers["$"+f] = slices.Clone(commands)
			}
		}

		word := in.str[f]
		for _, c := range commands {
			word = r.literalIgnoreCase(c.Replace).ReplaceAllLiteralString(word, c.With)
		}

		if f == "title" {
			title = pyStripSpace(word)
			continue
		}
		if word == "" {
			result = strings.ReplaceAll(result, field, "")
		} else {
			result = strings.ReplaceAll(result, "$"+f, word)
		}
	}
	return result, title
}

// replaceText is the plugin's replace_text (replace_words).
func (r *Renamer) replaceText(text string) string {
	for _, w := range r.replaceWords {
		switch w.mode {
		case "regex":
			text = w.re.ReplaceAllLiteralString(text, w.new)
		case "word":
			text = w.re.ReplaceAllString(text, "${1}"+strings.ReplaceAll(w.new, "$", "$$")+"${3}")
		case "any":
			text = strings.ReplaceAll(text, w.old, w.new)
		}
	}
	return text
}

// makeFilename is the plugin's makeFilename.
func (r *Renamer) makeFilename(in *info, st *sceneState, template string) string {
	result, title := r.fieldReplace(template, in, st)
	if len(r.replaceWords) > 0 {
		result = r.replaceText(result)
	}
	if title == "" {
		result = strings.ReplaceAll(result, "$title", "")
	}
	result = cleanupText(result)
	if title != "" {
		result = strings.ReplaceAll(result, "$title", title)
	}
	return strings.ReplaceAll(result, " ", r.c.FilenameSplitchar)
}

// makePath is the plugin's makePath, for one folder of the template.
func (r *Renamer) makePath(in *info, st *sceneState, template string) string {
	result, title := r.fieldReplace(strings.ReplaceAll(template, "$performer", "$performer_path"), in, st)
	if title == "" {
		result = strings.ReplaceAll(result, "$title", "")
	}
	result = cleanupText(result)
	if title != "" {
		result = strings.ReplaceAll(result, "$title", title)
	}
	return result
}

var (
	illegalCharacters = regexp.MustCompile(`[\\/:"*?<>|]+`)
	typographicQuotes = regexp.MustCompile(`[’‘”“]+`)
)

func (r *Renamer) removeCharacters(s string) string {
	if r.removeChars == "" {
		return s
	}
	return strings.Map(func(c rune) rune {
		if strings.ContainsRune(r.removeChars, c) {
			return -1
		}
		return c
	}, s)
}

// newFilename is the plugin's create_new_filename.
func (r *Renamer) newFilename(in *info, st *sceneState, template string, index int) (string, error) {
	if index >= len(r.c.DuplicateSuffix) {
		return "", fmt.Errorf("file %d of the scene: duplicate_suffix has only %d entries", index+1, len(r.c.DuplicateSuffix))
	}
	name := r.makeFilename(in, st, template) + r.c.DuplicateSuffix[index] + in.str["file_extension"]
	if r.c.LowercaseFilename {
		name = strings.ToLower(name)
	}
	if r.c.TitlecaseFilename {
		name = capitalizeWords(name)
	}
	name = illegalCharacters.ReplaceAllString(name, "")
	name = r.removeCharacters(name)
	return typographicQuotes.ReplaceAllString(name, "'"), nil
}

// newPath is the plugin's create_new_path. The volume (U:, UNC:) of the
// canonical path is kept as is; the plugin ran on /data/... paths, which have
// none.
func (r *Renamer) newPath(in *info, st *sceneState, opts Options) string {
	split := in.templateSplit
	var parts []string
	var raw []bool
	for _, part := range split {
		switch {
		case strings.Contains(part, ":") && split[0] != "":
			parts, raw = append(parts, part), append(raw, true)
		case part == "$studio_hierarchy":
			for _, p := range in.studioHierarchy {
				parts, raw = append(parts, pyStripSpace(illegalCharacters.ReplaceAllString(p, ""))), append(raw, false)
			}
		default:
			parts, raw = append(parts, pyStripSpace(illegalCharacters.ReplaceAllString(r.makePath(in, st, part), ""))), append(raw, false)
		}
	}

	clean := func(i int) string {
		if raw[i] {
			return parts[i]
		}
		return typographicQuotes.ReplaceAllString(r.removeCharacters(parts[i]), "'")
	}

	var out []string
	if len(parts) > 0 && parts[0] == "" {
		// the template started with a separator
		out = append(out, "")
	}
	prev := ""
	for i := range parts {
		// the plugin dropped empty folders and compared neighbours before
		// removing characters; compare what ends up in the path instead
		key := parts[i]
		if !opts.LegacyNesting {
			key = clean(i)
		}
		if key == "" {
			continue
		}
		if r.c.PreventConsecutive && key == prev {
			continue
		}
		prev = key
		out = append(out, clean(i))
	}
	return strings.Join(out, "/")
}

func firstRunes(s string, n int) string {
	rs := []rune(s)
	if len(rs) > n {
		rs = rs[:n]
	}
	return string(rs)
}

// pyNormpath is Python's posixpath.normpath.
func pyNormpath(p string) string {
	if p == "" {
		return "."
	}
	initial := 0
	if strings.HasPrefix(p, "/") {
		initial = 1
		if strings.HasPrefix(p, "//") && !strings.HasPrefix(p, "///") {
			initial = 2
		}
	}
	var comps []string
	for _, c := range strings.Split(p, "/") {
		if c == "" || c == "." {
			continue
		}
		if c != ".." || (initial == 0 && len(comps) == 0) || (len(comps) > 0 && comps[len(comps)-1] == "..") {
			comps = append(comps, c)
		} else if len(comps) > 0 {
			comps = comps[:len(comps)-1]
		}
	}
	ret := strings.Repeat("/", initial) + strings.Join(comps, "/")
	if ret == "" {
		return "."
	}
	return ret
}

// pyBasename is Python's posixpath.basename.
func pyBasename(p string) string {
	return p[strings.LastIndex(p, "/")+1:]
}

// pyDirname is Python's posixpath.dirname.
func pyDirname(p string) string {
	head := p[:strings.LastIndex(p, "/")+1]
	if head != "" && strings.Trim(head, "/") != "" {
		head = strings.TrimRight(head, "/")
	}
	return head
}

// pySplitExt returns the extension of Python's posixpath.splitext.
func pySplitExt(p string) string {
	sep := strings.LastIndex(p, "/")
	dot := strings.LastIndex(p, ".")
	if dot > sep {
		for i := sep + 1; i < dot; i++ {
			if p[i] != '.' {
				return p[dot:]
			}
		}
	}
	return ""
}

// pyJoin is Python's posixpath.join for two parts.
func pyJoin(a, b string) string {
	if strings.HasPrefix(b, "/") {
		return b
	}
	if a == "" || strings.HasSuffix(a, "/") {
		return a + b
	}
	return a + "/" + b
}
