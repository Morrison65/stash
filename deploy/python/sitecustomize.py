# Imported by every Python interpreter in the API image (PYTHONPATH): gives
# plugins and scrapers read-only access to media library paths through
# stash-fsd. See stash_libfs.py.
try:
    import stash_libfs

    stash_libfs.install()
except Exception:  # never break interpreter start-up
    pass
