"""Tests for stash_libfs with a fake stash-libfs helper that serves a
temporary directory as U:/ (python3 -m unittest test_stash_libfs)."""

import glob
import os
import pathlib
import shutil
import stat
import sys
import tempfile
import textwrap
import unittest

import stash_libfs

FAKE_HELPER = textwrap.dedent(
    """\
    #!{python}
    import json, os, sys
    root = os.environ["FAKE_LIBRARY_ROOT"]
    cmd, path = sys.argv[1], sys.argv[2]
    local = os.path.join(root, path[3:]) if path.upper().startswith("U:/") else None
    if local is None or not os.path.exists(local):
        sys.stderr.write("not found")
        sys.exit(2)
    def entry(p, name=None):
        st = os.stat(p)
        d = {{"dir": os.path.isdir(p), "size": st.st_size, "mtime": st.st_mtime,
             "mode": 0o040755 if os.path.isdir(p) else 0o100644}}
        if name: d["name"] = name
        return d
    if cmd == "stat":
        print(json.dumps(entry(local)))
    elif cmd == "ls":
        print(json.dumps([entry(os.path.join(local, n), n) for n in sorted(os.listdir(local))]))
    else:
        with open(local, "rb") as f:
            sys.stdout.buffer.write(f.read())
    """
)


class LibfsTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.tmp = tempfile.mkdtemp()
        cls.root = os.path.join(cls.tmp, "lib")
        os.makedirs(os.path.join(cls.root, "Videos", "Perf"))
        with open(os.path.join(cls.root, "Videos", "Perf", "scene.nfo"), "w", encoding="utf-8") as f:
            f.write("<movie><title>Ünïcode</title></movie>")
        with open(os.path.join(cls.root, "Videos", "Perf", "scene.jpg"), "wb") as f:
            f.write(b"\xff\xd8\xff")
        helper = os.path.join(cls.tmp, "stash-libfs")
        with open(helper, "w") as f:
            f.write(FAKE_HELPER.format(python=sys.executable))
        os.chmod(helper, 0o755)
        os.environ.update(FAKE_LIBRARY_ROOT=cls.root, STASH_FSD_ADDRESS="test:1")
        stash_libfs.HELPER = helper
        assert stash_libfs.install()

    @classmethod
    def tearDownClass(cls):
        shutil.rmtree(cls.tmp)

    def test_read(self):
        with open("U:/Videos/Perf/scene.nfo", encoding="utf-8") as f:
            self.assertIn("Ünïcode", f.read())
        with open("U:/Videos/Perf/scene.jpg", "rb") as f:
            self.assertEqual(f.read(), b"\xff\xd8\xff")
        self.assertIn("Ünïcode", pathlib.Path("U:/Videos/Perf/scene.nfo").read_text(encoding="utf-8"))

    def test_stat_family(self):
        self.assertTrue(os.path.exists("U:/Videos/Perf/scene.nfo"))
        self.assertTrue(os.path.isfile("U:/Videos/Perf/scene.nfo"))
        self.assertTrue(os.path.isdir("U:/Videos/Perf"))
        self.assertFalse(os.path.exists("U:/Videos/Perf/missing.nfo"))
        self.assertEqual(os.path.getsize("U:/Videos/Perf/scene.jpg"), 3)
        self.assertTrue(stat.S_ISDIR(os.stat("U:/Videos").st_mode))
        self.assertTrue(pathlib.Path("U:/Videos/Perf/scene.jpg").exists())
        self.assertTrue(os.access("U:/Videos/Perf/scene.nfo", os.R_OK))
        self.assertFalse(os.access("U:/Videos/Perf/scene.nfo", os.W_OK))

    def test_listing(self):
        self.assertEqual(os.listdir("U:/Videos/Perf"), ["scene.jpg", "scene.nfo"])
        self.assertEqual(sorted(glob.glob(glob.escape("U:/Videos/Perf") + "/*.*")),
                         ["U:/Videos/Perf/scene.jpg", "U:/Videos/Perf/scene.nfo"])
        with os.scandir("U:/Videos") as it:
            entries = [(e.name, e.is_dir()) for e in it]
        self.assertEqual(entries, [("Perf", True)])

    def test_large_files_are_streamed(self):
        old = stash_libfs.SPOOL_MAX
        stash_libfs.SPOOL_MAX = 1
        try:
            with open("U:/Videos/Perf/scene.jpg", "rb") as f:
                self.assertFalse(f.seekable())
                self.assertEqual(f.read(), b"\xff\xd8\xff")
            with open("U:/Videos/Perf/scene.nfo", encoding="utf-8") as f:
                self.assertIn("Ünïcode", f.read())
        finally:
            stash_libfs.SPOOL_MAX = old

    def test_errors(self):
        with self.assertRaises(FileNotFoundError):
            open("U:/Videos/Perf/missing.nfo")
        with self.assertRaises(OSError):
            open("U:/Videos/Perf/new.nfo", "w")
        with self.assertRaises(IsADirectoryError):
            open("U:/Videos/Perf")

    def test_other_paths_unchanged(self):
        path = os.path.join(self.tmp, "local.txt")
        with open(path, "w") as f:
            f.write("x")
        self.assertTrue(os.path.exists(path))
        self.assertEqual(os.listdir(self.tmp).count("local.txt"), 1)
        self.assertFalse(stash_libfs.is_library_path("/data/x.mp4"))
        self.assertFalse(stash_libfs.is_library_path(3))
        self.assertTrue(stash_libfs.is_library_path("UNC:/nas/share/x.mp4"))


if __name__ == "__main__":
    unittest.main()
