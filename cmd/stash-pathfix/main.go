// Command stash-pathfix converts the library paths stored in a Stash SQLite
// database to the canonical form used by the container deployment:
// forward-slash Windows paths ("U:/Media/x"), drive roots as "U:" and UNC
// paths as "UNC:/server/share/x" (see docs/architecture/README.md).
//
// It accepts databases from upstream Stash on Windows (backslash paths) and
// from Docker setups (Unix paths, mapped with --map):
//
//	stash-pathfix --db stash-go.sqlite                          # dry run
//	stash-pathfix --db stash-go.sqlite --map /data=U:/Media --apply
//
// Only folder paths are stored in full (files store a basename and their
// parent folder), so rewriting the folders table moves every file. Always
// keep a backup; without --apply the tool only reports what it would change.
package main

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path"
	"sort"
	"strings"

	_ "github.com/mattn/go-sqlite3"
	"github.com/spf13/pflag"

	"github.com/stashapp/stash/pkg/file"
)

type mapping struct{ from, to string }

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run() error {
	dbPath := pflag.String("db", "", "path to stash-go.sqlite")
	apply := pflag.Bool("apply", false, "write the changes (default: dry run)")
	verbose := pflag.BoolP("verbose", "v", false, "print every changed path")
	maps := pflag.StringArray("map", nil, "prefix mapping from=to applied before normalisation (repeatable), e.g. /data=U:/Media")
	pflag.Parse()

	if *dbPath == "" {
		pflag.Usage()
		return errors.New("--db is required")
	}

	var mappings []mapping
	for _, m := range *maps {
		from, to, ok := strings.Cut(m, "=")
		if !ok || from == "" || to == "" {
			return fmt.Errorf("invalid --map %q, expected from=to", m)
		}
		mappings = append(mappings, mapping{from: strings.TrimSuffix(from, "/"), to: strings.TrimSuffix(to, "/")})
	}
	// longest prefix first
	sort.Slice(mappings, func(i, j int) bool { return len(mappings[i].from) > len(mappings[j].from) })

	if _, err := os.Stat(*dbPath); err != nil {
		return err
	}

	db, err := sql.Open("sqlite3", "file:"+*dbPath+"?_fk=true")
	if err != nil {
		return err
	}
	defer db.Close()

	rows, err := db.Query("SELECT id, path FROM folders")
	if err != nil {
		return fmt.Errorf("reading folders: %w", err)
	}

	type change struct {
		id               int64
		oldPath, newPath string
	}
	var changes []change
	existing := make(map[string]int64)
	unmapped := 0

	for rows.Next() {
		var id int64
		var p string
		if err := rows.Scan(&id, &p); err != nil {
			rows.Close()
			return err
		}
		existing[p] = id

		np := convert(p, mappings)
		if !file.IsWindowsPath(np) {
			unmapped++
		}
		if np != p {
			changes = append(changes, change{id: id, oldPath: p, newPath: np})
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	// detect collisions (e.g. "u:/x" and "U:/x" both present)
	seen := make(map[string]int64)
	for p, id := range existing {
		np := convert(p, mappings)
		if other, ok := seen[np]; ok {
			return fmt.Errorf("folders %d and %d would both become %q; resolve manually first", other, id, np)
		}
		seen[np] = id
	}

	fmt.Printf("folders: %d total, %d to change, %d not a Windows path after conversion\n", len(existing), len(changes), unmapped)
	for i, c := range changes {
		if *verbose || i < 10 {
			fmt.Printf("  %q -> %q\n", c.oldPath, c.newPath)
		}
	}
	if !*verbose && len(changes) > 10 {
		fmt.Printf("  ... (%d more, use -v)\n", len(changes)-10)
	}
	if unmapped > 0 {
		fmt.Println("warning: some folders are not Windows paths; add --map rules for them, stash-fsd cannot serve them")
	}

	var filterHits int
	if err := db.QueryRow(`SELECT count(*) FROM saved_filters WHERE object_filter LIKE '%:\%' OR object_filter LIKE '%/data/%'`).Scan(&filterHits); err == nil && filterHits > 0 {
		fmt.Printf("note: %d saved filter(s) look like they contain paths; review them in the UI after the switch\n", filterHits)
	}

	if !*apply {
		fmt.Println("dry run: nothing written (use --apply)")
		return nil
	}

	tx, err := db.Begin()
	if err != nil {
		return err
	}
	stmt, err := tx.Prepare("UPDATE folders SET path = ?, basename = ? WHERE id = ?")
	if err != nil {
		_ = tx.Rollback()
		return err
	}
	for _, c := range changes {
		if _, err := stmt.Exec(c.newPath, basename(c.newPath), c.id); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("updating folder %d: %w", c.id, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}

	fmt.Printf("updated %d folders\n", len(changes))
	fmt.Println("remember to update the library paths (stash: ...) in config.yml to the same form")
	return nil
}

// convert applies the prefix mappings and normalises Windows paths.
func convert(p string, mappings []mapping) string {
	for _, m := range mappings {
		if p == m.from || strings.HasPrefix(p, m.from+"/") || strings.HasPrefix(p, m.from+`\`) {
			p = m.to + p[len(m.from):]
			break
		}
	}
	if file.IsWindowsPath(p) {
		return file.NormalizeLibraryPath(p)
	}
	return p
}

// basename mirrors filepath.Base on Linux for the canonical form.
func basename(p string) string {
	return path.Base(p)
}
