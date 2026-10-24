//! Conversion of native metadata into `FileInfo` with Go `fs.FileMode` bits.

use std::fs::Metadata;
use std::time::{SystemTime, UNIX_EPOCH};

use crate::paths::WirePath;
use crate::pb::FileInfo;

/// Go `fs.ModeDir`.
pub const MODE_DIR: u32 = 1 << 31;
/// Go `fs.ModeSymlink`.
pub const MODE_SYMLINK: u32 = 1 << 27;

/// Builds Go mode bits from type flags and permission bits.
pub fn go_mode(is_dir: bool, is_symlink: bool, perm: u32) -> u32 {
    let mut m = perm & 0o777;
    if is_dir {
        m |= MODE_DIR;
    }
    if is_symlink {
        m |= MODE_SYMLINK;
    }
    m
}

/// Permission bits approximated the way Go does on Windows: 0444 for
/// read-only files, 0666 otherwise, plus 0111 for directories.
pub fn windows_perm(readonly: bool, is_dir: bool) -> u32 {
    let mut p = if readonly { 0o444 } else { 0o666 };
    if is_dir {
        p |= 0o111;
    }
    p
}

#[cfg(windows)]
fn perm_bits(md: &Metadata) -> u32 {
    windows_perm(md.permissions().readonly(), md.is_dir())
}

#[cfg(unix)]
fn perm_bits(md: &Metadata) -> u32 {
    use std::os::unix::fs::PermissionsExt;
    md.permissions().mode() & 0o777
}

#[cfg(not(any(unix, windows)))]
fn perm_bits(md: &Metadata) -> u32 {
    windows_perm(md.permissions().readonly(), md.is_dir())
}

pub fn unix_nanos(t: SystemTime) -> i64 {
    match t.duration_since(UNIX_EPOCH) {
        Ok(d) => i64::try_from(d.as_nanos()).unwrap_or(i64::MAX),
        Err(e) => -i64::try_from(e.duration().as_nanos()).unwrap_or(i64::MAX),
    }
}

pub fn mtime_nanos(md: &Metadata) -> i64 {
    md.modified().map(unix_nanos).unwrap_or(0)
}

/// `FileInfo` for `path` from metadata (stat or lstat result).
pub fn file_info(path: &WirePath, md: &Metadata) -> FileInfo {
    file_info_named(path.name(), path.to_wire(), md)
}

pub fn file_info_named(name: String, path: String, md: &Metadata) -> FileInfo {
    let ft = md.file_type();
    let is_symlink = ft.is_symlink();
    let is_dir = md.is_dir() && !is_symlink;
    FileInfo {
        name,
        path,
        size: if is_dir { 0 } else { md.len() as i64 },
        mode: go_mode(is_dir, is_symlink, perm_bits(md)),
        mod_time_unix_nano: mtime_nanos(md),
        is_dir,
        is_symlink,
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn mode_bits_match_go() {
        assert_eq!(go_mode(true, false, 0o777), 0x8000_01ff);
        assert_eq!(go_mode(false, true, 0o666), 0x0800_01b6);
        assert_eq!(go_mode(false, false, 0o644), 0o644);
        assert_eq!(windows_perm(false, false), 0o666);
        assert_eq!(windows_perm(true, false), 0o444);
        assert_eq!(windows_perm(false, true), 0o777);
        assert_eq!(windows_perm(true, true), 0o555);
    }

    #[test]
    fn info_from_metadata() {
        let dir = tempfile::tempdir().unwrap();
        let f = dir.path().join("a.txt");
        std::fs::write(&f, b"hello").unwrap();
        let wp = WirePath::parse(&crate::paths::native_to_wire(&f)).unwrap();
        let fi = file_info(&wp, &std::fs::metadata(&f).unwrap());
        assert_eq!(fi.name, "a.txt");
        assert_eq!(fi.size, 5);
        assert!(!fi.is_dir && !fi.is_symlink);
        assert_eq!(fi.mode & MODE_DIR, 0);
        assert!(fi.mod_time_unix_nano > 1_600_000_000_000_000_000);

        let dwp = WirePath::parse(&crate::paths::native_to_wire(dir.path())).unwrap();
        let di = file_info(&dwp, &std::fs::metadata(dir.path()).unwrap());
        assert!(di.is_dir);
        assert_eq!(di.size, 0);
        assert_ne!(di.mode & MODE_DIR, 0);
    }

    #[cfg(unix)]
    #[test]
    fn symlink_info() {
        let dir = tempfile::tempdir().unwrap();
        let t = dir.path().join("t");
        std::fs::create_dir(&t).unwrap();
        let l = dir.path().join("l");
        std::os::unix::fs::symlink(&t, &l).unwrap();
        let wp = WirePath::parse(&crate::paths::native_to_wire(&l)).unwrap();
        let li = file_info(&wp, &std::fs::symlink_metadata(&l).unwrap());
        assert!(li.is_symlink && !li.is_dir);
        assert_ne!(li.mode & MODE_SYMLINK, 0);
        assert_eq!(li.mode & MODE_DIR, 0);
        let si = file_info(&wp, &std::fs::metadata(&l).unwrap());
        assert!(si.is_dir && !si.is_symlink);
    }
}
