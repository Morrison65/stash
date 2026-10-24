//! `stash-fsd.toml` loading, defaults and validation.

use std::path::{Path, PathBuf};

use anyhow::{bail, Context, Result};
use serde::{Deserialize, Serialize};

use crate::paths::{self, Roots};

pub const CONFIG_FILE_NAME: &str = "stash-fsd.toml";
const PLACEHOLDER_SECRET_PREFIX: &str = "change-me";

#[derive(Debug, Clone, Serialize, Deserialize)]
#[serde(default, deny_unknown_fields)]
pub struct Config {
    pub server: ServerConfig,
    pub access: AccessConfig,
    pub tools: ToolsConfig,
    pub storage: StorageConfig,
    pub limits: LimitsConfig,
    pub log: LogConfig,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
#[serde(default, deny_unknown_fields)]
pub struct ServerConfig {
    pub grpc_listen: String,
    pub http_listen: String,
    pub shared_secret: String,
}

#[derive(Debug, Clone, Serialize, Deserialize, Default)]
#[serde(default, deny_unknown_fields)]
pub struct AccessConfig {
    pub roots: Vec<String>,
}

#[derive(Debug, Clone, Serialize, Deserialize, Default)]
#[serde(default, deny_unknown_fields)]
pub struct ToolsConfig {
    pub ffmpeg: String,
    pub ffprobe: String,
    pub python: String,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
#[serde(default, deny_unknown_fields)]
pub struct StorageConfig {
    pub workspace_dir: String,
    pub log_dir: String,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
#[serde(default, deny_unknown_fields)]
pub struct LimitsConfig {
    pub max_concurrent_ffmpeg: u32,
    pub max_concurrent_hash: u32,
    pub ffmpeg_timeout_seconds: u64,
    pub walk_batch_size: u32,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
#[serde(default, deny_unknown_fields)]
pub struct LogConfig {
    pub level: String,
}

impl Default for Config {
    fn default() -> Self {
        Config {
            server: ServerConfig::default(),
            access: AccessConfig::default(),
            tools: ToolsConfig::default(),
            storage: StorageConfig::default(),
            limits: LimitsConfig::default(),
            log: LogConfig::default(),
        }
    }
}

impl Default for ServerConfig {
    fn default() -> Self {
        ServerConfig {
            grpc_listen: "0.0.0.0:7700".into(),
            http_listen: "0.0.0.0:7701".into(),
            shared_secret: String::new(),
        }
    }
}

impl Default for StorageConfig {
    fn default() -> Self {
        let base = program_data_dir().join("stash-fsd");
        StorageConfig {
            workspace_dir: base.join("work").to_string_lossy().into_owned(),
            log_dir: base.join("logs").to_string_lossy().into_owned(),
        }
    }
}

impl Default for LimitsConfig {
    fn default() -> Self {
        LimitsConfig {
            max_concurrent_ffmpeg: 4,
            max_concurrent_hash: 8,
            ffmpeg_timeout_seconds: 3600,
            walk_batch_size: 512,
        }
    }
}

impl Default for LogConfig {
    fn default() -> Self {
        LogConfig {
            level: "info".into(),
        }
    }
}

/// `%ProgramData%` (Windows) or `/var/lib` elsewhere.
pub fn program_data_dir() -> PathBuf {
    match std::env::var_os("ProgramData").or_else(|| std::env::var_os("PROGRAMDATA")) {
        Some(p) if !p.is_empty() => PathBuf::from(p),
        _ => {
            if cfg!(windows) {
                PathBuf::from(r"C:\ProgramData")
            } else {
                PathBuf::from("/var/lib")
            }
        }
    }
}

/// Config lookup order: explicit path, next to the executable, then
/// `%ProgramData%\stash-fsd\stash-fsd.toml`.
pub fn candidate_paths(explicit: Option<&Path>) -> Vec<PathBuf> {
    if let Some(p) = explicit {
        return vec![p.to_path_buf()];
    }
    let mut out = Vec::new();
    if let Ok(exe) = std::env::current_exe() {
        if let Some(dir) = exe.parent() {
            out.push(dir.join(CONFIG_FILE_NAME));
        }
    }
    out.push(program_data_dir().join("stash-fsd").join(CONFIG_FILE_NAME));
    out
}

/// Finds the config file to use; `None` when nothing exists (only valid when
/// no explicit path was given).
pub fn locate(explicit: Option<&Path>) -> Result<Option<PathBuf>> {
    if let Some(p) = explicit {
        if !p.is_file() {
            bail!("config file {} does not exist", p.display());
        }
        return Ok(Some(p.to_path_buf()));
    }
    Ok(candidate_paths(None).into_iter().find(|p| p.is_file()))
}

impl Config {
    pub fn parse(text: &str) -> Result<Config> {
        let cfg: Config = toml::from_str(text).context("parsing config")?;
        Ok(cfg)
    }

    pub fn load(path: &Path) -> Result<Config> {
        let text = std::fs::read_to_string(path)
            .with_context(|| format!("reading config {}", path.display()))?;
        Config::parse(&text).with_context(|| format!("in {}", path.display()))
    }

    /// Loads the config using the lookup order; defaults when nothing is found.
    pub fn load_effective(explicit: Option<&Path>) -> Result<(Config, Option<PathBuf>)> {
        match locate(explicit)? {
            Some(p) => Ok((Config::load(&p)?, Some(p))),
            None => Ok((Config::default(), None)),
        }
    }

    /// Checks everything that must be right before the service starts.
    pub fn validate(&self) -> Result<Roots> {
        let secret = self.server.shared_secret.trim();
        if secret.is_empty() {
            bail!("server.shared_secret is empty; set a long random secret");
        }
        if secret.starts_with(PLACEHOLDER_SECRET_PREFIX) {
            bail!("server.shared_secret still has the placeholder value; set a long random secret");
        }
        if secret.len() < 16 {
            bail!("server.shared_secret is too short (minimum 16 characters)");
        }
        if self.access.roots.is_empty() {
            bail!("access.roots is empty; nothing would be accessible");
        }
        self.server
            .grpc_listen
            .parse::<std::net::SocketAddr>()
            .with_context(|| format!("server.grpc_listen {:?}", self.server.grpc_listen))?;
        self.server
            .http_listen
            .parse::<std::net::SocketAddr>()
            .with_context(|| format!("server.http_listen {:?}", self.server.http_listen))?;
        if self.limits.max_concurrent_ffmpeg == 0 || self.limits.max_concurrent_hash == 0 {
            bail!("limits.max_concurrent_* must be at least 1");
        }
        if self.storage.workspace_dir.trim().is_empty() {
            bail!("storage.workspace_dir is empty");
        }
        Roots::new(&self.access.roots, paths::host_case_insensitive())
    }

    pub fn ffmpeg_path(&self) -> String {
        tool_or_default(&self.tools.ffmpeg, "ffmpeg")
    }

    pub fn ffprobe_path(&self) -> String {
        tool_or_default(&self.tools.ffprobe, "ffprobe")
    }

    pub fn walk_batch_size(&self) -> usize {
        self.limits.walk_batch_size.clamp(1, 65536) as usize
    }

    /// TOML rendering with the secret redacted.
    pub fn redacted_toml(&self) -> String {
        let mut c = self.clone();
        let n = c.server.shared_secret.len();
        c.server.shared_secret = if n == 0 {
            String::new()
        } else {
            format!("<redacted, {n} characters>")
        };
        toml::to_string_pretty(&c).unwrap_or_default()
    }
}

fn tool_or_default(v: &str, name: &str) -> String {
    let v = v.trim();
    if v.is_empty() {
        name.to_string()
    } else {
        paths::to_native_string(v)
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn valid() -> Config {
        let mut c = Config::default();
        c.server.shared_secret = "0123456789abcdef0123".into();
        c.access.roots = vec!["/media".into()];
        c
    }

    #[test]
    fn parses_example() {
        let text = include_str!("../stash-fsd.example.toml");
        let c = Config::parse(text).unwrap();
        assert_eq!(c.server.grpc_listen, "0.0.0.0:7700");
        assert_eq!(c.limits.walk_batch_size, 512);
        // the example ships with the placeholder secret and must be refused
        assert!(c.validate().is_err());
    }

    #[test]
    fn defaults_fill_missing_sections() {
        let c = Config::parse("[server]\nshared_secret = \"x\"\n").unwrap();
        assert_eq!(c.server.http_listen, "0.0.0.0:7701");
        assert_eq!(c.limits.max_concurrent_ffmpeg, 4);
        assert_eq!(c.limits.max_concurrent_hash, 8);
        assert_eq!(c.log.level, "info");
    }

    #[test]
    fn rejects_unknown_keys() {
        assert!(Config::parse("[server]\nbogus = 1\n").is_err());
    }

    #[test]
    fn secret_validation() {
        let mut c = valid();
        assert!(c.validate().is_ok());
        c.server.shared_secret = "".into();
        assert!(c.validate().is_err());
        c.server.shared_secret = "change-me-to-a-long-random-string".into();
        assert!(c.validate().is_err());
        c.server.shared_secret = "short".into();
        assert!(c.validate().is_err());
    }

    #[test]
    fn roots_required() {
        let mut c = valid();
        c.access.roots.clear();
        assert!(c.validate().is_err());
    }

    #[test]
    fn redacts_secret() {
        let c = valid();
        let s = c.redacted_toml();
        assert!(!s.contains("0123456789abcdef0123"));
        assert!(s.contains("redacted"));
    }
}
