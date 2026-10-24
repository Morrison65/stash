//! Shared service state and the bearer-token interceptor.

use std::path::PathBuf;
use std::sync::Arc;

use subtle::ConstantTimeEq;
use tokio::sync::Semaphore;
use tonic::{Request, Status};

use crate::config::Config;
use crate::paths::{self, Roots};
use crate::prio::PrioritySemaphore;

pub struct AppState {
    pub cfg: Config,
    pub roots: Roots,
    pub hash_sem: Arc<Semaphore>,
    pub ffmpeg_sem: Arc<PrioritySemaphore>,
    pub workspace_dir: PathBuf,
}

impl AppState {
    pub fn new(cfg: Config, roots: Roots) -> Arc<AppState> {
        let hash_sem = Arc::new(Semaphore::new(cfg.limits.max_concurrent_hash.max(1) as usize));
        let ffmpeg_sem = PrioritySemaphore::new(cfg.limits.max_concurrent_ffmpeg.max(1) as usize);
        let workspace_dir = PathBuf::from(paths::to_native_string(&cfg.storage.workspace_dir));
        Arc::new(AppState {
            cfg,
            roots,
            hash_sem,
            ffmpeg_sem,
            workspace_dir,
        })
    }

    pub fn secret(&self) -> &str {
        self.cfg.server.shared_secret.trim()
    }
}

/// Constant-time comparison of two byte strings (length is not hidden).
pub fn ct_eq(a: &[u8], b: &[u8]) -> bool {
    a.len() == b.len() && bool::from(a.ct_eq(b))
}

/// tonic interceptor requiring `authorization: Bearer <secret>`.
#[derive(Clone)]
pub struct BearerAuth {
    expected: Arc<Vec<u8>>,
}

impl BearerAuth {
    pub fn new(secret: &str) -> BearerAuth {
        BearerAuth {
            expected: Arc::new(format!("Bearer {secret}").into_bytes()),
        }
    }

    pub fn check(&self, header: Option<&[u8]>) -> Result<(), Status> {
        match header {
            Some(v) if ct_eq(v, &self.expected) => Ok(()),
            Some(_) => Err(Status::unauthenticated("invalid bearer token")),
            None => Err(Status::unauthenticated("missing authorization metadata")),
        }
    }
}

impl tonic::service::Interceptor for BearerAuth {
    fn call(&mut self, req: Request<()>) -> Result<Request<()>, Status> {
        let header = req.metadata().get("authorization").map(|v| v.as_bytes());
        self.check(header)?;
        Ok(req)
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use tonic::service::Interceptor;

    #[test]
    fn constant_time_eq() {
        assert!(ct_eq(b"abc", b"abc"));
        assert!(!ct_eq(b"abc", b"abd"));
        assert!(!ct_eq(b"abc", b"abcd"));
    }

    #[test]
    fn bearer_interceptor() {
        let mut a = BearerAuth::new("s3cret-s3cret-s3cret");
        let mut ok = Request::new(());
        ok.metadata_mut().insert("authorization", "Bearer s3cret-s3cret-s3cret".parse().unwrap());
        assert!(a.call(ok).is_ok());

        let mut bad = Request::new(());
        bad.metadata_mut().insert("authorization", "Bearer nope".parse().unwrap());
        assert_eq!(a.call(bad).unwrap_err().code(), tonic::Code::Unauthenticated);

        let mut raw = Request::new(());
        raw.metadata_mut().insert("authorization", "s3cret-s3cret-s3cret".parse().unwrap());
        assert_eq!(a.call(raw).unwrap_err().code(), tonic::Code::Unauthenticated);

        assert_eq!(a.call(Request::new(())).unwrap_err().code(), tonic::Code::Unauthenticated);
    }
}
