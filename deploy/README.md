# Running the microservice stack

Two parts: **stash-fsd** on the Windows host (owns the media library) and the
**compose stack** in Docker Desktop (gateway + API). Architecture:
[docs/architecture/README.md](../docs/architecture/README.md).

## 1. Windows: stash-fsd

```bash
scripts/dev/fsd-build.sh          # cross-compiles windows/stash-fsd/dist/stash-fsd.exe in Docker
```

Copy `stash-fsd.exe` and `windows/stash-fsd/stash-fsd.example.toml` (renamed to
`stash-fsd.toml`) to e.g. `C:\stash-fsd\`, then edit:

* `shared_secret` — a long random string (`openssl rand -hex 32`)
* `roots` — every library root, e.g. `U:/Lib/Videos`,
  `\\LIL_NAS_X\MainStoragePool\StashLib`
* `tools.ffmpeg` / `tools.ffprobe` if they are not on `PATH`
* listen addresses: `127.0.0.1:7700/7701` is enough for Docker Desktop
  (`host.docker.internal` reaches host loopback); use `0.0.0.0` plus a firewall
  rule only if Docker runs on another machine

```powershell
C:\stash-fsd\stash-fsd.exe run --config C:\stash-fsd\stash-fsd.toml      # try it in a console
C:\stash-fsd\stash-fsd.exe install --config C:\stash-fsd\stash-fsd.toml  # then as a service (admin)
```

A service running as LocalSystem cannot see mapped drive letters; use UNC
roots for the NAS (details in `docs/architecture/stash-fsd.md`).

## 2. Docker: gateway + API

```bash
cd deploy/compose
cp .env.example .env              # set STASH_FSD_SECRET to the same secret
docker compose up -d --build
```

Open `http://localhost:9990` (port: `STASH_GATEWAY_PORT`). The service worker
that keeps the UI configuration in the browser needs a secure context:
`localhost`/`127.0.0.1` qualify; for access from other devices put TLS in
front of the gateway, otherwise the in-page fallback is used.

In **Settings → Library** add library paths as Windows paths (`U:\Lib\Videos`
or `U:/Lib/Videos`; UNC as `\\nas\share\dir`). They are stored as
`U:/Lib/Videos` and `UNC:/nas/share/dir`.

## Adopting an existing database

The API keeps SQLite at `/database/stash-go.sqlite` (volume `database`).
Convert the library paths of an existing database first (keep a backup):

```bash
# database from native Windows Stash (backslash paths)
scripts/dev/go.sh go run ./cmd/stash-pathfix --db path/to/stash-go.sqlite            # dry run
scripts/dev/go.sh go run ./cmd/stash-pathfix --db path/to/stash-go.sqlite --apply

# database from a Docker setup (/data/... paths)
scripts/dev/go.sh go run ./cmd/stash-pathfix --db path/to/stash-go.sqlite --map /data=U:/Lib --apply
```

(The database file must be inside the repository for `scripts/dev/go.sh` to
see it.) Copy the result into the `database` volume and the old `config.yml`
into the `config` volume, then update in `config.yml`: library paths
(`stash:`) to the canonical form, and container paths (`generated`, `cache`,
`blobs`, `plugins_path`, `scrapers_path`, `python_path: /usr/bin/python3`) to
the paths inside the container. The schema version must be 85 (Stash
v0.31.x).

## Plugins

* Hooks run **asynchronously**; write `sync:Scene.Update.Post` in a plugin's
  `triggeredBy` to keep the old blocking behaviour for that trigger.
* Plugins run in the API container (Python 3 with `requests`, `pyyaml`;
  install more with `docker compose exec api pip install ...`).
* Plugins see library paths as `U:/...` strings but cannot open those files
  from the container.

## Not in v1

Live transcoding / HLS / DASH (files play by direct streaming), DLNA,
PostgreSQL, separate worker container.
