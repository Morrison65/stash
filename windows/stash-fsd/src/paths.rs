//! Path handling between the wire form and the native form, plus root
//! enforcement.
//!
//! Wire form: Windows paths written with forward slashes, e.g.
//! `U:/Media/clip.mp4` or `//nas/share/clip.mp4`. Backslashes are accepted
//! on input. Paths are cleaned lexically (`.`/`..`/duplicate separators) and
//! a `..` that would climb above the volume root is rejected.
//!
//! The parser has two styles so the whole service can be tested on Linux:
//! the Windows style (used on Windows builds) and a Unix style (used on
//! other hosts, `/tmp/x`).

use std::fmt;
use std::path::{Path, PathBuf};

use anyhow::{bail, Result};

#[derive(Debug, Clone, PartialEq, Eq)]
pub enum PathError {
    /// Malformed input (empty, NUL byte, bad UNC prefix, ...).
    Invalid(String),
    /// Relative paths or paths outside the configured roots.
    OutsideRoots(String),
}

impl fmt::Display for PathError {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            PathError::Invalid(m) => write!(f, "invalid path: {m}"),
            PathError::OutsideRoots(p) => write!(f, "path is outside the allowed roots: {p}"),
        }
    }
}

impl std::error::Error for PathError {}

impl From<PathError> for tonic::Status {
    fn from(e: PathError) -> Self {
        match e {
            PathError::Invalid(_) => tonic::Status::invalid_argument(e.to_string()),
            PathError::OutsideRoots(_) => tonic::Status::permission_denied(e.to_string()),
        }
    }
}

/// Whether paths on this host are compared case-insensitively.
pub fn host_case_insensitive() -> bool {
    cfg!(windows)
}

/// Whether wire paths are parsed in Windows style on this host.
pub fn host_windows_style() -> bool {
    cfg!(windows)
}

#[derive(Debug, Clone, PartialEq, Eq)]
enum Prefix {
    /// `X:` (letter case as given).
    Drive(char),
    /// `//server/share`.
    Unc(String, String),
    /// `/` (Unix style only).
    UnixRoot,
}

/// A cleaned, absolute path.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct WirePath {
    prefix: Prefix,
    comps: Vec<String>,
}

impl WirePath {
    /// Parses in the host style (tests and helpers; services go through `Roots::check`).
    #[cfg_attr(not(test), allow(dead_code))]
    pub fn parse(input: &str) -> Result<WirePath, PathError> {
        Self::parse_style(input, host_windows_style())
    }

    pub fn parse_style(input: &str, windows: bool) -> Result<WirePath, PathError> {
        if input.is_empty() {
            return Err(PathError::Invalid("empty path".into()));
        }
        if input.contains('\0') {
            return Err(PathError::Invalid("path contains a NUL byte".into()));
        }
        if windows {
            Self::parse_windows(input)
        } else {
            Self::parse_unix(input)
        }
    }

    fn parse_unix(input: &str) -> Result<WirePath, PathError> {
        if !input.starts_with('/') {
            return Err(PathError::OutsideRoots(input.to_string()));
        }
        let comps = clean_components(input, input)?;
        Ok(WirePath {
            prefix: Prefix::UnixRoot,
            comps,
        })
    }

    fn parse_windows(input: &str) -> Result<WirePath, PathError> {
        let mut s: String = input.replace('\\', "/");
        // Verbatim prefixes produced by some Windows APIs.
        if let Some(rest) = strip_prefix_ci(&s, "//?/UNC/") {
            s = format!("//{rest}");
        } else if let Some(rest) = s.strip_prefix("//?/") {
            s = rest.to_string();
        } else if s.starts_with("//./") {
            return Err(PathError::Invalid(format!("device paths are not supported: {input}")));
        }

        let b = s.as_bytes();
        if b.len() >= 2 && b[0].is_ascii_alphabetic() && b[1] == b':' {
            // "U:" alone is treated as the drive root: Go's filepath.Dir on
            // Linux turns "U:/clip.mp4" into "U:".
            let rest = &s[2..];
            if !rest.is_empty() && !rest.starts_with('/') {
                // "U:foo" is relative to the current directory of drive U.
                return Err(PathError::OutsideRoots(input.to_string()));
            }
            let comps = clean_components(rest, input)?;
            return Ok(WirePath {
                prefix: Prefix::Drive(b[0] as char),
                comps,
            });
        }

        if let Some(rest) = s.strip_prefix('/') {
            // "//server/share/..." is UNC. A single leading slash is also
            // read as UNC because Go's filepath.Clean on Linux collapses the
            // leading "//" of UNC paths; a drive-relative root path is never
            // meaningful for a service.
            let rest = rest.trim_start_matches('/');
            let mut it = rest.splitn(3, '/');
            let server = it.next().unwrap_or("");
            let share = it.next().unwrap_or("");
            if server.is_empty() || share.is_empty() || server == "." || server == ".." || share == "." || share == ".." {
                return Err(PathError::Invalid(format!("UNC path needs //server/share: {input}")));
            }
            let tail = it.next().unwrap_or("");
            let comps = clean_components(tail, input)?;
            return Ok(WirePath {
                prefix: Prefix::Unc(server.to_string(), share.to_string()),
                comps,
            });
        }

        Err(PathError::OutsideRoots(input.to_string()))
    }

    /// Wire form: forward slashes, drive roots as `U:/`.
    pub fn to_wire(&self) -> String {
        self.render('/')
    }

    /// Native form for the host OS.
    pub fn to_native(&self) -> PathBuf {
        match self.prefix {
            Prefix::UnixRoot => PathBuf::from(self.render('/')),
            _ => PathBuf::from(self.render(if cfg!(windows) { '\\' } else { '/' })),
        }
    }

    fn render(&self, sep: char) -> String {
        let mut out = match &self.prefix {
            Prefix::Drive(c) => format!("{c}:{sep}"),
            Prefix::Unc(server, share) => format!("{sep}{sep}{server}{sep}{share}"),
            Prefix::UnixRoot => "/".to_string(),
        };
        for (i, c) in self.comps.iter().enumerate() {
            let needs_sep = match self.prefix {
                Prefix::Unc(..) => true,
                _ => i > 0,
            };
            if needs_sep {
                out.push(sep);
            }
            out.push_str(c);
        }
        out
    }

    /// Base name; the full path for volume roots.
    pub fn name(&self) -> String {
        match self.comps.last() {
            Some(c) => c.clone(),
            None => self.to_wire(),
        }
    }

    pub fn join(&self, name: &str) -> WirePath {
        let mut p = self.clone();
        p.comps.push(name.to_string());
        p
    }

    pub fn parent(&self) -> Option<WirePath> {
        if self.comps.is_empty() {
            return None;
        }
        let mut p = self.clone();
        p.comps.pop();
        Some(p)
    }

    /// `true` if `self` equals or is below `root` (component boundaries).
    pub fn starts_with(&self, root: &WirePath, case_insensitive: bool) -> bool {
        if !prefix_eq(&self.prefix, &root.prefix, case_insensitive) {
            return false;
        }
        if root.comps.len() > self.comps.len() {
            return false;
        }
        root.comps
            .iter()
            .zip(self.comps.iter())
            .all(|(a, b)| comp_eq(a, b, case_insensitive))
    }

    pub fn same_path(&self, other: &WirePath, case_insensitive: bool) -> bool {
        self.comps.len() == other.comps.len() && self.starts_with(other, case_insensitive)
    }
}

impl fmt::Display for WirePath {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.write_str(&self.to_wire())
    }
}

fn strip_prefix_ci<'a>(s: &'a str, prefix: &str) -> Option<&'a str> {
    if s.len() >= prefix.len() && s.is_char_boundary(prefix.len()) && s[..prefix.len()].eq_ignore_ascii_case(prefix) {
        Some(&s[prefix.len()..])
    } else {
        None
    }
}

fn clean_components(rest: &str, original: &str) -> Result<Vec<String>, PathError> {
    let mut comps: Vec<String> = Vec::new();
    for c in rest.split('/') {
        match c {
            "" | "." => {}
            ".." => {
                if comps.pop().is_none() {
                    return Err(PathError::OutsideRoots(format!("{original} (escapes the volume root)")));
                }
            }
            c => comps.push(c.to_string()),
        }
    }
    Ok(comps)
}

fn prefix_eq(a: &Prefix, b: &Prefix, ci: bool) -> bool {
    match (a, b) {
        // Drive letters are always case-insensitive.
        (Prefix::Drive(x), Prefix::Drive(y)) => x.eq_ignore_ascii_case(y),
        (Prefix::Unc(s1, h1), Prefix::Unc(s2, h2)) => comp_eq(s1, s2, true) && comp_eq(h1, h2, ci),
        (Prefix::UnixRoot, Prefix::UnixRoot) => true,
        _ => false,
    }
}

fn comp_eq(a: &str, b: &str, ci: bool) -> bool {
    if !ci {
        return a == b;
    }
    if a.eq_ignore_ascii_case(b) {
        return true;
    }
    if a.is_ascii() && b.is_ascii() {
        return false;
    }
    a.to_lowercase() == b.to_lowercase()
}

/// Converts a native path (e.g. from `canonicalize`) to the wire form,
/// stripping verbatim prefixes.
pub fn native_to_wire(p: &Path) -> String {
    let s = p.to_string_lossy();
    if cfg!(windows) {
        native_to_wire_windows(&s)
    } else {
        s.into_owned()
    }
}

pub fn native_to_wire_windows(s: &str) -> String {
    if let Some(rest) = strip_prefix_ci(s, r"\\?\UNC\") {
        return format!("//{}", rest.replace('\\', "/"));
    }
    let s = s.strip_prefix(r"\\?\").unwrap_or(s);
    s.replace('\\', "/")
}

/// Converts a configured path (forward or back slashes) to native form
/// without root checks; used for tool and storage paths from the config.
pub fn to_native_string(s: &str) -> String {
    if cfg!(windows) {
        s.replace('/', "\\")
    } else {
        s.to_string()
    }
}

/// The configured roots below which access is allowed.
#[derive(Debug, Clone)]
pub struct Roots {
    roots: Vec<WirePath>,
    case_insensitive: bool,
    windows: bool,
}

impl Roots {
    pub fn new(list: &[String], case_insensitive: bool) -> Result<Roots> {
        Self::with_style(list, host_windows_style(), case_insensitive)
    }

    pub fn with_style(list: &[String], windows: bool, case_insensitive: bool) -> Result<Roots> {
        let mut roots = Vec::new();
        for r in list {
            match WirePath::parse_style(r.trim(), windows) {
                Ok(p) => roots.push(p),
                Err(e) => bail!("invalid root {r:?}: {e}"),
            }
        }
        if roots.is_empty() {
            bail!("no roots configured");
        }
        Ok(Roots {
            roots,
            case_insensitive,
            windows,
        })
    }

    pub fn case_insensitive(&self) -> bool {
        self.case_insensitive
    }

    /// Parses and cleans `input` and checks that it is below a root.
    pub fn check(&self, input: &str) -> Result<WirePath, PathError> {
        let p = WirePath::parse_style(input, self.windows)?;
        if self.contains(&p) {
            Ok(p)
        } else {
            Err(PathError::OutsideRoots(p.to_wire()))
        }
    }

    pub fn contains(&self, p: &WirePath) -> bool {
        self.roots.iter().any(|r| p.starts_with(r, self.case_insensitive))
    }

    /// `true` if `p` is exactly one of the configured roots.
    pub fn is_root(&self, p: &WirePath) -> bool {
        self.roots.iter().any(|r| p.same_path(r, self.case_insensitive))
    }

    pub fn list(&self) -> Vec<String> {
        self.roots.iter().map(|r| r.to_wire()).collect()
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn w(s: &str) -> Result<String, PathError> {
        WirePath::parse_style(s, true).map(|p| p.to_wire())
    }

    fn u(s: &str) -> Result<String, PathError> {
        WirePath::parse_style(s, false).map(|p| p.to_wire())
    }

    #[test]
    fn windows_drive_paths() {
        assert_eq!(w("U:/Media/x.mp4").unwrap(), "U:/Media/x.mp4");
        assert_eq!(w(r"U:\Media\x.mp4").unwrap(), "U:/Media/x.mp4");
        assert_eq!(w("u:/Media//sub/./x.mp4").unwrap(), "u:/Media/sub/x.mp4");
        assert_eq!(w("U:/Media/sub/../x.mp4").unwrap(), "U:/Media/x.mp4");
        assert_eq!(w("U:/Media/").unwrap(), "U:/Media");
        assert_eq!(w("U:/").unwrap(), "U:/");
        assert_eq!(w("U:").unwrap(), "U:/");
        assert_eq!(w("U:\\").unwrap(), "U:/");
        assert_eq!(w("U:/a/..").unwrap(), "U:/");
    }

    #[test]
    fn windows_escapes_rejected() {
        assert!(matches!(w("U:/.."), Err(PathError::OutsideRoots(_))));
        assert!(matches!(w("U:/Media/../../x"), Err(PathError::OutsideRoots(_))));
        assert!(matches!(w(r"U:\..\Windows"), Err(PathError::OutsideRoots(_))));
    }

    #[test]
    fn windows_relative_rejected() {
        assert!(matches!(w("Media/x.mp4"), Err(PathError::OutsideRoots(_))));
        assert!(matches!(w("U:Media"), Err(PathError::OutsideRoots(_))));
        assert!(matches!(w("x.mp4"), Err(PathError::OutsideRoots(_))));
        assert!(matches!(w(""), Err(PathError::Invalid(_))));
        assert!(matches!(w("U:/a\0b"), Err(PathError::Invalid(_))));
    }

    #[test]
    fn windows_unc_paths() {
        assert_eq!(w("//nas/share/x.mp4").unwrap(), "//nas/share/x.mp4");
        assert_eq!(w(r"\\nas\share\dir\x.mp4").unwrap(), "//nas/share/dir/x.mp4");
        assert_eq!(w("//nas/share").unwrap(), "//nas/share");
        assert_eq!(w("//nas/share/").unwrap(), "//nas/share");
        // Go's filepath.Clean on Linux collapses the leading "//".
        assert_eq!(w("/nas/share/x.mp4").unwrap(), "//nas/share/x.mp4");
        assert!(matches!(w("//nas"), Err(PathError::Invalid(_))));
        assert!(matches!(w("//nas/share/.."), Err(PathError::OutsideRoots(_))));
        assert!(matches!(w("//./pipe/x"), Err(PathError::Invalid(_))));
    }

    #[test]
    fn windows_verbatim_prefixes() {
        assert_eq!(w(r"\\?\U:\Media\x.mp4").unwrap(), "U:/Media/x.mp4");
        assert_eq!(w(r"\\?\UNC\nas\share\x").unwrap(), "//nas/share/x");
    }

    #[test]
    fn native_rendering() {
        let p = WirePath::parse_style("U:/Media/x.mp4", true).unwrap();
        let expected = if cfg!(windows) { r"U:\Media\x.mp4" } else { "U:/Media/x.mp4" };
        assert_eq!(p.to_native(), PathBuf::from(expected));
        let r = WirePath::parse_style("U:", true).unwrap();
        let expected = if cfg!(windows) { r"U:\" } else { "U:/" };
        assert_eq!(r.to_native(), PathBuf::from(expected));
        let unc = WirePath::parse_style("//nas/share/a", true).unwrap();
        let expected = if cfg!(windows) { r"\\nas\share\a" } else { "//nas/share/a" };
        assert_eq!(unc.to_native(), PathBuf::from(expected));
    }

    #[test]
    fn native_to_wire_strips_verbatim() {
        assert_eq!(native_to_wire_windows(r"\\?\U:\Media\x.mp4"), "U:/Media/x.mp4");
        assert_eq!(native_to_wire_windows(r"\\?\UNC\nas\share\x"), "//nas/share/x");
        assert_eq!(native_to_wire_windows(r"C:\ProgramData\stash-fsd"), "C:/ProgramData/stash-fsd");
    }

    #[test]
    fn names_join_parent() {
        let p = WirePath::parse_style("U:/Media/x.mp4", true).unwrap();
        assert_eq!(p.name(), "x.mp4");
        assert_eq!(p.parent().unwrap().to_wire(), "U:/Media");
        assert_eq!(p.parent().unwrap().parent().unwrap().to_wire(), "U:/");
        assert!(p.parent().unwrap().parent().unwrap().parent().is_none());
        let r = WirePath::parse_style("U:/", true).unwrap();
        assert_eq!(r.name(), "U:/");
        assert_eq!(r.join("a").join("b").to_wire(), "U:/a/b");
        let unc = WirePath::parse_style("//nas/share", true).unwrap();
        assert_eq!(unc.join("x").to_wire(), "//nas/share/x");
    }

    #[test]
    fn unix_paths() {
        assert_eq!(u("/tmp/x").unwrap(), "/tmp/x");
        assert_eq!(u("/tmp//a/./b/../c/").unwrap(), "/tmp/a/c");
        assert_eq!(u("/").unwrap(), "/");
        assert!(matches!(u("tmp/x"), Err(PathError::OutsideRoots(_))));
        assert!(matches!(u("/.."), Err(PathError::OutsideRoots(_))));
        let p = WirePath::parse_style("/tmp/x", false).unwrap();
        assert_eq!(p.to_native(), PathBuf::from("/tmp/x"));
        assert_eq!(p.join("y").to_wire(), "/tmp/x/y");
    }

    fn roots(list: &[&str]) -> Roots {
        Roots::with_style(&list.iter().map(|s| s.to_string()).collect::<Vec<_>>(), true, true).unwrap()
    }

    #[test]
    fn roots_case_insensitive_boundaries() {
        let r = roots(&["U:/", "D:/Media", "//nas/Share/Lib"]);
        assert!(r.check("U:/anything/x.mp4").is_ok());
        assert!(r.check("u:/anything").is_ok());
        assert!(r.check("d:/media/x.mp4").is_ok());
        assert!(r.check("D:/MEDIA").is_ok());
        assert!(r.check(r"D:\Media\sub\x").is_ok());
        assert!(matches!(r.check("D:/MediaOther/x"), Err(PathError::OutsideRoots(_))));
        assert!(matches!(r.check("D:/Medi"), Err(PathError::OutsideRoots(_))));
        assert!(matches!(r.check("D:/"), Err(PathError::OutsideRoots(_))));
        assert!(matches!(r.check("D:/Media/../Secret"), Err(PathError::OutsideRoots(_))));
        assert!(matches!(r.check("C:/Windows"), Err(PathError::OutsideRoots(_))));
        assert!(r.check("//NAS/share/lib/x").is_ok());
        assert!(matches!(r.check("//nas/share/other"), Err(PathError::OutsideRoots(_))));
        assert!(matches!(r.check("//nas/other/Lib"), Err(PathError::OutsideRoots(_))));
        assert!(matches!(r.check("relative"), Err(PathError::OutsideRoots(_))));
    }

    #[test]
    fn roots_unicode_case() {
        let r = roots(&["D:/Médias"]);
        assert!(r.check("D:/MÉDIAS/x").is_ok());
    }

    #[test]
    fn roots_case_sensitive_mode() {
        let r = Roots::with_style(&["/srv/Media".to_string()], false, false).unwrap();
        assert!(r.check("/srv/Media/x").is_ok());
        assert!(r.check("/srv/media/x").is_err());
        assert!(r.check("/srv/Media/../x").is_err());
    }

    #[test]
    fn is_root_detection() {
        let r = roots(&["D:/Media"]);
        assert!(r.is_root(&WirePath::parse_style("d:/media/", true).unwrap()));
        assert!(!r.is_root(&WirePath::parse_style("D:/Media/x", true).unwrap()));
        assert_eq!(r.list(), vec!["D:/Media".to_string()]);
    }

    #[test]
    fn invalid_root_rejected() {
        assert!(Roots::with_style(&["relative".to_string()], true, true).is_err());
        assert!(Roots::with_style(&[], true, true).is_err());
    }

    #[test]
    fn path_error_status_codes() {
        let s: tonic::Status = PathError::OutsideRoots("x".into()).into();
        assert_eq!(s.code(), tonic::Code::PermissionDenied);
        let s: tonic::Status = PathError::Invalid("x".into()).into();
        assert_eq!(s.code(), tonic::Code::InvalidArgument);
    }
}
