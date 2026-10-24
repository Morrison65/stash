//! stash-fsd: the Windows service that owns all access to the Stash media
//! library. See docs/architecture/stash-fsd.md.

use std::path::PathBuf;
use std::process::ExitCode;

use anyhow::{bail, Result};
use clap::{Parser, Subcommand};

mod activity;
mod config;
mod errors;
mod fileinfo;
mod fs_service;
mod hash;
mod http;
mod logging;
mod media;
mod ops;
mod paths;
mod prio;
mod server;
mod state;
mod walk;
#[cfg(windows)]
mod winsvc;

pub mod pb {
    #![allow(clippy::all)]
    tonic::include_proto!("stashfs.v1");
}

#[derive(Parser)]
#[command(name = "stash-fsd", version, about = "Stash filesystem service (gRPC + signed HTTP raw serving)")]
struct Cli {
    #[command(subcommand)]
    cmd: Cmd,
}

#[derive(Subcommand)]
enum Cmd {
    /// Run in the console (Ctrl+C to stop).
    Run {
        /// Config file (default: next to the executable, then %ProgramData%\stash-fsd\stash-fsd.toml).
        #[arg(long)]
        config: Option<PathBuf>,
    },
    /// Register the Windows service "stash-fsd" (automatic start). Needs an elevated prompt.
    Install {
        /// Config file passed to the service; validated before installing.
        #[arg(long)]
        config: Option<PathBuf>,
    },
    /// Stop and remove the Windows service. Needs an elevated prompt.
    Uninstall,
    /// Print the effective configuration (secret redacted).
    PrintConfig {
        #[arg(long)]
        config: Option<PathBuf>,
    },
    /// Print a signed /raw query string for a path (testing / debugging).
    SignRaw {
        #[arg(long)]
        config: Option<PathBuf>,
        /// Wire path, e.g. U:/Media/clip.mp4
        #[arg(long)]
        path: String,
        /// Optional zip entry inside --path.
        #[arg(long, default_value = "")]
        zip: String,
        /// Validity in seconds.
        #[arg(long, default_value_t = 3600)]
        ttl: u64,
    },
    /// Print md5 and oshash of a local file or zip entry (no roots check).
    Hash {
        /// File path (native or forward-slash form).
        file: PathBuf,
        /// Optional zip entry inside the file.
        #[arg(long, default_value = "")]
        zip: String,
    },
    /// Service entrypoint used by the Windows service manager.
    #[command(hide = true)]
    Service {
        #[arg(long)]
        config: Option<PathBuf>,
    },
}

fn main() -> ExitCode {
    let cli = Cli::parse();
    match dispatch(cli.cmd) {
        Ok(()) => ExitCode::SUCCESS,
        Err(e) => {
            eprintln!("stash-fsd: {e:#}");
            tracing::error!("{e:#}");
            ExitCode::FAILURE
        }
    }
}

fn dispatch(cmd: Cmd) -> Result<()> {
    match cmd {
        Cmd::Run { config } => run_console(config),
        Cmd::PrintConfig { config } => {
            let (cfg, path) = config::Config::load_effective(config.as_deref())?;
            match &path {
                Some(p) => println!("# config file: {}", p.display()),
                None => {
                    println!("# no config file found; defaults shown. Looked in:");
                    for c in config::candidate_paths(None) {
                        println!("#   {}", c.display());
                    }
                }
            }
            if let Err(e) = cfg.validate() {
                println!("# WARNING: not startable: {e:#}");
            }
            print!("{}", cfg.redacted_toml());
            Ok(())
        }
        Cmd::SignRaw { config, path, zip, ttl } => {
            let (cfg, _) = config::Config::load_effective(config.as_deref())?;
            cfg.validate()?;
            let now = std::time::SystemTime::now().duration_since(std::time::UNIX_EPOCH)?.as_secs();
            println!("/raw?{}", http::signed_query(cfg.server.shared_secret.trim(), &path, &zip, now + ttl));
            Ok(())
        }
        Cmd::Hash { file, zip } => {
            let abs = std::path::absolute(&file)?;
            let wp = paths::WirePath::parse(&paths::native_to_wire(&abs)).map_err(|e| anyhow::anyhow!("{e}"))?;
            let md5 = fs_service::fingerprint_blocking(&wp, &zip, true, false).map_err(|s| anyhow::anyhow!("{}", s.message()))?;
            let oshash = fs_service::fingerprint_blocking(&wp, &zip, false, true)
                .map(|r| r.oshash)
                .unwrap_or_else(|s| format!("error: {}", s.message()));
            println!("size={} md5={} oshash={}", md5.size, md5.md5, oshash);
            Ok(())
        }
        #[cfg(windows)]
        Cmd::Install { config } => winsvc::install(config),
        #[cfg(windows)]
        Cmd::Uninstall => winsvc::uninstall(),
        #[cfg(windows)]
        Cmd::Service { config } => winsvc::run_dispatcher(config),
        #[cfg(not(windows))]
        Cmd::Install { .. } | Cmd::Uninstall | Cmd::Service { .. } => {
            bail!("Windows service management is only available on Windows")
        }
    }
}

fn run_console(explicit: Option<PathBuf>) -> Result<()> {
    let (cfg, path) = config::Config::load_effective(explicit.as_deref())?;
    let Some(path) = path else {
        let looked: Vec<String> = config::candidate_paths(None).iter().map(|p| p.display().to_string()).collect();
        bail!("no config file found (looked in: {}); use --config", looked.join(", "));
    };
    let _guard = logging::init(&cfg, true);
    tracing::info!(config = %path.display(), "starting");
    let rt = tokio::runtime::Builder::new_multi_thread().enable_all().build()?;
    let res = rt.block_on(server::run(cfg, async {
        let _ = tokio::signal::ctrl_c().await;
    }));
    rt.shutdown_timeout(std::time::Duration::from_secs(5));
    res
}
