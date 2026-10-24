# stash-fsd — Windows filesystem service

`stash-fsd.exe` is a Rust program that runs on the Windows host (as a Windows
service or in a console) and is the only component that touches the media
library. Source: `windows/stash-fsd/`. Build: `scripts/dev/fsd-build.sh`
(cross-compiled in Docker to `x86_64-pc-windows-gnu`, output in
`windows/stash-fsd/dist/`).

## Paths on the wire

Library paths are Windows paths written with forward slashes. Every path the
service returns (`FileInfo.path`, `EvalSymlinks`, `MoveToTrash`, ...) is in
this canonical form:

| Kind | Canonical wire form | Native |
|---|---|---|
| drive path | `U:/Media/clip.mp4` (upper-case letter) | `U:\Media\clip.mp4` |
| drive root | `U:` | `U:\` |
| UNC path | `UNC:/nas/share/dir/clip.mp4` | `\\nas\share\dir\clip.mp4` |
| UNC share root | `UNC:/nas/share` | `\\nas\share` |

`UNC:` is a pseudo drive: Go's `filepath.Clean` on Linux would collapse a
leading `//` into `/` and break UNC paths, while `UNC:/nas/share/x` survives
`Clean`/`Join`/`Dir` unchanged. `U:` is what `filepath.Clean("U:/")` yields.

Input is lenient. The service accepts `U:/`, `u:`, backslashes, `unc:` in any
case, `//nas/share/...` and `\\nas\share\...`. The same forms work for `roots`
in `stash-fsd.toml` and for the base64url `p` parameter of `/raw`. Paths are
cleaned lexically. A `..` that climbs above the volume (drive or
`server/share`), or any path outside the roots, returns `PERMISSION_DENIED`;
relative paths do too. A path that is rooted but has no drive (`/x`) is not a
Windows path and returns `INVALID_ARGUMENT`.

## Interfaces

1. **gRPC** (`grpc_listen`, default `0.0.0.0:7700`) — the contract in
   `proto/stashfs/v1/stashfs.proto`: filesystem, hashing, ffmpeg.
2. **HTTP** (`http_listen`, default `0.0.0.0:7701`) — signed raw file
   serving used for playback (nginx proxies to it after the API authorises a
   request) and later DLNA.

Both require the shared secret: gRPC as `authorization: Bearer <secret>`
metadata, HTTP through an HMAC signature.

### HTTP raw endpoint

```
GET|HEAD /raw?p=<path>&z=<zip entry>&e=<expiry>&s=<signature>
```

| Param | Meaning |
|---|---|
| `p` | wire path (canonical form, see above; drive or `UNC:`), base64url (no padding) encoded |
| `z` | optional zip entry inside `p`, base64url encoded (empty/missing = none) |
| `e` | expiry, unix seconds |
| `s` | lower-case hex `HMAC-SHA256(secret, "raw\n" + p + "\n" + z + "\n" + e)`, where `p`/`z`/`e` are the *encoded* query values exactly as sent |

Behaviour: `403` on bad/expired signature or path outside roots, `404` if
missing; `Range` (single range) → `206`, `Accept-Ranges: bytes`,
`Content-Type` from the extension, `Last-Modified`, `ETag` (`"<size>-<mtime>"`),
`If-None-Match`/`If-Modified-Since` → `304`. Zip entries are served without
range support for compressed entries (stored entries support ranges).

`GET /healthz` → `200 ok` (no auth).

## Configuration — `stash-fsd.toml`

Looked up at `--config <path>`, else next to the executable, else
`%ProgramData%\stash-fsd\stash-fsd.toml`.

```toml
[server]
grpc_listen   = "0.0.0.0:7700"
http_listen   = "0.0.0.0:7701"
shared_secret = "change-me-to-a-long-random-string"

[access]
# Only paths below these roots are accessible (forward or back slashes,
# drive or UNC; UNC may be written \\server\share, //server/share or UNC:/server/share).
roots = ["U:/", "D:/Media", '\\LIL_NAS_X\MainStoragePool\StashLib']

[tools]
ffmpeg  = "C:/ffmpeg/bin/ffmpeg.exe"   # default: ffmpeg on PATH
ffprobe = "C:/ffmpeg/bin/ffprobe.exe"  # default: ffprobe on PATH
python  = ""                           # reserved for Windows-side helpers

[storage]
workspace_dir = "C:/ProgramData/stash-fsd/work"
log_dir       = "C:/ProgramData/stash-fsd/logs"

[limits]
max_concurrent_ffmpeg   = 4
max_concurrent_hash     = 8
ffmpeg_timeout_seconds  = 3600
walk_batch_size         = 512

[log]
level = "info"   # error|warn|info|debug|trace
```

## Running

```
stash-fsd.exe run --config C:\stash-fsd\stash-fsd.toml      # console
stash-fsd.exe install --config C:\stash-fsd\stash-fsd.toml  # register service (admin)
stash-fsd.exe uninstall
stash-fsd.exe print-config                                   # effective config
```

The service runs as LocalSystem, which cannot see drive letters mapped by
a user. Prefer UNC roots for NAS storage: they avoid drive-mapping problems
entirely. The share must grant access to the machine account (`DOMAIN\HOST$`).
On a NAS without domain membership, run the service as a user account that
has access to the share (`sc.exe config stash-fsd obj= .\user password= ...`).

Docker Desktop containers reach the host as `host.docker.internal`; allow
inbound TCP 7700/7701 for the WSL/Hyper-V virtual network in Windows
Firewall:

```powershell
New-NetFirewallRule -DisplayName "stash-fsd" -Direction Inbound -Protocol TCP `
  -LocalPort 7700,7701 -Action Allow -Profile Private
```

## Concurrency

* Hashing jobs are bounded by `max_concurrent_hash`, ffmpeg processes by
  `max_concurrent_ffmpeg`. `PRIORITY_INTERACTIVE` runs bypass the queue
  ordering (they wait for a free slot before background work does).
* Filesystem metadata calls are executed on a blocking thread pool so a slow
  network share never stalls the async runtime.
* `Walk` streams batches as soon as they are read; large libraries do not
  need to be buffered in memory.
