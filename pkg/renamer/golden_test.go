package renamer

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// TestGolden compares the engine with the output of the original plugin for
// a real library. RENAMER_GOLDEN_DIR holds data.json (organized scenes in the
// plugin's GraphQL shape, plus studios), golden.txt (the plugin's results,
// one line per file: id|index|status|current|final|message) and renamer.yml.
// See docs/architecture/renamer.md for how to produce them; the data contains
// media titles and stays out of the repository.
func TestGolden(t *testing.T) {
	dir := os.Getenv("RENAMER_GOLDEN_DIR")
	if dir == "" {
		t.Skip("RENAMER_GOLDEN_DIR not set")
	}

	cfg, err := LoadConfig(filepath.Join(dir, "renamer.yml"))
	if err != nil {
		t.Fatal(err)
	}
	r, err := New(*cfg)
	if err != nil {
		t.Fatal(err)
	}

	scenes, err := loadGoldenScenes(filepath.Join(dir, "data.json"))
	if err != nil {
		t.Fatal(err)
	}
	golden, err := loadGolden(filepath.Join(dir, "golden.txt"))
	if err != nil {
		t.Fatal(err)
	}

	var diffs []string
	counts := map[string]int{}
	production := map[string]int{}
	for _, s := range scenes {
		want := golden[s.ID]
		res := r.Compute(s, Options{SortTags: true, LegacyNesting: true})
		if res.Skip == "no title" {
			counts["skipped: no title"]++
			continue
		}
		if res.Skip != "" || res.Err != nil {
			if len(want) == 0 || want[0].status != "error" {
				diffs = append(diffs, fmt.Sprintf("%d: skip=%q err=%v, plugin: %+v", s.ID, res.Skip, res.Err, want))
			}
			counts["error"]++
			continue
		}
		if len(want) != len(res.Files) {
			diffs = append(diffs, fmt.Sprintf("%d: %d files, plugin %d", s.ID, len(res.Files), len(want)))
			continue
		}
		for i, f := range res.Files {
			w := want[i]
			var got string
			switch {
			case f.Err != nil:
				got = "error"
			case f.Target == f.Current:
				got = "same"
			default:
				got = "move"
			}
			counts[got]++
			if got != w.status || (got == "move" && f.Target != w.final) {
				diffs = append(diffs, fmt.Sprintf("%d/%d: %s %q (err %v)\n   plugin %s %q", s.ID, i, got, f.Target, f.Err, w.status, w.final))
			}
		}

		// the production options, for the record
		for _, f := range r.Compute(s, Options{}).Files {
			switch {
			case f.Err != nil:
				production["error"]++
			case f.Target == f.Current:
				production["same"]++
			case filepath.Dir(f.Target) != filepath.Dir(f.Current):
				production["move"]++
			default:
				production["rename"]++
			}
		}
	}

	t.Logf("plugin options: %v", counts)
	t.Logf("production options: %v", production)
	for i, d := range diffs {
		if i == 30 {
			t.Logf("... %d more", len(diffs)-30)
			break
		}
		t.Log(d)
	}
	if len(diffs) > 0 {
		t.Fatalf("%d differences to the plugin", len(diffs))
	}
}

type goldenLine struct {
	status string
	final  string
}

func loadGolden(path string) (map[int][]goldenLine, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	ret := map[int][]goldenLine{}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		p := strings.SplitN(sc.Text(), "|", 6)
		if len(p) != 6 {
			return nil, fmt.Errorf("bad line %q", sc.Text())
		}
		id, err := strconv.Atoi(p[0])
		if err != nil {
			return nil, err
		}
		ret[id] = append(ret[id], goldenLine{status: p[2], final: p[4]})
	}
	return ret, sc.Err()
}

type gqlStudio struct {
	ID     string     `json:"id"`
	Name   string     `json:"name"`
	Parent *gqlStudio `json:"parent_studio"`
}

type gqlStashID struct {
	StashID string `json:"stash_id"`
}

type gqlScene struct {
	ID        string       `json:"id"`
	Title     *string      `json:"title"`
	Date      *string      `json:"date"`
	Rating100 *int         `json:"rating100"`
	Organized bool         `json:"organized"`
	Code      *string      `json:"code"`
	StashIDs  []gqlStashID `json:"stash_ids"`
	Files     []struct {
		Path       string  `json:"path"`
		VideoCodec string  `json:"video_codec"`
		AudioCodec string  `json:"audio_codec"`
		Width      int     `json:"width"`
		Height     int     `json:"height"`
		Duration   float64 `json:"duration"`
		BitRate    int64   `json:"bit_rate"`
		Oshash     *string `json:"oshash"`
		Checksum   *string `json:"checksum"`
	} `json:"files"`
	Studio *gqlStudio `json:"studio"`
	Tags   []struct {
		ID       string   `json:"id"`
		Name     string   `json:"name"`
		SortName *string  `json:"sort_name"`
		Aliases  []string `json:"aliases"`
	} `json:"tags"`
	Performers []struct {
		Name      string       `json:"name"`
		Gender    *string      `json:"gender"`
		Favorite  bool         `json:"favorite"`
		Rating100 *int         `json:"rating100"`
		StashIDs  []gqlStashID `json:"stash_ids"`
	} `json:"performers"`
	Movies []struct {
		Movie struct {
			Name string  `json:"name"`
			Date *string `json:"date"`
		} `json:"movie"`
		SceneIndex *int `json:"scene_index"`
	} `json:"movies"`
}

func deref[T any](p *T) T {
	var zero T
	if p == nil {
		return zero
	}
	return *p
}

func loadGoldenScenes(path string) ([]Scene, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var dump struct {
		Scenes  []gqlScene  `json:"scenes"`
		Studios []gqlStudio `json:"studios"`
	}
	if err := json.Unmarshal(data, &dump); err != nil {
		return nil, err
	}

	studios := map[string]gqlStudio{}
	for _, s := range dump.Studios {
		studios[s.ID] = s
	}
	var chain func(id string, depth int) (*Studio, error)
	chain = func(id string, depth int) (*Studio, error) {
		if depth > 100 {
			return nil, errors.New("studio loop")
		}
		s, ok := studios[id]
		if !ok {
			return nil, fmt.Errorf("studio %s missing", id)
		}
		ret := &Studio{Name: s.Name}
		if s.Parent != nil {
			p, err := chain(s.Parent.ID, depth+1)
			if err != nil {
				return nil, err
			}
			ret.Parent = p
		}
		return ret, nil
	}

	var ret []Scene
	for _, g := range dump.Scenes {
		id, err := strconv.Atoi(g.ID)
		if err != nil {
			return nil, err
		}
		s := Scene{
			ID:        id,
			Title:     deref(g.Title),
			Date:      deref(g.Date),
			Rating100: deref(g.Rating100),
			Organized: g.Organized,
			Code:      deref(g.Code),
		}
		for _, sid := range g.StashIDs {
			s.StashIDs = append(s.StashIDs, sid.StashID)
		}
		for _, f := range g.Files {
			s.Files = append(s.Files, File{
				Path: f.Path, VideoCodec: f.VideoCodec, AudioCodec: f.AudioCodec,
				Width: f.Width, Height: f.Height, Duration: f.Duration, BitRate: f.BitRate,
				Oshash: deref(f.Oshash), Checksum: deref(f.Checksum),
			})
		}
		if g.Studio != nil {
			if s.Studio, err = chain(g.Studio.ID, 0); err != nil {
				return nil, err
			}
		}
		for _, tg := range g.Tags {
			tid, err := strconv.Atoi(tg.ID)
			if err != nil {
				return nil, err
			}
			s.Tags = append(s.Tags, Tag{ID: tid, Name: tg.Name, SortName: deref(tg.SortName), Aliases: tg.Aliases})
		}
		for _, p := range g.Performers {
			perf := Performer{Name: p.Name, Gender: deref(p.Gender), Favorite: p.Favorite, Rating100: deref(p.Rating100)}
			for _, sid := range p.StashIDs {
				perf.StashIDs = append(perf.StashIDs, sid.StashID)
			}
			s.Performers = append(s.Performers, perf)
		}
		for _, m := range g.Movies {
			s.Groups = append(s.Groups, Group{Name: m.Movie.Name, Date: deref(m.Movie.Date), SceneIndex: deref(m.SceneIndex)})
		}
		ret = append(ret, s)
	}
	sort.Slice(ret, func(a, b int) bool { return ret[a].ID < ret[b].ID })
	return ret, nil
}
