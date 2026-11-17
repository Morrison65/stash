//! Connects UNC shares with a configured login (`[[access.unc_login]]`).
//!
//! The service runs as LocalSystem. A NAS outside a domain often refuses the
//! machine account, and logins stored for LocalSystem in the Credential Manager
//! are not always usable. Connecting each share once with `WNetAddConnection2W`
//! (no drive letter) gives the LocalSystem logon session an authenticated SMB
//! connection that every later UNC access of this process reuses.
//! Passwords are never logged.

use std::time::Duration;

use crate::config::UncLogin;

/// How often a login that failed at start is retried (the NAS may still be booting).
pub const RETRY_INTERVAL: Duration = Duration::from_secs(60);

/// `\\server\share` from any form `roots` accepts (`\\s\sh\dir`, `//s/sh`,
/// `UNC:/s/sh/dir`); `None` when it is not a UNC path with server and share.
pub fn share_root(path: &str) -> Option<String> {
    let p = path.trim().replace('/', "\\");
    let rest = if let Some(r) = p.strip_prefix("\\\\") {
        r
    } else if p.len() >= 4 && p[..4].eq_ignore_ascii_case("UNC:") {
        p[4..].trim_start_matches('\\')
    } else {
        return None;
    };
    let mut parts = rest.split('\\').filter(|s| !s.is_empty());
    let server = parts.next()?;
    let share = parts.next()?;
    if server.contains(':') || share.contains(':') {
        return None;
    }
    Some(format!("\\\\{server}\\{share}"))
}

/// Connects every login once; returns the ones that failed (to retry later).
pub fn connect_all(logins: &[UncLogin]) -> Vec<UncLogin> {
    logins.iter().filter(|l| !connect_logged(l)).cloned().collect()
}

/// Retries failed logins every `RETRY_INTERVAL` until all are connected.
pub async fn retry_until_connected(mut pending: Vec<UncLogin>) {
    while !pending.is_empty() {
        tokio::time::sleep(RETRY_INTERVAL).await;
        let batch = std::mem::take(&mut pending);
        pending = tokio::task::spawn_blocking(move || connect_all(&batch))
            .await
            .unwrap_or_default();
    }
}

fn connect_logged(l: &UncLogin) -> bool {
    let Some(share) = share_root(&l.share) else {
        tracing::warn!(share = %l.share, "unc_login: not a UNC share, skipped");
        return true; // validation rejects this; never retry it
    };
    match sys::connect(&share, &l.user, &l.password) {
        Ok(()) => {
            tracing::info!(share = %share, user = %l.user, "unc_login: share connected");
            true
        }
        Err(code) => {
            tracing::warn!(
                share = %share,
                user = %l.user,
                code,
                "unc_login: connecting failed ({}); retrying every {} s",
                sys::describe(code),
                RETRY_INTERVAL.as_secs()
            );
            false
        }
    }
}

#[cfg(windows)]
mod sys {
    use std::ffi::OsStr;
    use std::os::windows::ffi::OsStrExt;
    use std::ptr;

    #[repr(C)]
    #[allow(non_snake_case)]
    struct NETRESOURCEW {
        dwScope: u32,
        dwType: u32,
        dwDisplayType: u32,
        dwUsage: u32,
        lpLocalName: *mut u16,
        lpRemoteName: *mut u16,
        lpComment: *mut u16,
        lpProvider: *mut u16,
    }

    #[link(name = "mpr")]
    extern "system" {
        fn WNetAddConnection2W(
            lpNetResource: *const NETRESOURCEW,
            lpPassword: *const u16,
            lpUserName: *const u16,
            dwFlags: u32,
        ) -> u32;
        fn WNetCancelConnection2W(lpName: *const u16, dwFlags: u32, fForce: i32) -> u32;
    }

    const RESOURCETYPE_DISK: u32 = 1;
    const NO_ERROR: u32 = 0;
    /// An existing connection to the server uses other credentials.
    const ERROR_SESSION_CREDENTIAL_CONFLICT: u32 = 1219;

    fn wide(s: &str) -> Vec<u16> {
        OsStr::new(s).encode_wide().chain(Some(0)).collect()
    }

    fn add(share: &[u16], user: &[u16], password: &[u16]) -> u32 {
        let mut remote = share.to_vec();
        let res = NETRESOURCEW {
            dwScope: 0,
            dwType: RESOURCETYPE_DISK,
            dwDisplayType: 0,
            dwUsage: 0,
            lpLocalName: ptr::null_mut(),
            lpRemoteName: remote.as_mut_ptr(),
            lpComment: ptr::null_mut(),
            lpProvider: ptr::null_mut(),
        };
        // SAFETY: all pointers are valid NUL-terminated UTF-16 buffers that outlive the call.
        unsafe { WNetAddConnection2W(&res, password.as_ptr(), user.as_ptr(), 0) }
    }

    pub fn connect(share: &str, user: &str, password: &str) -> Result<(), u32> {
        let (s, u, p) = (wide(share), wide(user), wide(password));
        let mut code = add(&s, &u, &p);
        if code == ERROR_SESSION_CREDENTIAL_CONFLICT {
            // A connection with other (e.g. stale) credentials exists: drop it, retry once.
            // SAFETY: `s` is a valid NUL-terminated UTF-16 buffer.
            unsafe { WNetCancelConnection2W(s.as_ptr(), 0, 1) };
            code = add(&s, &u, &p);
        }
        if code == NO_ERROR {
            Ok(())
        } else {
            Err(code)
        }
    }

    pub fn describe(code: u32) -> String {
        std::io::Error::from_raw_os_error(code as i32).to_string()
    }
}

#[cfg(not(windows))]
mod sys {
    /// Development builds on Linux: nothing to connect.
    pub fn connect(_share: &str, _user: &str, _password: &str) -> Result<(), u32> {
        Ok(())
    }

    pub fn describe(code: u32) -> String {
        format!("os error {code}")
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn share_root_accepts_root_forms() {
        let want = Some(r"\\NAS\Pool".to_string());
        assert_eq!(share_root(r"\\NAS\Pool"), want);
        assert_eq!(share_root(r"\\NAS\Pool\StashLib\sub"), want);
        assert_eq!(share_root("//NAS/Pool/StashLib"), want);
        assert_eq!(share_root("UNC:/NAS/Pool/StashLib"), want);
        assert_eq!(share_root("unc://NAS/Pool"), want);
        assert_eq!(share_root(r"  \\NAS\Pool\  "), want);
    }

    #[test]
    fn share_root_rejects_non_unc() {
        assert_eq!(share_root("C:/media"), None);
        assert_eq!(share_root(r"C:\media"), None);
        assert_eq!(share_root(r"\\NAS"), None);
        assert_eq!(share_root(r"\\NAS\"), None);
        assert_eq!(share_root(""), None);
        assert_eq!(share_root("/media"), None);
    }

    #[test]
    fn connect_all_reports_nothing_failed_off_windows() {
        if cfg!(windows) {
            return; // would really connect
        }
        let l = UncLogin { share: r"\\NAS\Pool".into(), user: "u".into(), password: "p".into() };
        assert!(connect_all(&[l]).is_empty());
    }
}
