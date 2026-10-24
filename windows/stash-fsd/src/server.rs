//! Starts the gRPC and HTTP servers and runs them until shutdown.

use std::future::Future;
use std::net::SocketAddr;
use std::time::Duration;

use anyhow::{Context, Result};
use tokio::net::TcpListener;
use tokio::sync::watch;
use tokio_stream::wrappers::TcpListenerStream;
use tonic::service::interceptor::InterceptedService;

use crate::config::Config;
use crate::fs_service::{FsService, HashService};
use crate::media::{self, MediaService};
use crate::pb::file_system_server::FileSystemServer;
use crate::pb::hasher_server::HasherServer;
use crate::pb::media_server::MediaServer;
use crate::state::{AppState, BearerAuth};

const MAX_DECODE: usize = 64 << 20;
const MAX_ENCODE: usize = 64 << 20;
const WORKSPACE_MAX_AGE: Duration = Duration::from_secs(24 * 3600);

pub async fn run(cfg: Config, shutdown: impl Future<Output = ()> + Send + 'static) -> Result<()> {
    let roots = cfg.validate()?;
    let grpc_addr: SocketAddr = cfg.server.grpc_listen.parse()?;
    let http_addr: SocketAddr = cfg.server.http_listen.parse()?;
    let st = AppState::new(cfg, roots);

    std::fs::create_dir_all(&st.workspace_dir)
        .with_context(|| format!("creating workspace_dir {}", st.workspace_dir.display()))?;

    let grpc_listener = TcpListener::bind(grpc_addr)
        .await
        .with_context(|| format!("binding gRPC listener {grpc_addr}"))?;
    let http_listener = TcpListener::bind(http_addr)
        .await
        .with_context(|| format!("binding HTTP listener {http_addr}"))?;

    let (stop_tx, stop_rx) = watch::channel(false);
    tokio::spawn(async move {
        shutdown.await;
        tracing::info!("shutdown requested");
        let _ = stop_tx.send(true);
    });
    let stopped = |mut rx: watch::Receiver<bool>| async move {
        let _ = rx.wait_for(|v| *v).await;
    };

    // Periodically remove abandoned workspaces.
    {
        let dir = st.workspace_dir.clone();
        let mut rx = stop_rx.clone();
        tokio::spawn(async move {
            loop {
                let d = dir.clone();
                if let Ok(n) = tokio::task::spawn_blocking(move || media::sweep_stale_workspaces(&d, WORKSPACE_MAX_AGE)).await {
                    if n > 0 {
                        tracing::info!(removed = n, "removed stale workspaces");
                    }
                }
                tokio::select! {
                    _ = tokio::time::sleep(Duration::from_secs(3600)) => {}
                    _ = rx.wait_for(|v| *v) => return,
                }
            }
        });
    }

    let auth = BearerAuth::new(st.secret());
    let fs = FileSystemServer::new(FsService::new(st.clone()))
        .max_decoding_message_size(MAX_DECODE)
        .max_encoding_message_size(MAX_ENCODE);
    let hasher = HasherServer::new(HashService::new(st.clone()))
        .max_decoding_message_size(MAX_DECODE)
        .max_encoding_message_size(MAX_ENCODE);
    let media = MediaServer::new(MediaService::new(st.clone()))
        .max_decoding_message_size(MAX_DECODE)
        .max_encoding_message_size(MAX_ENCODE);

    let grpc = tonic::transport::Server::builder()
        .tcp_nodelay(true)
        .tcp_keepalive(Some(Duration::from_secs(60)))
        .http2_keepalive_interval(Some(Duration::from_secs(30)))
        .http2_keepalive_timeout(Some(Duration::from_secs(20)))
        .add_service(InterceptedService::new(fs, auth.clone()))
        .add_service(InterceptedService::new(hasher, auth.clone()))
        .add_service(InterceptedService::new(media, auth))
        .serve_with_incoming_shutdown(TcpListenerStream::new(grpc_listener), stopped(stop_rx.clone()));

    let http = axum::serve(http_listener, crate::http::router(st.clone()).into_make_service())
        .with_graceful_shutdown(stopped(stop_rx.clone()));

    tracing::info!(
        grpc = %grpc_addr,
        http = %http_addr,
        roots = ?st.roots.list(),
        workspace = %st.workspace_dir.display(),
        version = env!("CARGO_PKG_VERSION"),
        "stash-fsd listening"
    );

    let (g, h) = tokio::join!(grpc, async { http.await });
    g.context("gRPC server")?;
    h.context("HTTP server")?;
    tracing::info!("stash-fsd stopped");
    Ok(())
}
