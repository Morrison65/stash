# Rename on update

The API renames and moves scene files when a scene is updated
(`pkg/renamer`, `internal/manager/renamer.go`). It replaces the
renamerOnUpdate community plugin (v2.5.0 with this installation's patches)
and produces the same file and folder names from the same configuration.

## Why not the plugin

The plugin ran as a Python process per scene update, inside the update, read
the scene again over GraphQL, moved the file inside the container, and wrote
the database directly with one SQLite transaction per file. A bulk edit of
100 scenes started 100 processes and 100 write transactions. In the
microservice stack it cannot work at all: the media library is on Windows
and only stash-fsd can move files.

## How it works

1. **Listen.** `plugin.Cache` notifies built-in listeners of every post hook
   event. The renamer records the scene id of `Scene.Update.Post` events in a
   set and returns: no database access in the update. Updates of fields that
   cannot change a name (custom fields, play counts, details, ...) are
   dropped there.
2. **Batch.** A worker waits 2 s after the first event (a bulk edit arrives
   as one batch), then takes up to 200 scenes. While a scan or clean job runs
   it waits: a scan walking a folder between the rename and the database
   update would see the file as new.
3. **Compute.** One read transaction loads the batch (scenes, files, tags with
   aliases, performers, studio chain, groups) and computes the targets. Only
   organized scenes with a title are considered; unchanged files are dropped.
   A target that already exists in the database is an error, unless the
   current name is the target with a duplicate suffix (`x_HD-001.mp4`, named
   by the plugin when the target was taken): that file keeps its name.
4. **Move.** stash-fsd renames the files one after the other: renames on the
   same drive first, copies to another drive (move tags) after them. A file
   that cannot be renamed (in use, target exists) is logged and left alone;
   the next update of the scene tries again. Associated files
   (`associated_extension`, and captions Stash knows such as `video.en.srt`)
   follow the video. An emptied folder is removed (never a library root).
5. **Record.** Successful moves are written in one write transaction per
   batch (at most 50, and at once after a copy to another drive): folder
   hierarchy, file folder and name, caption names, and removal of the move
   tag (`clean_tag`). If that transaction fails, the files are moved back.

A file that was moved but not recorded (API stopped in between) is found on
the next update of its scene: the source is missing and the target exists,
so only the database is updated. (This assumes the file at the target is the moved one; a file
put there by hand and not yet scanned would be taken for it.)

stash-fsd's `Rename` copies only when the rename fails because the target is
on another volume. The copy is written to `<target>.partial` and renamed into
place when complete. Any other failure, such as a file in use, is returned
at once.

## Configuration

`renamer.yml` next to the config file (`/root/.stash/renamer.yml`;
`STASH_RENAMER_CONFIG` overrides it). Without the file, or with
`enabled: false`, nothing is renamed. The keys are those of the plugin's
`config.py`; unknown keys are an error. Paths are canonical library paths
(`U:/Videos/$performer`, `UNC:/server/share/...`). Changes apply after an API
restart. `dry_run: true` only logs what would be renamed.

`docker-workspace/make-stash-microservices-config.py` disables the
renamerOnUpdate plugin in the microservice config when `renamer.yml` exists.

## Differences to the plugin

| Plugin | Here | Why |
| --- | --- | --- |
| Organized scenes are renamed, with or without a title | Organized scenes **with a title** | requested |
| A taken target gets `-001`, `-002`, ... | Error; a file already named with a suffix keeps it | requested |
| Tag order of the name is Python's set order, which changes between runs | Order of the current name if it has the same tags, else `tags_whitelist` order | no renames that only reorder tags |
| Folders are compared before characters are removed: a performer `Kim O'Hara` in folder `Kim-OHara` gets `Kim-OHara/Kim-OHara`, one level deeper on every update | Compared after removing characters | the folder stays |
| `/data/...` container paths; the 240-character limit counted them | Canonical Windows paths; the limit counts the Windows path | the real path length |
| `$oshash`/`$checksum` were always empty (fingerprints were read with the wrong keys) | Filled | not used by the current templates |

The plugin's `^*` branch in `create_new_path` that pops a folder is dead code
(the placeholder is replaced before the check) and is not ported.

Unsupported plugin options: `use_ascii` (unidecode), `p_non_organized`,
`filename_as_title`, regular-expression replacements with backslashes in the
replacement. `replace_words` patterns are Go regular expressions; for the
patterns in use they behave like Python's.

## Verification: golden test

The engine is compared with the plugin's own code over a real library:

```sh
# 1. dump organized scenes and studios (read-only GraphQL)
python scripts/dev/renamer-golden/dump.py <dir>/data.json
# 2. /data -> Windows maps of the classic stack
python docker-workspace/make-stash-microservices-config.py --output NUL --print-maps > <dir>/maps.txt
# 3. the plugin's results, in an offline container with its Python
docker run --rm --network none -e PYTHONHASHSEED=0 -v <dir>:/w \
  -v <stash-home>/plugins/community/renamerOnUpdate:/plugin:ro --entrypoint python \
  stashms/api-plugins:dev /w/golden.py /plugin /w/data.json /w/maps.txt /w/golden.txt
# 4. the engine against it (renamer.yml copied into <dir>)
DEV_MOUNT=<dir> RENAMER_GOLDEN_DIR=/mnt/dev scripts/dev/gotest.sh ./pkg/renamer/ -run TestGolden -v
```

(`golden.py` must be copied into `<dir>` for step 3.) `golden.py` runs the
plugin unchanged in hook mode with its side effects stubbed; library paths
are mapped back to the `/data` paths the plugin saw, and its results forward
again. The test runs the engine with the plugin's behaviour
(`Options{SortTags, LegacyNesting}`) and must match every file; it also logs
what the production options would change. The data contains media titles:
keep it out of the repository.

Result on 2026-10-24 (5,916 organized scenes, 5,947 files): all 5,817 files
of scenes with a title matched (440 moves, 5,377 unchanged); 130 scenes
without a title are skipped by design. With the production options, 164 files
would be renamed or moved on their next update (the plugin: 492, mostly tag
reordering).
