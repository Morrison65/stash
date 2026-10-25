"""Read-only access to the Stash media library for Python plugins.

In the microservice stack the media library is on Windows and only stash-fsd
reads it; Stash reports library paths as `U:/Lib/Videos/x.mp4` or
`UNC:/server/share/x.mp4`. Plugins that open such a path themselves (an .nfo
or .funscript next to a video, an image file) would fail in the container.

`install()` (run by sitecustomize.py at interpreter start) routes these
paths, and only these, through the `stash-libfs` helper (cmd/stash-ffshim):
`open()` for reading, `os.stat`/`os.lstat` (and so `os.path.exists`, `isfile`,
`isdir`, `getsize`, `getmtime`), `os.listdir`, `os.scandir` (and so `glob`),
and `os.access`. Writing is refused with EROFS. Nothing changes for other
paths, or when STASH_FSD_ADDRESS is not set.

Python 3.8+ compatible: plugin environments use several Python versions.
"""

import builtins
import errno
import functools
import io
import json
import os
import re
import subprocess
import tempfile
import time

HELPER = os.environ.get("STASH_LIBFS_HELPER", "/usr/local/bin/stash-libfs")

_LIBRARY_PATH = re.compile(r"^(?:[A-Za-z]:[/\\]|UNC:[/\\])", re.IGNORECASE)
_STAT_TTL = 5.0
# files up to this size are copied to a temporary file (seekable); larger
# ones are streamed
SPOOL_MAX = int(os.environ.get("STASH_LIBFS_SPOOL_MAX", 64 << 20))

_orig = {}
_stat_cache = {}


def is_library_path(path):
    """True for a canonical Windows library path (U:/..., UNC:/...)."""
    if isinstance(path, int):
        return False
    try:
        path = os.fspath(path)
    except TypeError:
        return False
    return isinstance(path, str) and bool(_LIBRARY_PATH.match(path))


def _norm(path):
    return os.fspath(path).replace("\\", "/")


def _helper(command, path, stdout=subprocess.PIPE):
    proc = subprocess.run(
        [HELPER, command, path], stdout=stdout, stderr=subprocess.PIPE
    )
    if proc.returncode == 2:
        raise FileNotFoundError(errno.ENOENT, os.strerror(errno.ENOENT), path)
    if proc.returncode != 0:
        message = proc.stderr.decode("utf-8", "replace").strip()
        raise OSError(errno.EIO, message or "stash-libfs failed", path)
    return proc.stdout


def _stat_result(info):
    mtime = int(info.get("mtime") or 0)
    return os.stat_result(
        (info["mode"], 0, 0, 1, 0, 0, info["size"], mtime, mtime, mtime)
    )


def library_stat(path):
    path = _norm(path)
    now = time.monotonic()
    hit = _stat_cache.get(path)
    if hit is not None and now - hit[0] < _STAT_TTL:
        if hit[1] is None:
            raise FileNotFoundError(errno.ENOENT, os.strerror(errno.ENOENT), path)
        return hit[1]
    try:
        result = _stat_result(json.loads(_helper("stat", path)))
    except FileNotFoundError:
        _stat_cache[path] = (now, None)
        raise
    _stat_cache[path] = (now, result)
    return result


def library_listdir(path):
    path = _norm(path).rstrip("/")
    entries = json.loads(_helper("ls", path))
    now = time.monotonic()
    for e in entries:
        _stat_cache[path + "/" + e["name"]] = (now, _stat_result(e))
    return path, entries


class _DirEntry(object):
    def __init__(self, parent, entry):
        self.name = entry["name"]
        self.path = parent + "/" + entry["name"]
        self._entry = entry

    def is_dir(self, follow_symlinks=True):
        return bool(self._entry["dir"])

    def is_file(self, follow_symlinks=True):
        return not self._entry["dir"]

    def is_symlink(self):
        return False

    def inode(self):
        return 0

    def stat(self, follow_symlinks=True):
        return _stat_result(self._entry)

    def __fspath__(self):
        return self.path

    def __repr__(self):
        return "<DirEntry %r>" % self.name


class _ScandirIterator(object):
    def __init__(self, entries):
        self._it = iter(entries)

    def __iter__(self):
        return self

    def __next__(self):
        return next(self._it)

    def close(self):
        self._it = iter(())

    def __enter__(self):
        return self

    def __exit__(self, *exc):
        self.close()
        return False


class _HelperStream(io.RawIOBase):
    """Reads a file as the helper streams it (not seekable)."""

    def __init__(self, path):
        super(_HelperStream, self).__init__()
        self._proc = subprocess.Popen(
            [HELPER, "cat", path], stdout=subprocess.PIPE, stderr=subprocess.DEVNULL
        )

    def readable(self):
        return True

    def readinto(self, b):
        return self._proc.stdout.readinto(b)

    def close(self):
        if not self.closed:
            if self._proc.poll() is None:
                self._proc.kill()
            self._proc.stdout.close()
            self._proc.wait()
        super(_HelperStream, self).close()


def library_open(path, mode="r", buffering=-1, encoding=None, errors=None, newline=None):
    if any(c in mode for c in "wax+"):
        raise OSError(errno.EROFS, "the media library is read-only here", path)
    st = library_stat(path)
    if st.st_mode & 0o040000:
        raise IsADirectoryError(errno.EISDIR, os.strerror(errno.EISDIR), path)
    if st.st_size > SPOOL_MAX:
        # videos: streamed, not copied first
        f = io.BufferedReader(_HelperStream(_norm(path)), 1 << 20)
    else:
        # small files (nfo, funscript, images): buffered, so seek works
        f = tempfile.TemporaryFile()
        try:
            _helper("cat", _norm(path), stdout=f)
            f.seek(0)
        except BaseException:
            f.close()
            raise
    if "b" in mode:
        return f
    return io.TextIOWrapper(f, encoding=encoding, errors=errors, newline=newline)


def _open(file, mode="r", buffering=-1, encoding=None, errors=None, newline=None, closefd=True, opener=None):
    if is_library_path(file):
        return library_open(file, mode, buffering, encoding, errors, newline)
    return _orig["open"](file, mode, buffering, encoding, errors, newline, closefd, opener)


def _stat(path, *args, **kwargs):
    if is_library_path(path):
        return library_stat(path)
    return _orig["stat"](path, *args, **kwargs)


def _lstat(path, *args, **kwargs):
    if is_library_path(path):
        return library_stat(path)
    return _orig["lstat"](path, *args, **kwargs)


def _listdir(path="."):
    if is_library_path(path):
        return [e["name"] for e in library_listdir(path)[1]]
    return _orig["listdir"](path)


def _scandir(path="."):
    if is_library_path(path):
        parent, entries = library_listdir(path)
        return _ScandirIterator([_DirEntry(parent, e) for e in entries])
    return _orig["scandir"](path)


def _access(path, mode, *args, **kwargs):
    if is_library_path(path):
        if mode & os.W_OK:
            return False
        try:
            library_stat(path)
        except OSError:
            return False
        return True
    return _orig["access"](path, mode, *args, **kwargs)


class _Unbound(object):
    """Calls fn, and like the builtins it replaces it does not bind as a
    method: pathlib < 3.11 keeps os.stat, io.open, ... as class attributes of
    _NormalAccessor, and a plain function there would get the accessor as its
    first argument when pathlib is imported after install()."""

    def __init__(self, fn):
        self._fn = fn
        functools.update_wrapper(self, fn)

    def __call__(self, *args, **kwargs):
        return self._fn(*args, **kwargs)


def install():
    """Routes library paths through stash-libfs (idempotent)."""
    if _orig or not os.environ.get("STASH_FSD_ADDRESS"):
        return False
    if not os.path.exists(HELPER):
        return False
    _orig.update(
        open=builtins.open,
        stat=os.stat,
        lstat=os.lstat,
        listdir=os.listdir,
        scandir=os.scandir,
        access=os.access,
    )
    fns = dict(open=_Unbound(_open), stat=_Unbound(_stat), lstat=_Unbound(_lstat),
               listdir=_Unbound(_listdir), scandir=_Unbound(_scandir), access=_Unbound(_access))
    builtins.open = fns["open"]
    io.open = fns["open"]
    os.stat = fns["stat"]
    os.lstat = fns["lstat"]
    os.listdir = fns["listdir"]
    os.scandir = fns["scandir"]
    os.access = fns["access"]

    # Python < 3.11: pathlib keeps the functions it saw when it was imported
    import sys

    pathlib = sys.modules.get("pathlib")
    accessor = getattr(pathlib, "_NormalAccessor", None)
    if accessor is not None:
        for name in ("stat", "lstat", "open", "listdir", "scandir"):
            if hasattr(accessor, name):
                setattr(accessor, name, fns[name])
    return True
