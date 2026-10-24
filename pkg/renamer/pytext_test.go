package renamer

import (
	"testing"
	"time"
)

// Expected values are the results of the plugin's Python functions.

func TestCleanupText(t *testing.T) {
	for in, want := range map[string]string{
		"A () B":                  "A B",
		"[ ] x":                   "x",
		"{ }y":                    "y",
		"{$a b}":                  "$a b",
		"a -- b":                  "a - b",
		"x...y":                   "x.y",
		"a-_-_-b":                 "a-_-_-b",
		"--a--":                   "a",
		"Ü...Ü":                   "Ü.Ü",
		"a!!b":                    "a!b",
		"a!?!?b":                  "a!?b",
		"a, , b":                  "a, b",
		"(é)":                     "(é)",
		"a—–b":                    "a—–b",
		"{Studio }{ }title HD{ }": "Studio title HD",
		"a----b":                  "a-b",
		"a- - -b":                 "a- -b",
		"a.,.,b":                  "a.,b",
		"_.x._":                   "x",
	} {
		if got := cleanupText(in); got != want {
			t.Errorf("cleanupText(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRemoveConsecutiveNonword(t *testing.T) {
	for in, want := range map[string]string{
		"a--b":     "a-b",
		"a-_-_-b":  "a-_-_-b",
		"a- - -b":  "a- -b",
		"a!!!b":    "a!b",
		"a.,.,b":   "a.,b",
		"a----b":   "a-b",
		"a-- --b":  "a- -b",
		"ab":       "ab",
		"--":       "-",
		"a.-.-.-b": "a.-b",
	} {
		if got := removeConsecutiveNonword(in); got != want {
			t.Errorf("removeConsecutiveNonword(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCapitalizeWords(t *testing.T) {
	for in, want := range map[string]string{
		"hello_world.avi":                    "hello_world.Avi",
		"the-big-and-small":                  "The-Big-And-Small",
		"don't-stop":                         "Don't-Stop",
		"Müller-der-mann":                    "Müller-Der-Mann",
		"x'abc":                              "X'abc",
		"and":                                "And",
		"a and b":                            "A and B",
		"ABC-def":                            "ABC-Def",
		"über-alles":                         "über-Alles",
		"1080p-video":                        "1080p-Video",
		"o’neil-x":                           "O’neil-X",
		"rock and roll.mkv":                  "Rock and Roll.Mkv",
		"Studio_Perf_the-title-of-it_HD.wmv": "Studio_Perf_the-Title-Of-it_HD.Wmv",
		"x.mp4":                              "X.mp4",
		"Ärger-im-büro.Avi":                  "Ärger-Im-büro.Avi",
		"'quoted'-word":                      "'Quoted'-Word",
	} {
		if got := capitalizeWords(in); got != want {
			t.Errorf("capitalizeWords(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestPythonPathHelpers(t *testing.T) {
	if got := pyNormpath("U:/a//b/./c/../d/"); got != "U:/a/b/d" {
		t.Errorf("normpath = %q", got)
	}
	if got := pyDirname("U:/a/b.mp4"); got != "U:/a" {
		t.Errorf("dirname = %q", got)
	}
	if got := pySplitExt("U:/a/.hidden"); got != "" {
		t.Errorf("splitext(.hidden) = %q", got)
	}
	if got := pySplitExt("U:/a.b/c.MP4"); got != ".MP4" {
		t.Errorf("splitext = %q", got)
	}
	if got := pyFloat(pyRound2(4567890 / 1e6)); got != "4.57" {
		t.Errorf("bit rate = %q", got)
	}
	if got := pyFloat(5); got != "5.0" {
		t.Errorf("pyFloat(5) = %q", got)
	}
	if got, _ := strftime("%Y-%m-%d %B %-d", time.Date(2024, 3, 7, 0, 0, 0, 0, time.UTC)); got != "2024-03-07 March 7" {
		t.Errorf("strftime = %q", got)
	}
}
