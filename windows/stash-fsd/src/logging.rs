//! Console + daily rolling file logs.

use tracing_appender::non_blocking::WorkerGuard;
use tracing_subscriber::layer::SubscriberExt;
use tracing_subscriber::util::SubscriberInitExt;
use tracing_subscriber::{fmt, EnvFilter, Layer};

use crate::config::Config;
use crate::paths;

/// Initialises logging. Keep the returned guard alive until exit so file
/// logs are flushed. `RUST_LOG` overrides `[log] level`.
pub fn init(cfg: &Config, console: bool) -> Option<WorkerGuard> {
    let filter = || {
        EnvFilter::try_from_default_env()
            .unwrap_or_else(|_| EnvFilter::new(format!("{},h2=warn,hyper=warn,tower=warn", cfg.log.level)))
    };

    let log_dir = paths::to_native_string(&cfg.storage.log_dir);
    let mut file_err = None;
    let (file_layer, guard) = if log_dir.trim().is_empty() {
        (None, None)
    } else {
        match std::fs::create_dir_all(&log_dir) {
            Ok(()) => {
                let appender = tracing_appender::rolling::daily(&log_dir, "stash-fsd.log");
                let (nb, guard) = tracing_appender::non_blocking(appender);
                let layer = fmt::layer().with_ansi(false).with_writer(nb).with_filter(filter());
                (Some(layer), Some(guard))
            }
            Err(e) => {
                file_err = Some(format!("cannot create log_dir {log_dir}: {e}"));
                (None, None)
            }
        }
    };
    let console_layer = console.then(|| fmt::layer().with_writer(std::io::stderr).with_filter(filter()));

    let _ = tracing_subscriber::registry().with(file_layer).with(console_layer).try_init();
    if let Some(e) = file_err {
        tracing::warn!("{e}; logging to console only");
    }
    guard
}
