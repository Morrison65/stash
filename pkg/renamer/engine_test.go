package renamer

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

const testConfig = `
enabled: true
default_template: "{$studio_family }{$performer }$title $resolution{ $tags}"
use_default_template: true
p_tag_templates:
  MOVE_D: "D:/media/videos/$performer"
  MOVE_U: "U:/Videos/$performer"
p_use_default_template: true
p_default_template: "^*/$performer"
p_tag_option:
  MOVE_D: [clean_tag]
  MOVE_U: [clean_tag]
associated_extension: [srt]
filename_splitchar: "_"
field_whitespaceSeperator: "-"
field_multi_replacer:
  "$title":
    - {replace: "Step", with: "St"}
replace_words:
  "ä|Ä": [ae, regex]
date_format: "%Y-%m-%d"
titlecase_Filename: true
removecharac_Filename: "#?!*~,:|''%@+ßäüö…"
performer_splitchar: "&"
performer_limit: 3
performer_sort: "none"
duplicate_suffix: ["", "-001", "-002"]
prevent_title_performer: true
prevent_consecutive: true
remove_emptyfolder: true
path_one_performer: true
path_keep_alrperf: true
prepositions_list: [The, A, An]
prepositions_removal: true
squeeze_studio_names: true
rating_format: "{}"
tags_splitchar: "&"
tags_whitelist: [Anal, Gabs, Payed]
order_field: [$video_codec, $tags, $studio_family, $performer]
`

func testRenamer(t *testing.T) *Renamer {
	t.Helper()
	p := filepath.Join(t.TempDir(), "renamer.yml")
	if err := os.WriteFile(p, []byte(testConfig), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := LoadConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	r, err := New(*c)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func testScene(path string) Scene {
	return Scene{
		ID:         1,
		Title:      "Jane Doe Gets A Step Ahead",
		Organized:  true,
		Files:      []File{{Path: path, VideoCodec: "h264", AudioCodec: "aac", Width: 1920, Height: 1080}},
		Studio:     &Studio{Name: "Big Studio"},
		Performers: []Performer{{Name: "Jane Doe"}},
		Tags:       []Tag{{ID: 7, Name: "Gabs"}, {ID: 8, Name: "Anal"}},
	}
}

func TestComputeDefaultTemplate(t *testing.T) {
	r := testRenamer(t)
	res := r.Compute(testScene("U:/Videos/inbox/old name.mp4"), Options{})
	if res.Skip != "" || res.Err != nil || len(res.Files) != 1 {
		t.Fatalf("unexpected result %+v", res)
	}
	want := "U:/Videos/inbox/Jane-Doe/BigStudio_Jane-Doe_-Gets-A-St-Ahead_HD_Anal&Gabs.mp4"
	if got := res.Files[0].Target; got != want {
		t.Errorf("target\n got %s\nwant %s", got, want)
	}
	if len(res.CleanTagIDs) != 0 {
		t.Errorf("clean tags without a move tag: %v", res.CleanTagIDs)
	}
}

func TestComputeRequiresOrganizedAndTitle(t *testing.T) {
	r := testRenamer(t)
	s := testScene("U:/Videos/a.mp4")
	s.Organized = false
	if res := r.Compute(s, Options{}); res.Skip != "not organized" || len(res.Files) != 0 {
		t.Errorf("not organized: %+v", res)
	}
	s = testScene("U:/Videos/a.mp4")
	s.Title = ""
	if res := r.Compute(s, Options{}); res.Skip != "no title" || len(res.Files) != 0 {
		t.Errorf("no title: %+v", res)
	}
}

func TestComputeKeepsTagOrder(t *testing.T) {
	r := testRenamer(t)
	current := "U:/Videos/Jane-Doe/BigStudio_Jane-Doe_-Gets-A-St-Ahead_HD_Gabs&Anal.mp4"
	res := r.Compute(testScene(current), Options{})
	if f := res.Files[0]; f.Changed() {
		t.Errorf("renamed only to reorder tags: %s", f.Target)
	}
	// with sorted tags (the golden test mode) the name changes
	res = r.Compute(testScene(current), Options{SortTags: true})
	if f := res.Files[0]; !f.Changed() {
		t.Error("sorted tags did not change the name")
	}
}

func TestComputeNoNestingAfterCharacterRemoval(t *testing.T) {
	r := testRenamer(t)
	s := testScene("U:/Videos/Kim-OHara/x.mp4")
	s.Performers = []Performer{{Name: "Kim O'Hara"}}
	s.Title = "Something"

	res := r.Compute(s, Options{})
	if got, want := res.Files[0].Target, "U:/Videos/Kim-OHara/BigStudio_Kim-OHara_Something_HD_Anal&Gabs.mp4"; got != want {
		t.Errorf("target\n got %s\nwant %s", got, want)
	}
	// the plugin nests the folder again
	res = r.Compute(s, Options{LegacyNesting: true})
	if got, want := res.Files[0].Target, "U:/Videos/Kim-OHara/Kim-OHara/BigStudio_Kim-OHara_Something_HD_Anal&Gabs.mp4"; got != want {
		t.Errorf("legacy target\n got %s\nwant %s", got, want)
	}
}

func TestComputeMoveTag(t *testing.T) {
	r := testRenamer(t)
	s := testScene("U:/Videos/inbox/a.mp4")
	s.Tags = append(s.Tags, Tag{ID: 9, Name: "MOVE_D"})
	res := r.Compute(s, Options{})
	if got, want := res.Files[0].Target, "D:/media/videos/Jane-Doe/BigStudio_Jane-Doe_-Gets-A-St-Ahead_HD_Anal&Gabs.mp4"; got != want {
		t.Errorf("target\n got %s\nwant %s", got, want)
	}
	if len(res.CleanTagIDs) != 1 || res.CleanTagIDs[0] != 9 {
		t.Errorf("clean tags %v", res.CleanTagIDs)
	}

	s.Tags = append(s.Tags, Tag{ID: 10, Name: "MOVE_U"})
	res = r.Compute(s, Options{})
	if !errors.Is(res.Err, ErrAmbiguousMoveTags) || len(res.Files) != 0 {
		t.Errorf("two move tags: %+v", res)
	}
}

func TestComputeUNCVolume(t *testing.T) {
	r := testRenamer(t)
	res := r.Compute(testScene("UNC:/nas/share/lib/a.mp4"), Options{})
	if got, want := res.Files[0].Target, "UNC:/nas/share/lib/Jane-Doe/BigStudio_Jane-Doe_-Gets-A-St-Ahead_HD_Anal&Gabs.mp4"; got != want {
		t.Errorf("target\n got %s\nwant %s", got, want)
	}
}

func TestComputeSecondFileSuffix(t *testing.T) {
	r := testRenamer(t)
	s := testScene("U:/Videos/a.mp4")
	s.Files = append(s.Files, File{Path: "U:/Videos/b.mp4", Width: 640, Height: 480})
	res := r.Compute(s, Options{})
	if got, want := res.Files[1].Target, "U:/Videos/Jane-Doe/BigStudio_Jane-Doe_-Gets-A-St-Ahead_SD_Anal&Gabs-001.mp4"; got != want {
		t.Errorf("second file\n got %s\nwant %s", got, want)
	}
}

func TestHasDuplicateSuffix(t *testing.T) {
	r := testRenamer(t)
	if !r.HasDuplicateSuffix("U:/a/x_HD-001.mp4", "U:/a/x_HD.mp4", 0) {
		t.Error("x_HD-001 not recognised")
	}
	if r.HasDuplicateSuffix("U:/a/y_HD-001.mp4", "U:/a/x_HD.mp4", 0) {
		t.Error("other name recognised")
	}
	if !r.HasDuplicateSuffix("U:/a/x_HD-002.mp4", "U:/a/x_HD-001.mp4", 1) {
		t.Error("second file with a later suffix not recognised")
	}
}

func TestConfigRejectsTypos(t *testing.T) {
	p := filepath.Join(t.TempDir(), "renamer.yml")
	if err := os.WriteFile(p, []byte("enabled: true\nperformer_limt: 3\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(p); err == nil {
		t.Error("unknown key accepted")
	}
}
