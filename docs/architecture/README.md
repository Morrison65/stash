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
                        │  ├─ /            UI static build (+ service worker)
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
     │  valkey 8         ◄── api (cache, pub/sub)      │
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
| `valkey` | docker | Valkey 8 | Global cache (settings, read-query results), pub/sub for job progress, cache invalidation and subscriptions across `api`/`worker`. |
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
* Every job is a row in PostgreSQL (`kind`, JSON `input`, priority, status,
  progress). Executors are registered per kind; the GraphQL inputs are the
  payloads. The GraphQL `Job` type, `jobQueue`, `findJob`, `jobsSubscribe`,
  `stopJob` are byte-for-byte compatible.
* Long-running mutations that used to block the request (backup, anonymise,
  stash-box fingerprint submission, plugin hooks) are queued.
* **Plugin hooks are asynchronous by default.** A plugin can opt a trigger
  back into synchronous execution by prefixing it with `sync:` in its YAML,
  e.g. `triggeredBy: [sync:Scene.Update.Post]`.

### Configuration
The single `config.yml` is split by consumer:

| Part | Stored in | Consumers |
|---|---|---|
| System (paths, DB, cache, fsd address, secrets) | environment / compose `.env` | containers at start |
| Settings (general, interface, scraping, dlna, defaults, stash-boxes, library paths) | PostgreSQL `settings`, cached in-process + Valkey, invalidated via pub/sub | GraphQL `configuration`, plugins, all services |
| Plugin registry (installed/enabled plugins, package sources, per-plugin settings) | PostgreSQL | scheduler + api; plugins only see their own settings through GraphQL as before |
| UI config (`configuration.ui`, incl. tagger `fingerprintQueue`) | browser IndexedDB via service worker | browser only; pushed to the server only via **Settings → Tools → Troubleshooting → Sync UI configuration to server** |
| Windows service | `stash-fsd.toml` on Windows | stash-fsd |

### Caching
* Settings: in-process snapshot (zero I/O per request) + Valkey, version
  bumped on every write.
* Read query cache: identical read-only GraphQL operations (same query,
  variables, user) are coalesced (singleflight) and cached in Valkey keyed by
  a global data generation counter that is incremented on every committed
  write — correct by construction, and it collapses the "same request three
  times" pattern caused by plugins.

## Repository layout (new parts)

```
proto/stashfs/v1/        gRPC contract between Go and the Windows service
windows/stash-fsd/       Rust Windows service (cross-compiled in Docker)
pkg/remotefs/            Go client for stash-fsd (models.FS implementation)
pkg/sqlite/              data layer, ported to PostgreSQL (renamed at the end)
deploy/                  docker compose stack, nginx gateway, Dockerfiles
scripts/dev/             containerised toolchain wrappers (no host installs)
docs/architecture/       this document and per-component notes
```

## Phases

| Phase | Content | Gate |
|---|---|---|
| P0 | Toolchain harness, this document, proto contract | `scripts/dev/build.sh` passes |
| P1 | PostgreSQL port of the data layer (**deferred**, SQLite kept for now) | existing `integration` test suite passes against PostgreSQL |
| P2 | `stash-fsd` filesystem + hashing, `pkg/remotefs`, scanner on top of it | library scan from Docker |
| P3 | ffmpeg on Windows (probe, screenshots, previews, sprites, phash, markers), direct streaming via gateway | generate + play |
| P4 | Durable job queue + worker/scheduler, async hooks | jobs API unchanged, hooks async |
| P5 | Config split, settings cache, read-query cache, api/worker role split | compose stack up |
| P6 | UI: IndexedDB + service worker for UI config, troubleshooting sync button | UI works with server-side `ui` untouched |
| P7 | `import-sqlite` migration tool with path rewriting | import of a real DB |

## Development

No toolchains are needed on the host; everything runs in Docker:

```
scripts/dev/build.sh                       # generate + build + vet all Go code
scripts/dev/gotest.sh ./pkg/...            # Go unit tests
scripts/dev/pg-test.sh                     # data-layer integration tests on PostgreSQL
scripts/dev/fsd-build.sh                   # cross-compile stash-fsd.exe
```
