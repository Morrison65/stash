# Stash microservice architecture (fork)

This fork splits Stash into a set of Linux containers that own all business
logic, plus one native Windows service that owns every access to the media
library. The GraphQL API, the data model (`pkg/models`) and the plugin /
stash-box contracts are **unchanged**, so existing plugins, scrapers, UI
plugins and stash-box integrations keep working.

Base commit: upstream `4de2351` (branch `microservices`).

## Topology

```
 Browser ──HTTP(S)──► gateway (nginx)
                        │  ├─ /            UI static build
                        │  ├─ /graphql     → api   (websocket upgrade)
                        │  ├─ /scene/... /image/... → api
                        │  │       api answers media requests with
                        │  │       X-Accel-Redirect → /_fsd/raw?...signed
                        │  └─ /_fsd/ (internal) ──HTTP range──► stash-fsd (Windows)
                        │
     ┌──────────────────┼─────────────────────────────┐ docker compose
     │  api (Go)         ── gRPC ──►  stash-fsd        │
     │   └ scheduler     (own worker container later)  │
     │  SQLite volume    ◄── api only (PostgreSQL later)│
     │  (valkey 8: added with PostgreSQL + worker)    │
     └────────────────────────────────────────────────┘
                                      stash-fsd.exe (Rust, Windows service)
                                        ├─ gRPC  :7700  filesystem, hashing, ffmpeg
                                        └─ HTTP  :7701  signed raw file serving
```

| Component | Where | Language | Responsibility |
|---|---|---|---|
| `gateway` | docker | nginx | Serves the UI build, reverse proxies the API, streams raw media directly from `stash-fsd` via `X-Accel-Redirect`, serves generated files directly from the shared volume. |
| `api` | docker | Go (`stash serve`) | GraphQL, auth, all request handling. Never runs long work inline: enqueues jobs. Runs plugin *operations* and scrapers (Python lives in this image). While the database is SQLite it also hosts the scheduler. |
| scheduler | inside `api` (own `worker` container once on PostgreSQL) | Go | Durable job queue, priorities, per-resource concurrency limits (fsd I/O, ffmpeg, network, plugins), executes every job including async plugin hooks. Written as a separate component so it can move out. |
| database | docker volume | SQLite now, PostgreSQL 17 later | See "Database". |
| `valkey` (planned) | docker | Valkey 8 | Shared cache and pub/sub across `api`/`worker` once there are several processes (with PostgreSQL). Until then the API caches in process. |
| `stash-fsd` | Windows host | Rust | The only process that touches library files: stat/readdir/walk, random-access reads, move/rename/delete/trash, md5/oshash, ffprobe, ffmpeg (screenshots, previews, sprites, phash frames, markers), raw HTTP range serving. |

## Key decisions

### Paths
* Library paths (video, image, gallery files and their folders) are **Windows
  paths written with forward slashes**: `U:/Media/clip.mp4`. Go's
  `path/filepath` on Linux treats `U:` as an ordinary first path element, so
  `Dir`, `Base`, `Join`, `Rel` and `Clean` all work and the scanner and
  business logic need no special path library. `stash-fsd` converts to/from
  native `\` form at its boundary.
* Drive roots are written `U:` (that is what `filepath.Clean("U:/")` yields).
* UNC paths use the pseudo drive **`UNC:`**: `\\nas\share\clip.mp4` is
  stored as `UNC:/nas/share/clip.mp4`. A leading `//` cannot be used because
  `filepath.Clean`/`Join` on Linux collapse it to `/`. Input in any form
  (`\\nas\share`, `//nas/share`, `unc:/nas/share`) is normalised by
  `file.NormalizeLibraryPath`. Details: [stash-fsd.md](stash-fsd.md#paths-on-the-wire).
* Everything else in the configuration is a **Unix path inside the
  containers** (`python_path: /usr/local/bin/python`, `generated: /generated`,
  `plugins_path: /config/plugins`, ...).
* `stash-fsd` only allows access below its configured `roots`.

### Database
**Status: SQLite is kept for now** (decision 2026-10-24). SQLite allows one
writer at a time and Stash serialises writes through a single connection per
process, so exactly one process owns the database file: the `api` container,
which also hosts the scheduler. Most of the plugin-induced load is addressed
by caching, request coalescing and asynchronous hooks, which do not depend on
the database engine. The only SQLite tuning knob Stash reads is
`STASH_SQLITE_CACHE_SIZE` (WAL and `synchronous=NORMAL` are already defaults).

Databases already rewritten for native Windows Stash store backslash paths
(`U:\Media\x.mp4`, `\\NAS\share\x`); adopting one only needs `\` → `/`.

The PostgreSQL port below is **deferred** until measurements show the
database is still the bottleneck:

* PostgreSQL with a single fresh baseline schema equivalent to SQLite schema
  v85, followed by normal incremental migrations.
* SQLite-only features are replaced: `regexp` → `~*`/`~`, `phash_distance` →
  SQL function using `bit_count`, `NATURAL_CI` → ICU collation, `LIKE` →
  `ILIKE`, `last_insert_rowid` → `RETURNING id`, dynamically-typed columns
  (`files_fingerprints.fingerprint`, `*_custom_fields.value`) get proper types.
* `stash import-sqlite` copies an existing `stash-go.sqlite` (v85) into
  PostgreSQL and rewrites library path prefixes (e.g. `/data` → `U:/Media`).
* Writes are no longer serialised through one connection.

### Filesystem and media (Windows service)
* Contract: `proto/stashfs/v1/stashfs.proto`. Go client: `pkg/remotefs`,
  which implements `models.FS` (+ `io.ReaderAt` handles), so the existing scan
  pipeline, zip galleries, captions etc. work unchanged on top of it.
* Hashing that is I/O bound (md5, oshash) runs in Rust next to the file.
  Perceptual hashes keep the exact Go algorithm (stash-box compatibility):
  ffmpeg on Windows produces the frames, Go computes the hash.
* ffmpeg runs on Windows. Multi-step generation (preview chunks + concat)
  uses a server-side *workspace*; final artefacts are streamed back and stored
  in the Docker `generated` volume.
* v1 has **no live transcoding / HLS / DASH**. Scenes play by direct streaming
  of the original file (`/scene/{id}/stream*` → raw range serving). The
  transcode routes stay registered and fall back to the direct stream so the
  API surface is unchanged.
* `cmd/phasher` and `internal/desktop` are dropped (the service covers
  phashing; there is no desktop/tray mode).

### Jobs and scheduling
* **Implemented:** the job manager (`pkg/job`) schedules per *lane* instead of
  one global serial queue. A lane runs one job at a time in FIFO order; lanes
  run concurrently:

  | Lane | Jobs |
  |---|---|
  | `library` | scan, clean, generate, clean generated, migrate hash naming |
  | `metadata` | identify, auto tag, stash-box batch tagging |
  | `plugin` | plugin tasks |
  | `system` | package install/update, ffmpeg download, export |
  | `exclusive` | database migrations, optimise, import, blob migration |

  `exclusive` jobs start only when nothing else runs, and nothing queued
  behind them starts first. The GraphQL `Job` type, `jobQueue`, `findJob`,
  `jobsSubscribe`, `stopJob` are unchanged.
* **Implemented: plugin hooks are asynchronous by default.** Each plugin has
  an ordered queue (one plugin sees its events in order; plugins run in
  parallel). A plugin opts a trigger back into synchronous execution by
  prefixing it with `sync:` in its YAML, e.g.
  `triggeredBy: [sync:Scene.Update.Post]`.
* **Later (with PostgreSQL):** durable jobs (a row per job with kind, JSON
  input, priority) executed by a separate `worker` container; with SQLite a
  second process would only contend for the single writer.

### Configuration
The single `config.yml` is split by consumer:

| Part | Stored in | Consumers |
|---|---|---|
| System (container paths, database, fsd address and secret) | environment / compose `.env` (`STASH_*`) | containers at start |
| Settings (general, interface, scraping, dlna, defaults, stash-boxes, library paths, plugin settings, package sources) | `config.yml` in the `config` volume, held in memory (koanf) — reads never touch the disk | GraphQL `configuration`, plugins, scheduler |
| UI config (`configuration.ui`, incl. tagger `fingerprintQueue`) | browser IndexedDB via an Apollo link in the app's client | browser only; pushed to the server only via **Settings → Tools → Troubleshooting → Sync UI configuration to server** |
| Windows service | `stash-fsd.toml` on Windows | stash-fsd |

All container paths in the settings are Unix paths (`python_path:
/usr/bin/python3`, `plugins_path: /config/plugins`); only library paths are
Windows paths. Moving settings into a database table is planned together with
PostgreSQL, when more than one process needs them.

### Caching
* **Implemented:** identical read-only GraphQL operations (same document,
  variables, user) are coalesced (singleflight) and cached in process, keyed
  by a global data generation counter (`txn.Generation`) that is bumped after
  every committed write transaction and on configuration changes, plugin and
  scraper reloads and finished jobs. The generation is captured before
  execution, so a result that raced a write is never stored. Scrapers, jobs,
  logs, directory listings and random walls are never cached. This collapses
  the "same request three times" pattern caused by plugins, including the
  ever-repeated `configuration` query.
* With one API process an in-process cache is strictly faster than a network
  cache. Valkey becomes useful (shared cache, pub/sub invalidation, job
  progress fan-out) once there are several processes — i.e. with PostgreSQL
  and a separate worker.

## Repository layout (new parts)

```
proto/stashfs/v1/        gRPC contract between Go and the Windows service
windows/stash-fsd/       Rust Windows service (cross-compiled in Docker)
pkg/remotefs/            Go client for stash-fsd (models.FS implementation)
pkg/remotefs/localserver Go reference implementation of the contract (tests, Linux dev)
pkg/file/library.go      LibraryFS + RoutingFS: library paths -> stash-fsd, others -> local
pkg/ffmpeg/remote.go     runs ffmpeg/ffprobe with library inputs on stash-fsd
cmd/stash-pathfix/       converts an existing database to canonical library paths
cmd/fsd-smoke/           end-to-end gRPC smoke test against a running stash-fsd
deploy/                  docker compose stack, nginx gateway, Dockerfiles
scripts/dev/             containerised toolchain wrappers (no host installs)
docs/architecture/       this document and per-component notes
```

## Phases

| Phase | Content | Status |
|---|---|---|
| P0 | Toolchain harness, this document, proto contract | done |
| P1 | PostgreSQL port of the data layer | **deferred** (SQLite kept) |
| P2 | `stash-fsd` filesystem + hashing, `pkg/remotefs`, scanner on top of it | done |
| P3 | ffmpeg on Windows (probe, screenshots, previews, sprites, phash, markers), direct streaming via gateway | done (no live transcoding in v1) |
| P4 | Lane scheduler, async hooks with `sync:` opt-in | done; durable worker deferred with P1 |
| P5 | Read-query cache, config split | done (in process); Valkey deferred with P1 |
| P6 | UI: UI config in IndexedDB (Apollo link; the service worker was removed 2026-10-24), troubleshooting sync button | done |
| P7 | Path conversion of an existing database (`cmd/stash-pathfix`) | done |
| P8 | Rename on update built in, replacing the renamerOnUpdate plugin ([renamer.md](renamer.md)) | done; golden-tested against the plugin |
| — | DLNA served from Windows | open |

## Development

No toolchains are needed on the host; everything runs in Docker:

```
scripts/dev/build.sh                       # generate + build + vet all Go code
scripts/dev/gotest.sh ./pkg/...            # Go unit tests
scripts/dev/pg-test.sh                     # data-layer integration tests on PostgreSQL
scripts/dev/fsd-build.sh                   # cross-compile stash-fsd.exe
```
