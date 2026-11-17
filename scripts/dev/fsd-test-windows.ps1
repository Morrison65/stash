[CmdletBinding()]
param()

$ErrorActionPreference = 'Stop'
$root = (Resolve-Path (Join-Path $PSScriptRoot '../..')).Path
$image = if ($env:RUST_IMAGE) { $env:RUST_IMAGE } else { 'stash-fsd-builder:latest' }

docker image inspect $image *> $null
if ($LASTEXITCODE -ne 0) {
    throw "Builder image $image is missing. Run scripts/dev/fsd-build.sh once to build it."
}

# Cargo reports the exact test executable, including its hash. Keep build
# caches in the same volume as fsd-build.sh; only the test binary is copied.
$output = docker run --rm -v "${root}:/src" -v 'stashms-cargo-registry:/usr/local/cargo/registry' -v 'stashms-cargo-git:/usr/local/cargo/git' -v 'stashms-fsd-target:/target' -e 'CARGO_TARGET_DIR=/target' -w /src/windows/stash-fsd $image cargo test --locked --target x86_64-pc-windows-gnu --no-run --message-format=json
if ($LASTEXITCODE -ne 0) { throw 'Cross-compiling the Windows tests failed.' }
$artifacts = @($output | ForEach-Object { $_ | ConvertFrom-Json } | Where-Object { $_.reason -eq 'compiler-artifact' -and $_.profile.test -and $_.executable })
if ($artifacts.Count -ne 1) { throw "Expected one Windows test executable, found $($artifacts.Count)." }
$exe = $artifacts[0].executable
docker run --rm -v "${root}:/src" -v 'stashms-fsd-target:/target' $image sh -c 'mkdir -p /src/windows/stash-fsd/dist && cp "$1" /src/windows/stash-fsd/dist/stash-fsd-tests.exe' sh $exe
if ($LASTEXITCODE -ne 0) { throw 'Copying the Windows test executable failed.' }
& (Join-Path $root 'windows/stash-fsd/dist/stash-fsd-tests.exe')
if ($LASTEXITCODE -ne 0) { throw 'Windows stash-fsd tests failed.' }
