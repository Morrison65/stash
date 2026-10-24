//! Blocking filesystem mutations ported from `pkg/fsutil`.

use std::fs::{self, File, FileTimes, OpenOptions};
use std::io::{self, Read, Write};
use std::path::Path;

use crate::paths::WirePath;

const COPY_BUF: usize = 4 << 20;

/// Go `fsutil.CopyFile`: copies to a new file (fails if `dst` exists) and
/// preserves the modification time. A partially written `dst` is removed.
pub fn copy_file(src: &Path, dst: &Path) -> io::Result<()> {
    let mut r = File::open(src)?;
    let src_md = r.metadata()?;
    let mut w = OpenOptions::new().write(true).create_new(true).open(dst)?;
    let res = (|| -> io::Result<()> {
        let mut buf = vec![0u8; COPY_BUF];
        loop {
            let n = match r.read(&mut buf) {
                Ok(0) => break,
                Ok(n) => n,
                Err(e) if e.kind() == io::ErrorKind::Interrupted => continue,
                Err(e) => return Err(e),
            };
            w.write_all(&buf[..n])?;
        }
        w.flush()?;
        let mtime = src_md.modified()?;
        w.set_times(FileTimes::new().set_modified(mtime).set_accessed(mtime))?;
        Ok(())
    })();
    drop(w);
    if res.is_err() {
        let _ = fs::remove_file(dst);
    }
    res
}

/// Go `fsutil.SafeMove`: rename, falling back to copy + delete for a file on
/// another volume. The copy is written to `<dst>.partial` and renamed into
/// place when complete, so a scan never indexes a half-written file. Other
/// rename errors (e.g. the file is in use) are returned as they are: copying
/// a locked file would only fail when deleting the source.
pub fn safe_move(src: &Path, dst: &Path) -> io::Result<()> {
    let rename_err = match fs::rename(src, dst) {
        Ok(()) => return Ok(()),
        Err(e) => e,
    };
    if rename_err.kind() != io::ErrorKind::CrossesDevices {
        return Err(rename_err);
    }
    let md = fs::symlink_metadata(src)?;
    if !md.is_file() {
        return Err(rename_err);
    }
    let partial = partial_path(dst);
    // left over from an interrupted move
    let _ = fs::remove_file(&partial);
    if let Err(e) = copy_file(src, &partial) {
        return Err(io::Error::new(
            e.kind(),
            format!("copying file during SafeMove failed with: '{e}'; renaming file failed previously with: '{rename_err}'"),
        ));
    }
    if let Err(e) = fs::rename(&partial, dst) {
        let _ = fs::remove_file(&partial);
        return Err(io::Error::new(e.kind(), format!("renaming the copy into place failed with: '{e}'")));
    }
    if let Err(e) = fs::remove_file(src) {
        let _ = fs::remove_file(dst);
        return Err(io::Error::new(
            e.kind(),
            format!("removing old file during SafeMove failed with: '{e}'; renaming file failed previously with: '{rename_err}'"),
        ));
    }
    Ok(())
}

fn partial_path(dst: &Path) -> std::path::PathBuf {
    let mut s = dst.as_os_str().to_owned();
    s.push(".partial");
    s.into()
}

/// Go `os.Remove`: removes a file, an empty directory or a symlink.
pub fn remove(p: &Path) -> io::Result<()> {
    let md = fs::symlink_metadata(p)?;
    if md.is_dir() {
        return fs::remove_dir(p);
    }
    match fs::remove_file(p) {
        Ok(()) => Ok(()),
        // directory symlinks / junctions on Windows need RemoveDirectory
        Err(e) if md.file_type().is_symlink() => fs::remove_dir(p).map_err(|_| e),
        Err(e) => Err(e),
    }
}

/// Go `os.RemoveAll`: a missing path is not an error; symlinks are removed,
/// never followed.
pub fn remove_all(p: &Path) -> io::Result<()> {
    let md = match fs::symlink_metadata(p) {
        Ok(md) => md,
        Err(e) if e.kind() == io::ErrorKind::NotFound => return Ok(()),
        Err(e) => return Err(e),
    };
    if md.is_dir() {
        fs::remove_dir_all(p)
    } else {
        remove(p)
    }
}

/// Go `filepath.Ext` of a base name.
pub fn go_ext(base: &str) -> &str {
    match base.rfind('.') {
        Some(i) => &base[i..],
        None => "",
    }
}

/// Destination for `MoveToTrash` (Go `fsutil.MoveToTrash`): `<trash>/<base>`,
/// or `<trash>/<name>_<YYYYMMDD-HHMMSS><ext>` when that exists.
pub fn trash_destination(src: &WirePath, trash: &WirePath, exists: impl Fn(&WirePath) -> bool, timestamp: &str) -> WirePath {
    let base = src.name();
    let dest = trash.join(&base);
    if !exists(&dest) {
        return dest;
    }
    let ext = go_ext(&base);
    let stem = &base[..base.len() - ext.len()];
    trash.join(&format!("{stem}_{timestamp}{ext}"))
}

/// Local-time timestamp in Go's "20060102-150405" layout.
pub fn trash_timestamp() -> String {
    chrono::Local::now().format("%Y%m%d-%H%M%S").to_string()
}

/// Go `fsutil.flipCaseSingle`: flips the case of the first letter.
pub fn flip_case_single(s: &str) -> Option<String> {
    let mut out = String::with_capacity(s.len());
    let mut flipped = false;
    for ch in s.chars() {
        if !flipped && ch.is_alphabetic() {
            if ch.is_uppercase() {
                out.extend(ch.to_lowercase());
            } else {
                out.extend(ch.to_uppercase());
            }
            flipped = true;
        } else {
            out.push(ch);
        }
    }
    if flipped && out != s {
        Some(out)
    } else {
        None
    }
}

/// Go `fsutil.IsFsPathCaseSensitive`.
pub fn is_path_case_sensitive(p: &WirePath) -> io::Result<bool> {
    let md = fs::metadata(p.to_native())?;
    let parent = match p.parent() {
        Some(parent) => parent,
        // Volume roots: drive letters and share names are case-insensitive.
        None if cfg!(windows) => return Ok(false),
        None => {
            return Err(io::Error::new(io::ErrorKind::InvalidInput, "cannot flip the case of a root path"));
        }
    };
    let flipped = flip_case_single(&p.name())
        .ok_or_else(|| io::Error::new(io::ErrorKind::InvalidInput, format!("can not flip the case of {}", p.name())))?;
    let fp = parent.join(&flipped);
    match fs::metadata(fp.to_native()) {
        Err(_) => Ok(true),
        Ok(fmd) => {
            if fmd.modified().ok() == md.modified().ok() {
                Ok(false)
            } else {
                Err(io::Error::other(format!("can not determine case sensitivity of path {p}")))
            }
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::paths::native_to_wire;
    use std::time::{Duration, SystemTime};

    fn wp(p: &Path) -> WirePath {
        WirePath::parse(&native_to_wire(p)).unwrap()
    }

    #[test]
    fn copy_preserves_mtime_and_refuses_existing() {
        let d = tempfile::tempdir().unwrap();
        let src = d.path().join("s.bin");
        fs::write(&src, vec![7u8; 5_000_000]).unwrap();
        let old = SystemTime::UNIX_EPOCH + Duration::from_secs(1_500_000_000);
        File::options().write(true).open(&src).unwrap().set_modified(old).unwrap();
        let dst = d.path().join("d.bin");
        copy_file(&src, &dst).unwrap();
        assert_eq!(fs::read(&dst).unwrap().len(), 5_000_000);
        assert_eq!(fs::metadata(&dst).unwrap().modified().unwrap(), old);
        assert_eq!(copy_file(&src, &dst).unwrap_err().kind(), io::ErrorKind::AlreadyExists);
    }

    #[test]
    fn safe_move_renames() {
        let d = tempfile::tempdir().unwrap();
        let src = d.path().join("a");
        fs::write(&src, b"x").unwrap();
        safe_move(&src, &d.path().join("b")).unwrap();
        assert!(!src.exists());
        assert_eq!(fs::read(d.path().join("b")).unwrap(), b"x");
    }

    #[test]
    fn safe_move_returns_other_errors_without_copying() {
        let d = tempfile::tempdir().unwrap();
        let src = d.path().join("a");
        fs::write(&src, b"x").unwrap();
        let dst = d.path().join("missing").join("b");
        assert_eq!(safe_move(&src, &dst).unwrap_err().kind(), io::ErrorKind::NotFound);
        assert!(src.exists());
        assert!(!partial_path(&dst).exists());
    }

    #[test]
    fn remove_semantics() {
        let d = tempfile::tempdir().unwrap();
        let f = d.path().join("f");
        fs::write(&f, b"x").unwrap();
        remove(&f).unwrap();
        assert_eq!(remove(&f).unwrap_err().kind(), io::ErrorKind::NotFound);
        let dir = d.path().join("dir");
        fs::create_dir(&dir).unwrap();
        fs::write(dir.join("x"), b"x").unwrap();
        assert_eq!(remove(&dir).unwrap_err().kind(), io::ErrorKind::DirectoryNotEmpty);
        remove_all(&dir).unwrap();
        assert!(!dir.exists());
        remove_all(&dir).unwrap(); // missing is fine
    }

    #[cfg(unix)]
    #[test]
    fn remove_all_does_not_follow_symlinks() {
        let d = tempfile::tempdir().unwrap();
        let keep = d.path().join("keep");
        fs::create_dir(&keep).unwrap();
        fs::write(keep.join("precious"), b"x").unwrap();
        let victim = d.path().join("victim");
        fs::create_dir(&victim).unwrap();
        std::os::unix::fs::symlink(&keep, victim.join("link")).unwrap();
        remove_all(&victim).unwrap();
        assert!(keep.join("precious").exists());
    }

    #[test]
    fn ext_like_go() {
        assert_eq!(go_ext("a.mp4"), ".mp4");
        assert_eq!(go_ext("a.b.mkv"), ".mkv");
        assert_eq!(go_ext("noext"), "");
        assert_eq!(go_ext(".hidden"), ".hidden");
    }

    #[test]
    fn trash_naming_matches_go() {
        let src = WirePath::parse_style("U:/Media/clip.mp4", true).unwrap();
        let trash = WirePath::parse_style("U:/.trash", true).unwrap();
        let d = trash_destination(&src, &trash, |_| false, "20260102-030405");
        assert_eq!(d.to_wire(), "U:/.trash/clip.mp4");
        let d = trash_destination(&src, &trash, |_| true, "20260102-030405");
        assert_eq!(d.to_wire(), "U:/.trash/clip_20260102-030405.mp4");
        let dir = WirePath::parse_style("U:/Media/folder", true).unwrap();
        let d = trash_destination(&dir, &trash, |_| true, "20260102-030405");
        assert_eq!(d.to_wire(), "U:/.trash/folder_20260102-030405");
        let ts = trash_timestamp();
        assert_eq!(ts.len(), 15);
        assert_eq!(&ts[8..9], "-");
    }

    #[test]
    fn flip_case() {
        assert_eq!(flip_case_single("abc").unwrap(), "Abc");
        assert_eq!(flip_case_single("Abc").unwrap(), "abc");
        assert_eq!(flip_case_single("123x").unwrap(), "123X");
        assert_eq!(flip_case_single("éa").unwrap(), "Éa");
        assert!(flip_case_single("1234").is_none());
    }

    #[test]
    fn case_sensitivity_on_linux_tmp() {
        let d = tempfile::tempdir().unwrap();
        let f = d.path().join("Video.mp4");
        fs::write(&f, b"x").unwrap();
        // ext4/overlayfs are case sensitive
        assert!(is_path_case_sensitive(&wp(&f)).unwrap());
        assert!(is_path_case_sensitive(&wp(&d.path().join("missing"))).is_err());
    }
}
