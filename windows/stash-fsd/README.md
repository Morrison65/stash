# stash-fsd

Windows service that owns every access to the Stash media library. The Go
containers talk to it over gRPC (`proto/stashfs/v1/stashfs.proto`); nginx
streams raw media from its signed HTTP endpoint. Design notes:
[`docs/architecture/stash-fsd.md`](../../docs/architecture/stash-fsd.md).

## Build

Nothing needs to be installed on the host; everything runs in Docker.

```sh
scripts/dev/fsd-build.sh            # cargo test (Linux) + cross-compile -> windows/stash-fsd/dist/stash-fsd.exe
scripts/dev/fsd-build.sh test walk  # any cargo command inside the toolchain container
```

The toolchain image `stash-fsd-builder:latest` is built from
`Dockerfile.build` on first use (`FSD_REBUILD_IMAGE=1` forces a rebuild).
Cargo caches live in the `stashms-cargo-registry`, `stashms-cargo-git` and
`stashms-fsd-target` volumes. The exe targets `x86_64-pc-windows-gnu` with a
static C runtime and only imports DLLs that ship with Windows.

## Install

1. Copy `dist/stash-fsd.exe` to e.g. `C:\stash-fsd\`.
2. Copy `stash-fsd.example.toml` to `C:\stash-fsd\stash-fsd.toml` (or
   `%ProgramData%\stash-fsd\stash-fsd.toml`) and edit it. At minimum set
   `shared_secret` (the service refuses to start with an empty or `change-me…`
   secret) and `roots`.
3. Try it in a console first:

   ```powershell
   C:\stash-fsd\stash-fsd.exe print-config --config C:\stash-fsd\stash-fsd.toml
   C:\stash-fsd\stash-fsd.exe run --config C:\stash-fsd\stash-fsd.toml
   ```

4. Register the service from an elevated prompt (automatic start, restarts on
   failure, runs as LocalSystem):

   ```powershell
   C:\stash-fsd\stash-fsd.exe install --config C:\stash-fsd\stash-fsd.toml
   sc.exe start stash-fsd
   # later: C:\stash-fsd\stash-fsd.exe uninstall
   ```

LocalSystem does not see drive letters that a user mapped to network shares.
Use local drives, UNC roots (`//nas/share/media`), or change the service
account (`sc.exe config stash-fsd obj= .\user password= ...`).

### Firewall

Docker Desktop containers reach the host as `host.docker.internal`. If the
service listens on `0.0.0.0`, allow the ports for the private profile:

```powershell
New-NetFirewallRule -DisplayName "stash-fsd" -Direction Inbound -Protocol TCP `
  -LocalPort 7700,7701 -Action Allow -Profile Private
```

On Docker Desktop (WSL 2 backend), connections to `host.docker.internal`
are also forwarded to services bound to `127.0.0.1`. Binding to loopback
keeps the ports off the LAN and does not need a firewall rule.

## Configuration

See `stash-fsd.example.toml`. Lookup order: `--config`, then
`stash-fsd.toml` next to the executable, then
`%ProgramData%\stash-fsd\stash-fsd.toml`. `RUST_LOG` overrides `[log] level`.
Logs go to the console (`run`) and to a daily rolling file `stash-fsd.log.*`
in `log_dir`.

| Key | Default | Notes |
|---|---|---|
| `server.grpc_listen` | `0.0.0.0:7700` | gRPC, `authorization: Bearer <secret>` |
| `server.http_listen` | `0.0.0.0:7701` | `/healthz`, signed `/raw` |
| `server.shared_secret` | — | required, ≥ 16 chars |
| `access.roots` | — | required; case-insensitive prefix match on path components |
| `tools.ffmpeg` / `tools.ffprobe` | on `PATH` | |
| `storage.workspace_dir` | `%ProgramData%\stash-fsd\work` | ffmpeg workspaces; abandoned ones are removed after 24 h |
| `storage.log_dir` | `%ProgramData%\stash-fsd\logs` | |
| `limits.max_concurrent_ffmpeg` | 4 | `Probe` and `Run`; interactive requests are served first |
| `limits.max_concurrent_hash` | 8 | `Fingerprint` |
| `limits.ffmpeg_timeout_seconds` | 3600 | default `Run`/`Probe` timeout |
| `limits.walk_batch_size` | 512 | default `Walk` batch size |

## Debugging helpers

```powershell
stash-fsd.exe sign-raw --config C:\stash-fsd\stash-fsd.toml --path U:/Media/clip.mp4   # signed /raw query
stash-fsd.exe hash U:\Media\clip.mp4                                                   # md5 + oshash
stash-fsd.exe hash U:\Gallery\set.zip --zip img/001.jpg
```

`cmd/fsd-smoke` (Go) runs the gRPC API end to end against a running instance
and checks fingerprints against `pkg/hash`:

```sh
scripts/dev/go.sh go run ./cmd/fsd-smoke -addr host.docker.internal:7700 \
  -secret <secret> -root C:/temp/fsd-scratch
```

## Behaviour notes

* Paths on the wire use forward slashes (`U:/Media/x.mp4`, `//nas/share/x`).
  Backslashes are accepted. `U:` means `U:/` because Go's `filepath.Dir` on
  Linux produces it. A single leading slash (`/nas/share/x`) is read as UNC
  because Go's `filepath.Clean` on Linux collapses `//`.
* Paths are cleaned lexically. A `..` that climbs above the volume, or any
  path outside the roots, returns `PERMISSION_DENIED`. Symlinks and junctions
  inside a root are followed, even when their target is outside the roots.
* `Rename`, `Remove`, `RemoveAll` and `MoveToTrash` refuse to act on a
  configured root itself. `Rename` refuses an existing destination
  (`ALREADY_EXISTS`), except for a case-only rename of the same entry.
* `ReadAt` returns up to 8 MiB, which is more than gRPC-Go's default 4 MiB
  receive limit, so clients must set `grpc.MaxCallRecvMsgSize`.
* `Run`: a tool that fails to start is reported as a gRPC error
  (`FAILED_PRECONDITION`), not as a `RunStatus`. If the client disconnects,
  the process is killed. The process runs with the workspace as its working
  directory.
* `ListWorkspace` on a workspace that does not exist returns an empty list;
  `DeleteWorkspace` on a missing workspace succeeds.
* HTTP `ETag` is `"<size>-<mtime in unix nanoseconds>"`. For zip entries it
  uses the entry size and the archive's mtime.
