//! gRPC Media service: ffprobe / ffmpeg on Windows plus workspaces.

use std::path::{Path, PathBuf};
use std::process::Stdio;
use std::sync::Arc;
use std::time::{Duration, SystemTime};

use futures::StreamExt;
use tokio::io::{AsyncReadExt, AsyncWriteExt};
use tokio::process::Command;
use tokio::sync::{mpsc, OnceCell};
use tokio_stream::wrappers::ReceiverStream;
use tokio_util::io::ReaderStream;
use tonic::{Request, Response, Status};

use crate::errors::io_status;
use crate::fileinfo::{file_info_named, mtime_nanos};
use crate::fs_service::{blocking, BoxStream, STREAM_CHUNK};
use crate::paths::native_to_wire;
use crate::pb::media_server::Media;
use crate::pb::{
    run_output, CapabilitiesResponse, DataChunk, Empty, ListWorkspaceResponse, ProbeRequest, ProbeResponse,
    RunOutput, RunRequest, RunStatus, Tool, WorkspaceFileRequest, WorkspaceRequest,
};
use crate::prio::Class;
use crate::state::AppState;

pub const STDERR_TAIL: usize = 64 * 1024;
const STDOUT_CHUNK: usize = 256 * 1024;
const TOOL_INFO_TIMEOUT: Duration = Duration::from_secs(20);
#[cfg(windows)]
const CREATE_NO_WINDOW: u32 = 0x0800_0000;

/// `[A-Za-z0-9_-]{1,64}`.
pub fn valid_workspace_id(id: &str) -> bool {
    !id.is_empty() && id.len() <= 64 && id.bytes().all(|b| b.is_ascii_alphanumeric() || b == b'_' || b == b'-')
}

/// Validates a relative file name inside a workspace and returns its parts.
pub fn workspace_relative(name: &str) -> Result<Vec<&str>, Status> {
    let parts: Vec<&str> = name.split(['/', '\\']).filter(|p| !p.is_empty()).collect();
    if parts.is_empty() {
        return Err(Status::invalid_argument("empty workspace file name"));
    }
    for p in &parts {
        if *p == "." || *p == ".." || p.contains(':') || p.contains('\0') {
            return Err(Status::invalid_argument(format!("invalid workspace file name {name:?}")));
        }
    }
    if name.starts_with('/') || name.starts_with('\\') {
        return Err(Status::invalid_argument(format!("workspace file name must be relative: {name:?}")));
    }
    Ok(parts)
}

/// Replaces `{input:N}` and `{workspace}` in `args`. Substituted values are
/// not rescanned.
pub fn substitute_args(args: &[String], inputs: &[String], workspace: Option<&str>) -> Result<Vec<String>, Status> {
    args.iter().map(|a| substitute_one(a, inputs, workspace)).collect()
}

fn substitute_one(arg: &str, inputs: &[String], workspace: Option<&str>) -> Result<String, Status> {
    let mut out = String::with_capacity(arg.len());
    let mut rest = arg;
    while let Some(i) = rest.find('{') {
        out.push_str(&rest[..i]);
        let tail = &rest[i..];
        if let Some(after) = tail.strip_prefix("{workspace}") {
            let ws = workspace.ok_or_else(|| Status::invalid_argument("{workspace} used without a workspace"))?;
            out.push_str(ws);
            rest = after;
        } else if let Some(after) = tail.strip_prefix("{input:") {
            let end = after
                .find('}')
                .ok_or_else(|| Status::invalid_argument(format!("unterminated placeholder in {arg:?}")))?;
            let n: usize = after[..end]
                .parse()
                .map_err(|_| Status::invalid_argument(format!("bad input index in {arg:?}")))?;
            let v = inputs
                .get(n)
                .ok_or_else(|| Status::invalid_argument(format!("{{input:{n}}} but only {} inputs", inputs.len())))?;
            out.push_str(v);
            rest = &after[end + 1..];
        } else {
            out.push('{');
            rest = &tail[1..];
        }
    }
    out.push_str(rest);
    Ok(out)
}

/// Keeps the last `cap` bytes written.
pub struct TailBuffer {
    buf: Vec<u8>,
    cap: usize,
}

impl TailBuffer {
    pub fn new(cap: usize) -> TailBuffer {
        TailBuffer {
            buf: Vec::new(),
            cap,
        }
    }

    pub fn push(&mut self, data: &[u8]) {
        if data.len() >= self.cap {
            self.buf.clear();
            self.buf.extend_from_slice(&data[data.len() - self.cap..]);
            return;
        }
        self.buf.extend_from_slice(data);
        if self.buf.len() > self.cap {
            let excess = self.buf.len() - self.cap;
            self.buf.drain(..excess);
        }
    }

    pub fn into_string(self) -> String {
        String::from_utf8_lossy(&self.buf).into_owned()
    }
}

pub fn tool_command(program: &str) -> Command {
    #[allow(unused_mut)]
    let mut cmd = Command::new(program);
    #[cfg(windows)]
    cmd.creation_flags(CREATE_NO_WINDOW);
    cmd.kill_on_drop(true);
    cmd
}

/// Runs a short informational command and returns stdout (or None).
async fn tool_output(program: &str, args: &[&str]) -> Option<String> {
    let mut cmd = tool_command(program);
    cmd.args(args).stdin(Stdio::null()).stdout(Stdio::piped()).stderr(Stdio::null());
    let out = tokio::time::timeout(TOOL_INFO_TIMEOUT, cmd.output()).await.ok()?.ok()?;
    Some(String::from_utf8_lossy(&out.stdout).into_owned())
}

pub fn parse_encoders(out: &str) -> Vec<String> {
    // lines look like " V....D h264_nvenc           NVIDIA NVENC H.264 encoder"
    out.lines()
        .filter_map(|l| {
            let mut it = l.split_whitespace();
            let flags = it.next()?;
            let name = it.next()?;
            if flags.len() == 6 && flags != "------" && is_hw_name(name) {
                Some(name.to_string())
            } else {
                None
            }
        })
        .collect()
}

pub fn parse_hwaccels(out: &str) -> Vec<String> {
    out.lines()
        .skip_while(|l| !l.to_ascii_lowercase().starts_with("hardware acceleration methods"))
        .skip(1)
        .map(str::trim)
        .filter(|l| !l.is_empty())
        .map(str::to_string)
        .collect()
}

fn is_hw_name(n: &str) -> bool {
    ["nvenc", "qsv", "amf", "vaapi"].iter().any(|k| n.contains(k))
}

#[derive(Clone, Default)]
struct ToolInfo {
    ffmpeg_version: String,
    ffprobe_version: String,
    hardware_codecs: Vec<String>,
}

pub struct MediaService {
    st: Arc<AppState>,
    tools: OnceCell<ToolInfo>,
}

impl MediaService {
    pub fn new(st: Arc<AppState>) -> MediaService {
        MediaService {
            st,
            tools: OnceCell::new(),
        }
    }

    fn workspace_path(&self, id: &str) -> Result<PathBuf, Status> {
        if !valid_workspace_id(id) {
            return Err(Status::invalid_argument(format!("invalid workspace id {id:?} (want [A-Za-z0-9_-]{{1,64}})")));
        }
        Ok(self.st.workspace_dir.join(id))
    }

    async fn tool_info(&self) -> ToolInfo {
        if let Some(t) = self.tools.get() {
            return t.clone();
        }
        let ffmpeg = self.st.cfg.ffmpeg_path();
        let ffprobe = self.st.cfg.ffprobe_path();
        let first_line = |s: Option<String>| s.and_then(|s| s.lines().next().map(str::to_string)).unwrap_or_default();
        let info = ToolInfo {
            ffmpeg_version: first_line(tool_output(&ffmpeg, &["-version"]).await),
            ffprobe_version: first_line(tool_output(&ffprobe, &["-version"]).await),
            hardware_codecs: {
                let mut v = Vec::new();
                if let Some(o) = tool_output(&ffmpeg, &["-hide_banner", "-hwaccels"]).await {
                    v.extend(parse_hwaccels(&o).into_iter().map(|h| format!("hwaccel:{h}")));
                }
                if let Some(o) = tool_output(&ffmpeg, &["-hide_banner", "-encoders"]).await {
                    v.extend(parse_encoders(&o));
                }
                v
            },
        };
        if !info.ffmpeg_version.is_empty() {
            let _ = self.tools.set(info.clone());
        }
        info
    }
}

pub fn sweep_stale_workspaces(dir: &Path, max_age: Duration) -> usize {
    let Ok(rd) = std::fs::read_dir(dir) else { return 0 };
    let now = SystemTime::now();
    let mut n = 0;
    for ent in rd.flatten() {
        let Ok(md) = ent.metadata() else { continue };
        let old = md
            .modified()
            .ok()
            .and_then(|m| now.duration_since(m).ok())
            .is_some_and(|age| age > max_age);
        let name = ent.file_name().to_string_lossy().into_owned();
        if md.is_dir() && old && valid_workspace_id(&name) && std::fs::remove_dir_all(ent.path()).is_ok() {
            n += 1;
        }
    }
    n
}

#[tonic::async_trait]
impl Media for MediaService {
    async fn capabilities(&self, _req: Request<Empty>) -> Result<Response<CapabilitiesResponse>, Status> {
        let t = self.tool_info().await;
        Ok(Response::new(CapabilitiesResponse {
            service_version: env!("CARGO_PKG_VERSION").to_string(),
            ffmpeg_version: t.ffmpeg_version,
            ffprobe_version: t.ffprobe_version,
            hardware_codecs: t.hardware_codecs,
            roots: self.st.roots.list(),
            max_concurrent_ffmpeg: self.st.cfg.limits.max_concurrent_ffmpeg as i32,
            max_concurrent_hash: self.st.cfg.limits.max_concurrent_hash as i32,
        }))
    }

    async fn probe(&self, req: Request<ProbeRequest>) -> Result<Response<ProbeResponse>, Status> {
        let r = req.into_inner();
        let p = self.st.roots.check(&r.path)?;
        let native = p.to_native();
        let (size, mtime) = {
            let native = native.clone();
            let w = p.to_wire();
            blocking(move || {
                let md = std::fs::metadata(&native).map_err(|e| io_status("stat", &w, &e))?;
                if md.is_dir() {
                    return Err(Status::failed_precondition(format!("{w} is a directory")));
                }
                Ok((md.len() as i64, mtime_nanos(&md)))
            })
            .await?
        };
        let mut args: Vec<String> = ["-v", "quiet", "-print_format", "json", "-show_format", "-show_streams", "-show_error"]
            .iter()
            .map(|s| s.to_string())
            .collect();
        args.extend(r.extra_args);
        args.push(native.to_string_lossy().into_owned());

        let _permit = self.st.ffmpeg_sem.acquire(Class::Normal).await;
        let mut cmd = tool_command(&self.st.cfg.ffprobe_path());
        cmd.args(&args).stdin(Stdio::null()).stdout(Stdio::piped()).stderr(Stdio::piped());
        let timeout = Duration::from_secs(self.st.cfg.limits.ffmpeg_timeout_seconds.max(1));
        let out = match tokio::time::timeout(timeout, cmd.output()).await {
            Err(_) => return Err(Status::deadline_exceeded(format!("ffprobe timed out on {p}"))),
            Ok(Err(e)) => return Err(Status::failed_precondition(format!("cannot run ffprobe: {e}"))),
            Ok(Ok(o)) => o,
        };
        if out.stdout.is_empty() {
            let mut tail = TailBuffer::new(4096);
            tail.push(&out.stderr);
            return Err(Status::internal(format!(
                "ffprobe failed on {p} ({}): {}",
                out.status,
                tail.into_string()
            )));
        }
        Ok(Response::new(ProbeResponse {
            json: out.stdout,
            size,
            mod_time_unix_nano: mtime,
        }))
    }

    type RunStream = BoxStream<RunOutput>;

    async fn run(&self, req: Request<RunRequest>) -> Result<Response<Self::RunStream>, Status> {
        let r = req.into_inner();
        let program = match Tool::try_from(r.tool) {
            Ok(Tool::Ffmpeg) => self.st.cfg.ffmpeg_path(),
            Ok(Tool::Ffprobe) => self.st.cfg.ffprobe_path(),
            _ => return Err(Status::invalid_argument("tool must be TOOL_FFMPEG or TOOL_FFPROBE")),
        };
        let mut inputs = Vec::with_capacity(r.inputs.len());
        for i in &r.inputs {
            inputs.push(self.st.roots.check(i)?.to_native().to_string_lossy().into_owned());
        }
        let ws_dir = if r.workspace.is_empty() {
            None
        } else {
            let d = self.workspace_path(&r.workspace)?;
            let d2 = d.clone();
            blocking(move || std::fs::create_dir_all(&d2).map_err(|e| io_status("mkdir", &d2.to_string_lossy(), &e)))
                .await?;
            Some(d)
        };
        let ws_str = ws_dir.as_ref().map(|d| d.to_string_lossy().into_owned());
        let args = substitute_args(&r.args, &inputs, ws_str.as_deref())?;
        let timeout = if r.timeout_seconds > 0 {
            Duration::from_secs(r.timeout_seconds as u64)
        } else {
            Duration::from_secs(self.st.cfg.limits.ffmpeg_timeout_seconds.max(1))
        };
        let class = Class::from_proto(r.priority);
        let sem = self.st.ffmpeg_sem.clone();
        let stdin = r.stdin;

        let (tx, rx) = mpsc::channel::<Result<RunOutput, Status>>(8);
        tokio::spawn(async move {
            let _permit = tokio::select! {
                p = sem.acquire(class) => p,
                _ = tx.closed() => return,
            };
            run_process(program, args, ws_dir, stdin, timeout, tx).await;
        });
        Ok(Response::new(Box::pin(ReceiverStream::new(rx))))
    }

    type FetchWorkspaceFileStream = BoxStream<DataChunk>;

    async fn fetch_workspace_file(
        &self,
        req: Request<WorkspaceFileRequest>,
    ) -> Result<Response<Self::FetchWorkspaceFileStream>, Status> {
        let r = req.into_inner();
        let mut path = self.workspace_path(&r.workspace)?;
        for part in workspace_relative(&r.name)? {
            path.push(part);
        }
        let label = format!("{}/{}", r.workspace, r.name);
        let f = tokio::fs::File::open(&path).await.map_err(|e| io_status("open", &label, &e))?;
        let md = f.metadata().await.map_err(|e| io_status("stat", &label, &e))?;
        if md.is_dir() {
            return Err(Status::failed_precondition(format!("{label} is a directory")));
        }
        let stream = ReaderStream::with_capacity(f, STREAM_CHUNK).map(move |res| match res {
            Ok(b) => Ok(DataChunk { data: b.to_vec() }),
            Err(e) => Err(io_status("read", &label, &e)),
        });
        Ok(Response::new(Box::pin(stream)))
    }

    async fn list_workspace(&self, req: Request<WorkspaceRequest>) -> Result<Response<ListWorkspaceResponse>, Status> {
        let id = req.into_inner().workspace;
        let dir = self.workspace_path(&id)?;
        let entries = blocking(move || {
            let list = match crate::walk::read_dir_sorted(&dir) {
                Ok(l) => l,
                Err(e) if e.kind() == std::io::ErrorKind::NotFound => return Ok(Vec::new()),
                Err(e) => return Err(io_status("readdir", &id, &e)),
            };
            Ok(list
                .into_iter()
                .filter_map(|(name, md)| {
                    let md = md.ok()?;
                    let path = native_to_wire(&dir.join(&name));
                    Some(file_info_named(name, path, &md))
                })
                .collect())
        })
        .await?;
        Ok(Response::new(ListWorkspaceResponse { entries }))
    }

    async fn delete_workspace(&self, req: Request<WorkspaceRequest>) -> Result<Response<Empty>, Status> {
        let id = req.into_inner().workspace;
        let dir = self.workspace_path(&id)?;
        blocking(move || crate::ops::remove_all(&dir).map_err(|e| io_status("delete workspace", &id, &e))).await?;
        Ok(Response::new(Empty {}))
    }
}

async fn run_process(
    program: String,
    args: Vec<String>,
    cwd: Option<PathBuf>,
    stdin: Vec<u8>,
    timeout: Duration,
    tx: mpsc::Sender<Result<RunOutput, Status>>,
) {
    let mut cmd = tool_command(&program);
    cmd.args(&args)
        .stdin(if stdin.is_empty() { Stdio::null() } else { Stdio::piped() })
        .stdout(Stdio::piped())
        .stderr(Stdio::piped());
    if let Some(d) = &cwd {
        cmd.current_dir(d);
    }
    tracing::debug!(program = %program, ?args, "run");
    let mut child = match cmd.spawn() {
        Ok(c) => c,
        Err(e) => {
            let code = if e.kind() == std::io::ErrorKind::NotFound {
                tonic::Code::FailedPrecondition
            } else {
                crate::errors::io_code(&e)
            };
            let _ = tx.send(Err(Status::new(code, format!("cannot start {program}: {e}")))).await;
            return;
        }
    };

    if let Some(mut si) = child.stdin.take() {
        tokio::spawn(async move {
            let _ = si.write_all(&stdin).await;
            let _ = si.shutdown().await;
        });
    }
    let mut stderr = child.stderr.take().expect("stderr piped");
    // Shared so the tail can be taken even if a grandchild keeps the pipe open.
    let tail = Arc::new(std::sync::Mutex::new(TailBuffer::new(STDERR_TAIL)));
    let tail_w = tail.clone();
    let mut stderr_task = tokio::spawn(async move {
        let mut buf = vec![0u8; 16 * 1024];
        loop {
            match stderr.read(&mut buf).await {
                Ok(0) | Err(_) => break,
                Ok(n) => tail_w.lock().unwrap().push(&buf[..n]),
            }
        }
    });

    let mut stdout = child.stdout.take().expect("stdout piped");
    let deadline = tokio::time::sleep(timeout);
    tokio::pin!(deadline);
    let mut timed_out = false;
    let mut client_gone = false;
    let mut buf = vec![0u8; STDOUT_CHUNK];
    loop {
        // tx.closed() fires when the client cancels the call (tonic drops
        // the response stream), even while the process writes no stdout.
        let n = tokio::select! {
            r = stdout.read(&mut buf) => match r { Ok(n) => n, Err(_) => 0 },
            _ = &mut deadline => { timed_out = true; break; }
            _ = tx.closed() => { client_gone = true; break; }
        };
        if n == 0 {
            break;
        }
        let msg = Ok(RunOutput {
            payload: Some(run_output::Payload::Stdout(buf[..n].to_vec())),
        });
        tokio::select! {
            r = tx.send(msg) => if r.is_err() { client_gone = true; break; },
            _ = &mut deadline => { timed_out = true; break; }
        }
    }

    let mut status = None;
    if !timed_out && !client_gone {
        tokio::select! {
            s = child.wait() => status = s.ok(),
            _ = &mut deadline => timed_out = true,
            _ = tx.closed() => client_gone = true,
        }
    }
    if status.is_none() {
        let _ = child.start_kill();
        status = child.wait().await.ok();
    }
    if tokio::time::timeout(Duration::from_secs(3), &mut stderr_task).await.is_err() {
        stderr_task.abort();
    }
    let stderr = std::mem::replace(&mut *tail.lock().unwrap(), TailBuffer::new(0)).into_string();
    if client_gone {
        tracing::info!(program = %program, "client went away; process killed");
        return;
    }
    let exit_code = status.and_then(|s| s.code()).unwrap_or(-1);
    let _ = tx
        .send(Ok(RunOutput {
            payload: Some(run_output::Payload::Status(RunStatus {
                exit_code,
                stderr,
                timed_out,
            })),
        }))
        .await;
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn workspace_ids() {
        assert!(valid_workspace_id("abc"));
        assert!(valid_workspace_id("A-b_9"));
        assert!(valid_workspace_id(&"x".repeat(64)));
        assert!(!valid_workspace_id(&"x".repeat(65)));
        assert!(!valid_workspace_id(""));
        assert!(!valid_workspace_id("a/b"));
        assert!(!valid_workspace_id(".."));
        assert!(!valid_workspace_id("a b"));
        assert!(!valid_workspace_id("é"));
        assert!(!valid_workspace_id("C:"));
    }

    #[test]
    fn workspace_names() {
        assert_eq!(workspace_relative("a.jpg").unwrap(), vec!["a.jpg"]);
        assert_eq!(workspace_relative("sub/a.jpg").unwrap(), vec!["sub", "a.jpg"]);
        assert!(workspace_relative("../x").is_err());
        assert!(workspace_relative("a/../../x").is_err());
        assert!(workspace_relative("C:/x").is_err());
        assert!(workspace_relative("/abs").is_err());
        assert!(workspace_relative(r"\abs").is_err());
        assert!(workspace_relative("").is_err());
        assert!(workspace_relative("a:stream").is_err());
    }

    #[test]
    fn placeholders() {
        let inputs = vec![r"U:\Media\a b.mp4".to_string(), r"U:\x.mkv".to_string()];
        let args: Vec<String> = ["-i", "{input:0}", "-i", "{input:1}", "{workspace}/out%03d.jpg", "-vf", "select='eq(n\\,0)'", "{}", "{in"]
            .iter()
            .map(|s| s.to_string())
            .collect();
        let got = substitute_args(&args, &inputs, Some(r"C:\work\ws1")).unwrap();
        assert_eq!(got[1], r"U:\Media\a b.mp4");
        assert_eq!(got[3], r"U:\x.mkv");
        assert_eq!(got[4], r"C:\work\ws1/out%03d.jpg");
        assert_eq!(got[6], "select='eq(n\\,0)'");
        assert_eq!(got[7], "{}");
        assert_eq!(got[8], "{in");
        assert!(substitute_args(&["{input:2}".into()], &inputs, None).is_err());
        assert!(substitute_args(&["{input:x}".into()], &inputs, None).is_err());
        assert!(substitute_args(&["{input:0".into()], &inputs, None).is_err());
        assert!(substitute_args(&["{workspace}".into()], &inputs, None).is_err());
        // values containing placeholders are not rescanned
        let tricky = vec!["{workspace}".to_string()];
        assert_eq!(substitute_args(&["{input:0}".into()], &tricky, None).unwrap()[0], "{workspace}");
    }

    #[test]
    fn tail_buffer() {
        let mut t = TailBuffer::new(4);
        t.push(b"ab");
        t.push(b"cd");
        t.push(b"ef");
        assert_eq!(t.into_string(), "cdef");
        let mut t = TailBuffer::new(4);
        t.push(b"0123456789");
        assert_eq!(t.into_string(), "6789");
    }

    #[test]
    fn capability_parsing() {
        let enc = " V....D libx264              libx264 H.264\n V....D h264_nvenc           NVIDIA NVENC H.264 encoder (codec h264)\n V..... hevc_qsv             H.265 (Intel Quick Sync Video acceleration)\n V....D h264_amf  AMD AMF\n ------\n V..... = Video\n";
        assert_eq!(parse_encoders(enc), vec!["h264_nvenc", "hevc_qsv", "h264_amf"]);
        let hw = "Hardware acceleration methods:\ncuda\nd3d11va\nqsv\n\n";
        assert_eq!(parse_hwaccels(hw), vec!["cuda", "d3d11va", "qsv"]);
    }

    #[cfg(unix)]
    #[tokio::test]
    async fn run_process_streams_and_reports() {
        let (tx, mut rx) = mpsc::channel(8);
        run_process(
            "sh".into(),
            vec!["-c".into(), "cat; echo err >&2; exit 3".into()],
            None,
            b"hello stdin".to_vec(),
            Duration::from_secs(10),
            tx,
        )
        .await;
        let mut out = Vec::new();
        let mut statuses = Vec::new();
        while let Some(m) = rx.recv().await {
            match m.unwrap().payload.unwrap() {
                run_output::Payload::Stdout(b) => out.extend(b),
                run_output::Payload::Status(s) => statuses.push(s),
            }
        }
        assert_eq!(out, b"hello stdin");
        assert_eq!(statuses.len(), 1);
        assert_eq!(statuses[0].exit_code, 3);
        assert_eq!(statuses[0].stderr, "err\n");
        assert!(!statuses[0].timed_out);
    }

    #[cfg(unix)]
    #[tokio::test]
    async fn run_process_timeout_and_missing_tool() {
        let (tx, mut rx) = mpsc::channel(8);
        run_process("sh".into(), vec!["-c".into(), "sleep 30".into()], None, vec![], Duration::from_millis(200), tx).await;
        let m = rx.recv().await.unwrap().unwrap();
        match m.payload.unwrap() {
            run_output::Payload::Status(s) => assert!(s.timed_out),
            _ => panic!("expected status"),
        }
        let (tx, mut rx) = mpsc::channel(8);
        run_process("/nonexistent/ffmpeg".into(), vec![], None, vec![], Duration::from_secs(1), tx).await;
        let e = rx.recv().await.unwrap().unwrap_err();
        assert_eq!(e.code(), tonic::Code::FailedPrecondition);
    }

    #[cfg(unix)]
    #[tokio::test]
    async fn run_process_killed_when_client_cancels() {
        let d = tempfile::tempdir().unwrap();
        let pidfile = d.path().join("pid");
        let script = format!("echo $$ > {}; exec sleep 30", pidfile.display());
        let (tx, rx) = mpsc::channel(8);
        let start = std::time::Instant::now();
        let task = tokio::spawn(run_process("sh".into(), vec!["-c".into(), script], None, vec![], Duration::from_secs(60), tx));
        tokio::time::sleep(Duration::from_millis(300)).await;
        let pid = std::fs::read_to_string(&pidfile).unwrap().trim().to_string();
        assert!(Path::new(&format!("/proc/{pid}")).exists());
        drop(rx); // what tonic does when the client cancels the call
        tokio::time::timeout(Duration::from_secs(5), task).await.unwrap().unwrap();
        assert!(start.elapsed() < Duration::from_secs(10));
        assert!(!Path::new(&format!("/proc/{pid}")).exists(), "process {pid} still alive");
    }

    #[test]
    fn sweeps_only_old_workspace_dirs() {
        let d = tempfile::tempdir().unwrap();
        std::fs::create_dir(d.path().join("fresh")).unwrap();
        std::fs::create_dir(d.path().join("not.a.workspace")).unwrap();
        assert_eq!(sweep_stale_workspaces(d.path(), Duration::from_secs(3600)), 0);
        std::thread::sleep(Duration::from_millis(20));
        assert_eq!(sweep_stale_workspaces(d.path(), Duration::from_millis(1)), 1);
        assert!(!d.path().join("fresh").exists());
        assert!(d.path().join("not.a.workspace").exists());
    }
}
