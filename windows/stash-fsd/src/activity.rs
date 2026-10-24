//! Activity log: what Stash asks of the service, visible in the console.
//!
//! Changes to the library (mkdir, rename/move, remove, trash) and every
//! ffmpeg/ffprobe run are logged one by one. Frequent operations (stat,
//! directory listings, reads, hashing, probes, playback requests) are counted
//! and summarised once a minute, so a library scan does not flood the log.

use std::sync::atomic::{AtomicU64, Ordering::Relaxed};
use std::time::Duration;

use tokio::sync::watch;

pub struct Counter(AtomicU64);

impl Counter {
    const fn new() -> Self {
        Counter(AtomicU64::new(0))
    }

    pub fn add(&self, n: u64) {
        self.0.fetch_add(n, Relaxed);
    }

    fn take(&self) -> u64 {
        self.0.swap(0, Relaxed)
    }
}

pub static STAT: Counter = Counter::new();
pub static READDIR: Counter = Counter::new();
pub static WALK: Counter = Counter::new();
pub static READ: Counter = Counter::new();
pub static READ_BYTES: Counter = Counter::new();
pub static HASH: Counter = Counter::new();
pub static PROBE: Counter = Counter::new();
pub static STREAM: Counter = Counter::new();

/// The counts since the last call, or None when nothing happened.
pub fn summary() -> Option<String> {
    let counts = [
        ("stat", STAT.take()),
        ("list", READDIR.take()),
        ("scan", WALK.take()),
        ("read", READ.take()),
        ("hash", HASH.take()),
        ("probe", PROBE.take()),
        ("stream", STREAM.take()),
    ];
    let bytes = READ_BYTES.take();
    let parts: Vec<String> = counts.iter().filter(|(_, n)| *n > 0).map(|(k, n)| format!("{k}={n}")).collect();
    if parts.is_empty() {
        return None;
    }
    let mut s = parts.join(" ");
    if bytes > 0 {
        s.push_str(&format!(" read_mb={:.1}", bytes as f64 / 1048576.0));
    }
    Some(s)
}

/// Logs the summary every `every` until `stop` turns true.
pub async fn report(every: Duration, mut stop: watch::Receiver<bool>) {
    loop {
        tokio::select! {
            _ = tokio::time::sleep(every) => {}
            _ = stop.wait_for(|v| *v) => return,
        }
        if let Some(s) = summary() {
            tracing::info!("activity in the last {}s: {s}", every.as_secs());
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn summary_counts_and_resets() {
        STAT.add(3);
        READ.add(2);
        READ_BYTES.add(3 * 1048576);
        let s = summary().unwrap();
        assert!(s.contains("stat=3") && s.contains("read=2") && s.contains("read_mb=3.0"), "{s}");
        assert!(!s.contains("hash"));
        assert!(summary().is_none());
    }
}
