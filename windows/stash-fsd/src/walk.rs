//! Directory tree walking with the semantics of `pkg/file/walk.go`
//! (`SymWalk` / `fsWalk`), plus lexical symlink evaluation.

use std::collections::VecDeque;
use std::fs::{self, Metadata};
use std::io;
use std::path::{Path, PathBuf};

use crate::errors::{code_i32, io_code};
use crate::fileinfo::{file_info, file_info_named};
use crate::paths::{native_to_wire, WirePath};
use crate::pb::{FileInfo, WalkBatch, WalkError};

/// Streams a tree in depth-first order (entries of each directory sorted by
/// name, root first) into `sink` in batches of at most `batch_size` items.
/// `sink` returns `false` to stop walking (client went away).
pub struct Walker<F: FnMut(WalkBatch) -> bool> {
    sink: F,
    batch: WalkBatch,
    batch_size: usize,
    follow: bool,
    stopped: bool,
    ancestors: Vec<PathBuf>,
}

impl<F: FnMut(WalkBatch) -> bool> Walker<F> {
    pub fn new(follow_symlinks: bool, batch_size: usize, sink: F) -> Self {
        Walker {
            sink,
            batch: WalkBatch::default(),
            batch_size: batch_size.max(1),
            follow: follow_symlinks,
            stopped: false,
            ancestors: Vec::new(),
        }
    }

    pub fn run(mut self, root: &WirePath) {
        let native = root.to_native();
        match fs::symlink_metadata(&native) {
            Err(e) => self.error(root, &e.to_string(), io_code(&e)),
            Ok(md) => self.visit(root.clone(), native, md),
        }
        self.flush();
    }

    fn push_entry(&mut self, fi: FileInfo) {
        self.batch.entries.push(fi);
        self.maybe_flush();
    }

    fn error(&mut self, path: &WirePath, msg: &str, code: tonic::Code) {
        self.batch.errors.push(WalkError {
            path: path.to_wire(),
            message: msg.to_string(),
            code: code_i32(code),
        });
        self.maybe_flush();
    }

    fn io_error(&mut self, path: &WirePath, e: &io::Error) {
        self.error(path, &e.to_string(), io_code(e));
    }

    fn maybe_flush(&mut self) {
        if self.batch.entries.len() + self.batch.errors.len() >= self.batch_size {
            self.flush();
        }
    }

    fn flush(&mut self) {
        if self.stopped || (self.batch.entries.is_empty() && self.batch.errors.is_empty()) {
            return;
        }
        let b = std::mem::take(&mut self.batch);
        if !(self.sink)(b) {
            self.stopped = true;
        }
    }

    fn visit(&mut self, display: WirePath, real: PathBuf, md: Metadata) {
        if self.stopped {
            return;
        }
        let ft = md.file_type();
        if ft.is_symlink() {
            if self.follow {
                // Go: EvalSymlinks + Lstat; directories are walked under the
                // link path, other targets are reported as the link itself.
                let target = match fs::canonicalize(&real) {
                    Ok(t) => t,
                    Err(e) => return self.io_error(&display, &e),
                };
                match fs::symlink_metadata(&target) {
                    Err(e) => return self.io_error(&display, &e),
                    Ok(tmd) if tmd.is_dir() => return self.visit_dir(display, target, tmd),
                    Ok(_) => {}
                }
            }
            self.push_entry(file_info(&display, &md));
            return;
        }
        if md.is_dir() {
            self.visit_dir(display, real, md);
        } else {
            self.push_entry(file_info(&display, &md));
        }
    }

    fn visit_dir(&mut self, display: WirePath, real: PathBuf, md: Metadata) {
        let canon = if self.follow { fs::canonicalize(&real).ok() } else { None };
        if let Some(c) = &canon {
            if self.ancestors.iter().any(|a| a == c) {
                return self.error(
                    &display,
                    &format!("symlink loop detected: {} is an ancestor", native_to_wire(c)),
                    tonic::Code::FailedPrecondition,
                );
            }
        }
        self.push_entry(file_info_named(display.name(), display.to_wire(), &md));

        let entries = match read_dir_sorted(&real) {
            Ok(v) => v,
            Err(e) => return self.io_error(&display, &e),
        };
        let pushed = canon.is_some();
        if let Some(c) = canon {
            self.ancestors.push(c);
        }
        for (name, res) in entries {
            if self.stopped {
                break;
            }
            let child = display.join(&name);
            match res {
                Ok(cmd) => self.visit(child, real.join(&name), cmd),
                Err(e) => self.io_error(&child, &e),
            }
        }
        if pushed {
            self.ancestors.pop();
        }
    }
}

/// Reads a directory, returning (name, lstat metadata) pairs sorted by name
/// (byte order of the UTF-8 name, like Go's sort of DirEntry names).
pub fn read_dir_sorted(dir: &Path) -> io::Result<Vec<(String, io::Result<Metadata>)>> {
    let mut out = Vec::new();
    for ent in fs::read_dir(dir)? {
        let ent = ent?;
        let name = ent.file_name().to_string_lossy().into_owned();
        if name.is_empty() || name == "." || name == ".." {
            continue;
        }
        // DirEntry::metadata does not follow symlinks (lstat semantics) and on
        // Windows comes for free from the directory listing.
        let md = ent.metadata().or_else(|_| fs::symlink_metadata(ent.path()));
        out.push((name, md));
    }
    out.sort_by(|a, b| a.0.as_bytes().cmp(b.0.as_bytes()));
    Ok(out)
}

/// Resolves symlinks component by component (like Go's `filepath.EvalSymlinks`)
/// so that the drive-letter form of mapped drives is preserved.
pub fn eval_symlinks(p: &WirePath, windows: bool) -> io::Result<WirePath> {
    let mut base = volume_root(p);
    let mut queue: VecDeque<String> = components(p).into();
    let mut hops = 0;
    while let Some(c) = queue.pop_front() {
        match c.as_str() {
            "" | "." => continue,
            ".." => {
                if let Some(parent) = base.parent() {
                    base = parent;
                }
                continue;
            }
            _ => {}
        }
        let cand = base.join(&c);
        let md = fs::symlink_metadata(cand.to_native())?;
        if !md.file_type().is_symlink() {
            base = cand;
            continue;
        }
        hops += 1;
        if hops > 255 {
            return Err(io::Error::new(io::ErrorKind::InvalidInput, "too many levels of symbolic links"));
        }
        let target = fs::read_link(cand.to_native())?;
        let tw = native_to_wire(&target);
        let is_abs = if windows {
            target.is_absolute() || tw.starts_with("//") || tw.as_bytes().get(1) == Some(&b':')
        } else {
            target.is_absolute()
        };
        let rel_parts: Vec<String> = if is_abs {
            let abs = WirePath::parse_style(&tw, windows)
                .map_err(|e| io::Error::new(io::ErrorKind::InvalidData, e.to_string()))?;
            base = volume_root(&abs);
            components(&abs)
        } else {
            tw.split('/').map(str::to_string).collect()
        };
        for part in rel_parts.into_iter().rev() {
            queue.push_front(part);
        }
    }
    Ok(base)
}

fn volume_root(p: &WirePath) -> WirePath {
    let mut r = p.clone();
    while let Some(parent) = r.parent() {
        r = parent;
    }
    r
}

fn components(p: &WirePath) -> Vec<String> {
    let mut out = Vec::new();
    let mut cur = p.clone();
    while let Some(parent) = cur.parent() {
        out.push(cur.name());
        cur = parent;
    }
    out.reverse();
    out
}

#[cfg(all(test, unix))]
mod tests {
    use super::*;
    use std::os::unix::fs::symlink;

    fn wp(p: &Path) -> WirePath {
        WirePath::parse(&native_to_wire(p)).unwrap()
    }

    fn walk(root: &Path, follow: bool, batch: usize) -> (Vec<WalkBatch>, Vec<String>, Vec<WalkError>) {
        let mut batches = Vec::new();
        Walker::new(follow, batch, |b| {
            batches.push(b);
            true
        })
        .run(&wp(root));
        let base = native_to_wire(root);
        let paths = batches
            .iter()
            .flat_map(|b| b.entries.iter())
            .map(|e| e.path.strip_prefix(&base).unwrap().to_string())
            .collect();
        let errors = batches.iter().flat_map(|b| b.errors.clone()).collect();
        (batches, paths, errors)
    }

    fn tree() -> tempfile::TempDir {
        let d = tempfile::tempdir().unwrap();
        let r = d.path();
        fs::create_dir_all(r.join("b/sub")).unwrap();
        fs::create_dir_all(r.join("a")).unwrap();
        fs::create_dir_all(r.join("B")).unwrap();
        fs::write(r.join("a/2.mp4"), b"22").unwrap();
        fs::write(r.join("a/10.mp4"), b"10").unwrap();
        fs::write(r.join("b/sub/x.jpg"), b"x").unwrap();
        fs::write(r.join("c.txt"), b"c").unwrap();
        fs::write(r.join("a.zip"), b"z").unwrap();
        d
    }

    #[test]
    fn order_matches_go_filepath_walk() {
        let d = tree();
        let (_, paths, errors) = walk(d.path(), false, 512);
        assert!(errors.is_empty());
        assert_eq!(
            paths,
            vec!["", "/B", "/a", "/a/10.mp4", "/a/2.mp4", "/a.zip", "/b", "/b/sub", "/b/sub/x.jpg", "/c.txt"]
        );
    }

    #[test]
    fn batches_respect_size() {
        let d = tree();
        let (batches, paths, _) = walk(d.path(), false, 3);
        assert_eq!(paths.len(), 10);
        assert!(batches.iter().all(|b| b.entries.len() + b.errors.len() <= 3));
        assert_eq!(batches.len(), 4);
    }

    #[test]
    fn stops_when_sink_refuses() {
        let d = tree();
        let mut n = 0;
        Walker::new(false, 2, |_| {
            n += 1;
            false
        })
        .run(&wp(d.path()));
        assert_eq!(n, 1);
    }

    #[test]
    fn missing_root_reports_error() {
        let d = tempfile::tempdir().unwrap();
        let (_, paths, errors) = walk(&d.path().join("nope"), false, 10);
        assert!(paths.is_empty());
        assert_eq!(errors.len(), 1);
        assert_eq!(errors[0].code, tonic::Code::NotFound as i32);
    }

    #[test]
    fn symlinks_not_followed_by_default() {
        let d = tree();
        let r = d.path();
        symlink(r.join("b"), r.join("link")).unwrap();
        let (batches, paths, _) = walk(r, false, 100);
        assert!(paths.contains(&"/link".to_string()));
        assert!(!paths.iter().any(|p| p.starts_with("/link/")));
        let link = batches[0].entries.iter().find(|e| e.path.ends_with("/link")).unwrap();
        assert!(link.is_symlink);
    }

    #[test]
    fn follow_symlinks_with_loop_detection() {
        let d = tree();
        let r = d.path();
        symlink(r.join("b"), r.join("link")).unwrap(); // dir link: walked under link path
        symlink(r.join("c.txt"), r.join("flink")).unwrap(); // file link: reported as link
        symlink(r, r.join("b/loop")).unwrap(); // loop back to the root
        symlink(r.join("missing"), r.join("broken")).unwrap();
        let (batches, paths, errors) = walk(r, true, 100);
        assert!(paths.contains(&"/link".to_string()));
        assert!(paths.contains(&"/link/sub/x.jpg".to_string()));
        let all: Vec<&FileInfo> = batches.iter().flat_map(|b| b.entries.iter()).collect();
        let link = all.iter().find(|e| e.path.ends_with("/link")).unwrap();
        assert!(link.is_dir && !link.is_symlink);
        assert_eq!(link.name, "link");
        let flink = all.iter().find(|e| e.path.ends_with("/flink")).unwrap();
        assert!(flink.is_symlink);
        // loops are reported, not walked
        assert!(errors.iter().any(|e| e.path.ends_with("/b/loop") && e.message.contains("loop")));
        assert!(errors.iter().any(|e| e.path.ends_with("/link/loop")));
        assert!(errors.iter().any(|e| e.path.ends_with("/broken")));
        assert!(!paths.iter().any(|p| p.contains("/loop/")));
    }

    #[test]
    fn eval_symlinks_resolves_components() {
        let d = tree();
        let r = d.path().canonicalize().unwrap();
        symlink(r.join("b"), r.join("abs")).unwrap();
        symlink("b/sub", r.join("rel")).unwrap();
        symlink("../c.txt", r.join("a/up")).unwrap();
        let got = eval_symlinks(&wp(&r.join("abs/sub/x.jpg")), false).unwrap();
        assert_eq!(got.to_wire(), native_to_wire(&r.join("b/sub/x.jpg")));
        let got = eval_symlinks(&wp(&r.join("rel/x.jpg")), false).unwrap();
        assert_eq!(got.to_wire(), native_to_wire(&r.join("b/sub/x.jpg")));
        let got = eval_symlinks(&wp(&r.join("a/up")), false).unwrap();
        assert_eq!(got.to_wire(), native_to_wire(&r.join("c.txt")));
        assert!(eval_symlinks(&wp(&r.join("nope")), false).is_err());
        symlink(r.join("self"), r.join("self")).unwrap();
        assert!(eval_symlinks(&wp(&r.join("self")), false).is_err());
    }
}
