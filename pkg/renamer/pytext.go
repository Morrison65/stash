package renamer

// Text helpers with the semantics of the Python functions and regular
// expressions used by the renamerOnUpdate plugin. Python's \w, \W, \s and \b
// are Unicode-aware and RE2 does not support back-references, so the
// patterns that depend on them are implemented by hand.

import (
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// pySpaceClass is the content of a character class equivalent to Python's \s.
const pySpaceClass = `\t\n\v\f\r\x1c-\x1f\x85\p{Z}`

// pyIsSpace reports whether Python's str.isspace() is true for r.
func pyIsSpace(r rune) bool {
	switch r {
	case '\t', '\n', '\v', '\f', '\r', ' ', 0x1c, 0x1d, 0x1e, 0x1f, 0x85:
		return true
	}
	return unicode.In(r, unicode.Z)
}

// pyIsAlnum reports whether Python's str.isalnum() is true for r.
func pyIsAlnum(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsNumber(r)
}

// pyIsWord reports whether r matches Python's \w.
func pyIsWord(r rune) bool {
	return pyIsAlnum(r) || r == '_'
}

// pyStrip is Python's str.strip(chars).
func pyStrip(s, chars string) string {
	return strings.Trim(s, chars)
}

// pyStripSpace is Python's str.strip().
func pyStripSpace(s string) string {
	return strings.TrimFunc(s, pyIsSpace)
}

// pyFloat formats f like Python's str(float).
func pyFloat(f float64) string {
	if math.IsInf(f, 0) || math.IsNaN(f) {
		return strings.ToLower(strconv.FormatFloat(f, 'g', -1, 64))
	}
	abs := math.Abs(f)
	if abs != 0 && (abs < 1e-4 || abs >= 1e16) {
		s := strconv.FormatFloat(f, 'e', -1, 64)
		// Python: 1e-05, Go: 1e-05; Python: 1e+16, Go: 1e+16
		return s
	}
	s := strconv.FormatFloat(f, 'f', -1, 64)
	if !strings.Contains(s, ".") {
		s += ".0"
	}
	return s
}

// pyRound2 is Python's round(f, 2).
func pyRound2(f float64) float64 {
	v, _ := strconv.ParseFloat(strconv.FormatFloat(f, 'f', 2, 64), 64)
	return v
}

var cleanupGroups = regexp.MustCompile(`\([^\p{L}\p{N}_]*\)|\[[^\p{L}\p{N}_]*\]|\{[^a-zA-Z0-9]*\}`)

// cleanupText is the plugin's cleanup_text: removes groups without
// alphanumeric content, braces and repeated separators, then trims " -_.".
func cleanupText(text string) string {
	text = cleanupGroups.ReplaceAllString(text, "")
	text = strings.NewReplacer("{", "", "}", "").Replace(text)
	text = removeConsecutiveNonword(text)
	return pyStrip(text, " -_.")
}

// removeConsecutiveNonword is the plugin's remove_consecutive_nonword:
// re.sub(r"(\W+)\1+", r"\1", text), repeated up to 10 times.
func removeConsecutiveNonword(text string) string {
	for i := 0; i < 10; i++ {
		out, changed := subRepeatedNonword([]rune(text))
		if !changed {
			break
		}
		text = out
	}
	return text
}

func subRepeatedNonword(rs []rune) (string, bool) {
	var b strings.Builder
	changed := false
	for pos := 0; pos < len(rs); {
		if l, n := matchRepeatedNonword(rs, pos); l > 0 {
			b.WriteString(string(rs[pos : pos+l]))
			pos += l * n
			changed = true
			continue
		}
		b.WriteRune(rs[pos])
		pos++
	}
	return b.String(), changed
}

// matchRepeatedNonword matches (\W+)\1+ at pos like a backtracking engine:
// the longest group that is repeated at least once wins, and the repeat is
// greedy. It returns the group length and the total number of copies.
func matchRepeatedNonword(rs []rune, pos int) (int, int) {
	run := 0
	for pos+run < len(rs) && !pyIsWord(rs[pos+run]) {
		run++
	}
	same := func(a, b, l int) bool {
		for i := 0; i < l; i++ {
			if rs[a+i] != rs[b+i] {
				return false
			}
		}
		return true
	}
	for l := run; l >= 1; l-- {
		if pos+2*l > len(rs) || !same(pos, pos+l, l) {
			continue
		}
		n := 2
		for pos+(n+1)*l <= len(rs) && same(pos, pos+n*l, l) {
			n++
		}
		return l, n
	}
	return 0, 0
}

// capitalizeWords is the plugin's capitalizeWords (title case):
// re.sub(r"\b[A-Z]?[a-z'’‘]+\b", process_word, s) with Unicode word
// boundaries.
func capitalizeWords(s string) string {
	rs := []rune(s)
	isWord := func(i int) bool { return i >= 0 && i < len(rs) && pyIsWord(rs[i]) }
	boundary := func(i int) bool { return isWord(i-1) != isWord(i) }
	inClass := func(r rune) bool { return r >= 'a' && r <= 'z' || r == '\'' || r == '’' || r == '‘' }

	match := func(pos int) int {
		if !boundary(pos) {
			return -1
		}
		q := pos
		if rs[pos] >= 'A' && rs[pos] <= 'Z' {
			q++
		}
		run := 0
		for q+run < len(rs) && inClass(rs[q+run]) {
			run++
		}
		for l := run; l >= 1; l-- {
			if boundary(q + l) {
				return q + l
			}
		}
		return -1
	}

	exceptions := map[string]bool{"and": true, "of": true, "the": true}
	process := func(start, end int) string {
		word := string(rs[start:end])
		var preceding, following rune
		hasPreceding, hasFollowing := false, false
		for i := start - 1; i >= 0; i-- {
			if !pyIsSpace(rs[i]) {
				preceding, hasPreceding = rs[i], true
				break
			}
		}
		for i := end; i < len(rs); i++ {
			if !pyIsSpace(rs[i]) {
				following, hasFollowing = rs[i], true
				break
			}
		}
		if start == 0 || end == len(rs) || !exceptions[strings.ToLower(word)] ||
			(hasPreceding && !pyIsAlnum(preceding)) || (hasFollowing && !pyIsAlnum(following)) {
			return pyCapitalize(word)
		}
		return strings.ToLower(word)
	}

	var b strings.Builder
	for pos := 0; pos < len(rs); {
		if end := match(pos); end > 0 {
			b.WriteString(process(pos, end))
			pos = end
			continue
		}
		b.WriteRune(rs[pos])
		pos++
	}
	return b.String()
}

// pyCapitalize is Python's str.capitalize().
func pyCapitalize(s string) string {
	rs := []rune(s)
	for i, r := range rs {
		if i == 0 {
			rs[i] = unicode.ToTitle(r)
		} else {
			rs[i] = unicode.ToLower(r)
		}
	}
	return string(rs)
}

var (
	strftimeDays   = []string{"Sunday", "Monday", "Tuesday", "Wednesday", "Thursday", "Friday", "Saturday"}
	strftimeMonths = []string{"January", "February", "March", "April", "May", "June", "July", "August", "September", "October", "November", "December"}
)

// strftime is Python's strftime for the common directives (C locale).
func strftime(format string, t time.Time) (string, error) {
	var b strings.Builder
	rs := []rune(format)
	for i := 0; i < len(rs); i++ {
		if rs[i] != '%' {
			b.WriteRune(rs[i])
			continue
		}
		i++
		if i >= len(rs) {
			return "", fmt.Errorf("format %q ends with %%", format)
		}
		pad := true
		if rs[i] == '-' && i+1 < len(rs) {
			pad = false
			i++
		}
		num := func(v, width int) string {
			if !pad {
				return strconv.Itoa(v)
			}
			return fmt.Sprintf("%0*d", width, v)
		}
		switch rs[i] {
		case 'Y':
			b.WriteString(strconv.Itoa(t.Year()))
		case 'y':
			b.WriteString(num(t.Year()%100, 2))
		case 'm':
			b.WriteString(num(int(t.Month()), 2))
		case 'd':
			b.WriteString(num(t.Day(), 2))
		case 'H':
			b.WriteString(num(t.Hour(), 2))
		case 'I':
			h := t.Hour() % 12
			if h == 0 {
				h = 12
			}
			b.WriteString(num(h, 2))
		case 'M':
			b.WriteString(num(t.Minute(), 2))
		case 'S':
			b.WriteString(num(t.Second(), 2))
		case 'j':
			b.WriteString(num(t.YearDay(), 3))
		case 'p':
			if t.Hour() < 12 {
				b.WriteString("AM")
			} else {
				b.WriteString("PM")
			}
		case 'B':
			b.WriteString(strftimeMonths[t.Month()-1])
		case 'b':
			b.WriteString(strftimeMonths[t.Month()-1][:3])
		case 'A':
			b.WriteString(strftimeDays[t.Weekday()])
		case 'a':
			b.WriteString(strftimeDays[t.Weekday()][:3])
		case '%':
			b.WriteRune('%')
		default:
			return "", fmt.Errorf("unsupported directive %%%c in %q", rs[i], format)
		}
	}
	return b.String(), nil
}

// pyFormat is Python's str.format with one positional argument, for the
// rating format ("{}" placeholders only).
func pyFormat(format string, v int) (string, error) {
	rest := strings.ReplaceAll(format, "{}", "")
	if strings.ContainsAny(rest, "{}") {
		return "", fmt.Errorf("unsupported format %q: only {} placeholders are supported", format)
	}
	return strings.ReplaceAll(format, "{}", strconv.Itoa(v)), nil
}
