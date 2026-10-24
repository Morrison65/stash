// Package renamer computes where a scene's files belong, from templates. It is
// a port of the renamerOnUpdate community plugin (v2.5.0 with the local
// patches of this installation) and produces the same file and folder names
// for the same configuration; docs/architecture/renamer.md lists the
// intentional differences.
//
// The package is pure: it neither reads the database nor touches files.
// internal/manager/renamer.go feeds it scenes and carries out the moves.
package renamer

import (
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
	"sync"

	"gopkg.in/yaml.v2"
)

// Replacer replaces text in a field, case-insensitively.
type Replacer struct {
	Replace string `yaml:"replace"`
	With    string `yaml:"with"`
}

// Config is the renamer configuration (renamer.yml). The keys and their
// meaning are those of the plugin's config.py; paths are library paths in
// Stash's canonical form (U:/Videos, UNC:/server/share/...).
type Config struct {
	// Enabled turns the renamer on. Off (or no renamer.yml) leaves files alone.
	Enabled bool `yaml:"enabled"`
	// DryRun only logs what would be renamed.
	DryRun bool `yaml:"dry_run"`

	// file name templates
	TagTemplates       yaml.MapSlice     `yaml:"tag_templates"`
	StudioTemplates    map[string]string `yaml:"studio_templates"`
	UseDefaultTemplate bool              `yaml:"use_default_template"`
	DefaultTemplate    string            `yaml:"default_template"`

	// folder templates
	PTagTemplates       yaml.MapSlice       `yaml:"p_tag_templates"`
	PStudioTemplates    map[string]string   `yaml:"p_studio_templates"`
	PPathTemplates      yaml.MapSlice       `yaml:"p_path_templates"`
	PUseDefaultTemplate bool                `yaml:"p_use_default_template"`
	PDefaultTemplate    string              `yaml:"p_default_template"`
	PTagOption          map[string][]string `yaml:"p_tag_option"`

	AssociatedExtension      []string      `yaml:"associated_extension"`
	FilenameSplitchar        string        `yaml:"filename_splitchar"`
	FieldWhitespaceSeparator string        `yaml:"field_whitespaceSeperator"`
	FieldMultiReplacer       yaml.MapSlice `yaml:"field_multi_replacer"`
	ReplaceWords             yaml.MapSlice `yaml:"replace_words"`
	DateFormat               string        `yaml:"date_format"`
	DurationFormat           string        `yaml:"duration_format"`
	LowercaseFilename        bool          `yaml:"lowercase_Filename"`
	TitlecaseFilename        bool          `yaml:"titlecase_Filename"`
	RemoveCharacters         string        `yaml:"removecharac_Filename"`
	PerformerSplitchar       string        `yaml:"performer_splitchar"`
	PerformerLimit           int           `yaml:"performer_limit"`
	PerformerLimitKeep       bool          `yaml:"performer_limit_keep"`
	PerformerSort            string        `yaml:"performer_sort"`
	PerformerIgnoreGender    []string      `yaml:"performer_ignoreGender"`
	DuplicateSuffix          []string      `yaml:"duplicate_suffix"`
	PreventTitlePerformer    bool          `yaml:"prevent_title_performer"`
	PreventConsecutive       bool          `yaml:"prevent_consecutive"`
	RemoveEmptyFolder        bool          `yaml:"remove_emptyfolder"`
	PathOnePerformer         bool          `yaml:"path_one_performer"`
	PathNoPerformerFolder    bool          `yaml:"path_noperformer_folder"`
	PathKeepAlreadyPerformer bool          `yaml:"path_keep_alrperf"`
	PrepositionsList         []string      `yaml:"prepositions_list"`
	PrepositionsRemoval      bool          `yaml:"prepositions_removal"`
	SqueezeStudioNames       bool          `yaml:"squeeze_studio_names"`
	RatingFormat             string        `yaml:"rating_format"`
	TagsSplitchar            string        `yaml:"tags_splitchar"`
	TagsWhitelist            []string      `yaml:"tags_whitelist"`
	TagsBlacklist            []string      `yaml:"tags_blacklist"`
	IgnorePathLength         bool          `yaml:"ignore_path_length"`
	OrderField               []string      `yaml:"order_field"`
}

// LoadConfig reads a renamer.yml. Unknown keys are an error, so a typo does
// not silently change file names.
func LoadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Config
	if err := yaml.UnmarshalStrict(data, &c); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &c, nil
}

type pair struct{ key, value string }

type replaceWord struct {
	mode string // regex, word, any
	old  string
	new  string
	re   *regexp.Regexp
}

// Renamer computes target paths. It is safe for concurrent use.
type Renamer struct {
	c Config

	tagTemplates   []pair
	pTagTemplates  []pair
	pPathTemplates []pair
	fieldReplacers map[string][]Replacer
	replaceWords   []replaceWord
	removeChars    string
	// orderField starts with "", the plugin's leading None: compute once
	// with all fields before removing any
	orderField   []string
	prepositions []*regexp.Regexp

	reMu    sync.Mutex
	reCache map[string]*regexp.Regexp
}

// New validates c and prepares the templates.
func New(c Config) (*Renamer, error) {
	r := &Renamer{c: c, reCache: make(map[string]*regexp.Regexp)}

	var err error
	if r.tagTemplates, err = stringPairs(c.TagTemplates); err != nil {
		return nil, fmt.Errorf("tag_templates: %w", err)
	}
	if r.pTagTemplates, err = stringPairs(c.PTagTemplates); err != nil {
		return nil, fmt.Errorf("p_tag_templates: %w", err)
	}
	if r.pPathTemplates, err = stringPairs(c.PPathTemplates); err != nil {
		return nil, fmt.Errorf("p_path_templates: %w", err)
	}

	r.fieldReplacers = make(map[string][]Replacer)
	for _, item := range c.FieldMultiReplacer {
		key, ok := item.Key.(string)
		if !ok {
			return nil, fmt.Errorf("field_multi_replacer: key %v is not a string", item.Key)
		}
		var list []Replacer
		if err := reDecode(item.Value, &list); err != nil {
			return nil, fmt.Errorf("field_multi_replacer %s: %w", key, err)
		}
		for _, rep := range list {
			if rep.Replace == "" {
				return nil, fmt.Errorf("field_multi_replacer %s: empty replace", key)
			}
			// Python treats backslashes in the replacement as escapes
			if strings.Contains(rep.With, `\`) {
				return nil, fmt.Errorf("field_multi_replacer %s: backslashes in with are not supported", key)
			}
		}
		r.fieldReplacers[key] = list
	}

	for _, item := range c.ReplaceWords {
		old, ok := item.Key.(string)
		if !ok {
			return nil, fmt.Errorf("replace_words: key %v is not a string", item.Key)
		}
		var spec []string
		if s, isString := item.Value.(string); isString {
			spec = []string{s}
		} else if err := reDecode(item.Value, &spec); err != nil {
			return nil, fmt.Errorf("replace_words %s: %w", old, err)
		}
		if len(spec) == 0 {
			return nil, fmt.Errorf("replace_words %s: no replacement", old)
		}
		w := replaceWord{mode: "word", old: old, new: spec[0]}
		if len(spec) > 1 {
			w.mode = spec[1]
		}
		if strings.Contains(w.new, `\`) {
			return nil, fmt.Errorf("replace_words %s: backslashes in the replacement are not supported", old)
		}
		switch w.mode {
		case "regex":
			w.re, err = regexp.Compile(old)
		case "word":
			w.re, err = regexp.Compile(`([` + pySpaceClass + `_-])(` + old + `)([` + pySpaceClass + `_-])`)
		case "any":
		default:
			// the plugin silently reuses the previous result; refuse instead
			err = fmt.Errorf("unknown mode %q", w.mode)
		}
		if err != nil {
			return nil, fmt.Errorf("replace_words %s: %w", old, err)
		}
		r.replaceWords = append(r.replaceWords, w)
	}

	r.removeChars = c.RemoveCharacters
	r.orderField = append([]string{""}, c.OrderField...)

	if c.PrepositionsRemoval {
		for _, word := range c.PrepositionsList {
			re, err := regexp.Compile(`^` + word + `[` + pySpaceClass + `_-]`)
			if err != nil {
				return nil, fmt.Errorf("prepositions_list %s: %w", word, err)
			}
			r.prepositions = append(r.prepositions, re)
		}
	}

	if c.PerformerSplitchar == "" {
		return nil, errors.New("performer_splitchar must not be empty")
	}
	if len(c.DuplicateSuffix) == 0 {
		return nil, errors.New("duplicate_suffix must have at least one entry (the first is used for the first file)")
	}
	if c.DateFormat != "" {
		if _, err := strftime(c.DateFormat, epoch); err != nil {
			return nil, fmt.Errorf("date_format: %w", err)
		}
	}
	if c.DurationFormat != "" {
		if _, err := strftime(c.DurationFormat, epoch); err != nil {
			return nil, fmt.Errorf("duration_format: %w", err)
		}
	}
	if _, err := pyFormat(c.RatingFormat, 0); err != nil {
		return nil, fmt.Errorf("rating_format: %w", err)
	}
	for _, opts := range c.PTagOption {
		for _, o := range opts {
			if o != "clean_tag" && o != "dry_run" && o != "inverse_performer" {
				return nil, fmt.Errorf("p_tag_option: unknown option %q", o)
			}
		}
	}

	return r, nil
}

// Config returns the configuration r was made from.
func (r *Renamer) Config() Config {
	return r.c
}

func stringPairs(ms yaml.MapSlice) ([]pair, error) {
	var ret []pair
	for _, item := range ms {
		k, ok1 := item.Key.(string)
		v, ok2 := item.Value.(string)
		if !ok1 || !ok2 {
			return nil, fmt.Errorf("%v: %v is not a string mapping", item.Key, item.Value)
		}
		ret = append(ret, pair{k, v})
	}
	return ret, nil
}

// reDecode converts a generically decoded YAML value into out.
func reDecode(v interface{}, out interface{}) error {
	data, err := yaml.Marshal(v)
	if err != nil {
		return err
	}
	return yaml.UnmarshalStrict(data, out)
}

// literalIgnoreCase returns a compiled case-insensitive literal pattern.
func (r *Renamer) literalIgnoreCase(s string) *regexp.Regexp {
	r.reMu.Lock()
	defer r.reMu.Unlock()
	if re, ok := r.reCache[s]; ok {
		return re
	}
	if len(r.reCache) > 4096 {
		r.reCache = make(map[string]*regexp.Regexp)
	}
	re := regexp.MustCompile(`(?i)` + regexp.QuoteMeta(s))
	r.reCache[s] = re
	return re
}
