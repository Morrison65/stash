//! HTTP server: `/healthz` and the signed `/raw` file endpoint.

use std::collections::HashMap;
use std::io::{self, Read};
use std::sync::Arc;
use std::time::{Duration, SystemTime, UNIX_EPOCH};

use axum::body::Body;
use axum::extract::{Query, State};
use axum::http::{header, HeaderMap, HeaderValue, Method, StatusCode};
use axum::response::{IntoResponse, Response};
use axum::routing::get;
use axum::Router;
use base64::alphabet;
use base64::engine::{DecodePaddingMode, GeneralPurpose, GeneralPurposeConfig};
use base64::Engine;
use hmac::{Hmac, Mac};
use sha2::Sha256;
use tokio::io::{AsyncReadExt, AsyncSeekExt};
use tokio_util::io::ReaderStream;

use crate::activity;
use crate::fileinfo::unix_nanos;
use crate::fs_service::find_zip_entry;
use crate::state::AppState;

const BODY_CHUNK: usize = 256 * 1024;

const B64: GeneralPurpose = GeneralPurpose::new(
    &alphabet::URL_SAFE,
    GeneralPurposeConfig::new().with_decode_padding_mode(DecodePaddingMode::Indifferent),
);

pub fn router(st: Arc<AppState>) -> Router {
    Router::new()
        .route("/healthz", get(|| async { "ok" }))
        .route("/raw", get(raw))
        .with_state(st)
}

/// Lower-case hex HMAC-SHA256 over `"raw\n" + p + "\n" + z + "\n" + e`.
pub fn sign(secret: &str, p: &str, z: &str, e: &str) -> String {
    let mut mac = Hmac::<Sha256>::new_from_slice(secret.as_bytes()).expect("hmac accepts any key length");
    mac.update(format!("raw\n{p}\n{z}\n{e}").as_bytes());
    hex::encode(mac.finalize().into_bytes())
}

/// Builds the `/raw` query string for `path` (and optional zip entry).
pub fn signed_query(secret: &str, path: &str, zip_entry: &str, expiry: u64) -> String {
    let enc = base64::engine::general_purpose::URL_SAFE_NO_PAD;
    let p = enc.encode(path);
    let z = if zip_entry.is_empty() { String::new() } else { enc.encode(zip_entry) };
    let e = expiry.to_string();
    let s = sign(secret, &p, &z, &e);
    format!("p={p}&z={z}&e={e}&s={s}")
}

/// Constant-time signature verification.
pub fn verify(secret: &str, p: &str, z: &str, e: &str, sig_hex: &str) -> bool {
    let Ok(sig) = hex::decode(sig_hex) else { return false };
    let mut mac = Hmac::<Sha256>::new_from_slice(secret.as_bytes()).expect("hmac accepts any key length");
    mac.update(format!("raw\n{p}\n{z}\n{e}").as_bytes());
    mac.verify_slice(&sig).is_ok()
}

#[derive(Debug, PartialEq, Eq)]
pub enum RangeSpec {
    /// No (usable) range: serve the whole body.
    Full,
    /// Inclusive byte range.
    Single(u64, u64),
    Unsatisfiable,
}

/// Parses a `Range` header for a body of `size` bytes. Multiple ranges and
/// malformed headers are ignored (full response), as RFC 9110 allows.
pub fn parse_range(h: &str, size: u64) -> RangeSpec {
    let Some(spec) = h.trim().strip_prefix("bytes=") else { return RangeSpec::Full };
    if spec.contains(',') {
        return RangeSpec::Full;
    }
    let Some((a, b)) = spec.trim().split_once('-') else { return RangeSpec::Full };
    let (a, b) = (a.trim(), b.trim());
    if a.is_empty() {
        let Ok(n) = b.parse::<u64>() else { return RangeSpec::Full };
        if n == 0 || size == 0 {
            return RangeSpec::Unsatisfiable;
        }
        return RangeSpec::Single(size.saturating_sub(n), size - 1);
    }
    let Ok(start) = a.parse::<u64>() else { return RangeSpec::Full };
    let end = if b.is_empty() {
        u64::MAX
    } else {
        match b.parse::<u64>() {
            Ok(e) if e >= start => e,
            _ => return RangeSpec::Full,
        }
    };
    if start >= size {
        return RangeSpec::Unsatisfiable;
    }
    RangeSpec::Single(start, end.min(size - 1))
}

pub fn content_type(name: &str) -> &'static str {
    let ext = name.rsplit_once('.').map(|(_, e)| e.to_ascii_lowercase()).unwrap_or_default();
    match ext.as_str() {
        "mp4" | "m4v" => "video/mp4",
        "mkv" => "video/x-matroska",
        "webm" => "video/webm",
        "avi" => "video/x-msvideo",
        "wmv" => "video/x-ms-wmv",
        "mov" => "video/quicktime",
        "flv" => "video/x-flv",
        "mpg" | "mpeg" => "video/mpeg",
        "ts" | "m2ts" => "video/mp2t",
        "jpg" | "jpeg" => "image/jpeg",
        "png" => "image/png",
        "gif" => "image/gif",
        "webp" => "image/webp",
        "avif" => "image/avif",
        "svg" => "image/svg+xml",
        "vtt" => "text/vtt; charset=utf-8",
        "srt" => "application/x-subrip",
        "funscript" | "json" => "application/json",
        "zip" => "application/zip",
        _ => "application/octet-stream",
    }
}

pub fn etag_matches(if_none_match: &str, etag: &str) -> bool {
    let bare = etag.trim_start_matches("W/");
    if_none_match.split(',').map(str::trim).any(|t| t == "*" || t.trim_start_matches("W/") == bare)
}

fn plain(code: StatusCode, msg: &str) -> Response {
    (code, [(header::CONTENT_TYPE, "text/plain; charset=utf-8")], format!("{msg}\n")).into_response()
}

struct Validators {
    etag: String,
    last_modified: String,
    mtime_secs: u64,
}

impl Validators {
    fn new(size: u64, mtime: SystemTime) -> Validators {
        let mtime_secs = mtime.duration_since(UNIX_EPOCH).map(|d| d.as_secs()).unwrap_or(0);
        Validators {
            etag: format!("\"{size}-{}\"", unix_nanos(mtime)),
            last_modified: httpdate::fmt_http_date(UNIX_EPOCH + Duration::from_secs(mtime_secs)),
            mtime_secs,
        }
    }

    fn not_modified(&self, h: &HeaderMap) -> bool {
        if let Some(inm) = h.get(header::IF_NONE_MATCH).and_then(|v| v.to_str().ok()) {
            return etag_matches(inm, &self.etag);
        }
        if let Some(ims) = h.get(header::IF_MODIFIED_SINCE).and_then(|v| v.to_str().ok()) {
            if let Ok(t) = httpdate::parse_http_date(ims) {
                let secs = t.duration_since(UNIX_EPOCH).map(|d| d.as_secs()).unwrap_or(0);
                return self.mtime_secs <= secs;
            }
        }
        false
    }

    /// `If-Range`: the range applies only if the validator still matches.
    fn range_allowed(&self, h: &HeaderMap) -> bool {
        match h.get(header::IF_RANGE).and_then(|v| v.to_str().ok()) {
            None => true,
            Some(v) if v.starts_with('"') => v == self.etag,
            Some(v) if v.starts_with("W/") => false,
            Some(v) => httpdate::parse_http_date(v)
                .ok()
                .and_then(|t| t.duration_since(UNIX_EPOCH).ok())
                .is_some_and(|d| self.mtime_secs <= d.as_secs()),
        }
    }

    fn apply(&self, b: axum::http::response::Builder) -> axum::http::response::Builder {
        b.header(header::ETAG, &self.etag).header(header::LAST_MODIFIED, &self.last_modified)
    }
}

enum Source {
    /// A byte range of a plain file: (path, base offset).
    File(std::path::PathBuf, u64),
    /// A compressed zip entry: (archive path, entry index); no range support.
    ZipCompressed(std::path::PathBuf, usize),
}

struct Resolved {
    source: Source,
    size: u64,
    mtime: SystemTime,
    name: String,
}

fn resolve(native: std::path::PathBuf, zip_entry: Option<String>) -> io::Result<Resolved> {
    let md = std::fs::metadata(&native)?;
    if md.is_dir() {
        return Err(io::Error::new(io::ErrorKind::NotFound, "is a directory"));
    }
    let mtime = md.modified().unwrap_or(UNIX_EPOCH);
    let Some(entry) = zip_entry else {
        let name = native.file_name().map(|n| n.to_string_lossy().into_owned()).unwrap_or_default();
        return Ok(Resolved {
            source: Source::File(native, 0),
            size: md.len(),
            mtime,
            name,
        });
    };
    let f = std::fs::File::open(&native)?;
    let mut archive = zip::ZipArchive::new(io::BufReader::new(f)).map_err(zip_io)?;
    let idx = find_zip_entry(&mut archive, &entry).ok_or_else(|| io::Error::new(io::ErrorKind::NotFound, "zip entry not found"))?;
    let ze = archive.by_index_raw(idx).map_err(zip_io)?;
    if ze.is_dir() {
        return Err(io::Error::new(io::ErrorKind::NotFound, "zip entry is a directory"));
    }
    if ze.encrypted() {
        return Err(io::Error::new(io::ErrorKind::PermissionDenied, "encrypted zip entry"));
    }
    let size = ze.size();
    let name = entry.rsplit('/').next().unwrap_or(&entry).to_string();
    let source = if ze.compression() == zip::CompressionMethod::Stored {
        Source::File(native.clone(), ze.data_start())
    } else {
        Source::ZipCompressed(native.clone(), idx)
    };
    Ok(Resolved {
        source,
        size,
        mtime,
        name,
    })
}

fn zip_io(e: zip::result::ZipError) -> io::Error {
    match e {
        zip::result::ZipError::Io(e) => e,
        zip::result::ZipError::FileNotFound => io::Error::new(io::ErrorKind::NotFound, "zip entry not found"),
        other => io::Error::new(io::ErrorKind::InvalidData, other.to_string()),
    }
}

async fn raw(
    State(st): State<Arc<AppState>>,
    method: Method,
    headers: HeaderMap,
    Query(q): Query<HashMap<String, String>>,
) -> Response {
    let get = |k: &str| q.get(k).map(String::as_str).unwrap_or("");
    let (p, z, e, s) = (get("p"), get("z"), get("e"), get("s"));
    if p.is_empty() || e.is_empty() || s.is_empty() {
        return plain(StatusCode::BAD_REQUEST, "missing p, e or s");
    }
    if !verify(st.secret(), p, z, e, s) {
        return plain(StatusCode::FORBIDDEN, "bad signature");
    }
    let Ok(expiry) = e.parse::<u64>() else { return plain(StatusCode::BAD_REQUEST, "bad expiry") };
    let now = SystemTime::now().duration_since(UNIX_EPOCH).map(|d| d.as_secs()).unwrap_or(0);
    if now > expiry {
        return plain(StatusCode::FORBIDDEN, "link expired");
    }
    let decode = |v: &str| B64.decode(v).ok().and_then(|b| String::from_utf8(b).ok());
    let Some(path) = decode(p) else { return plain(StatusCode::BAD_REQUEST, "bad path encoding") };
    let zip_entry = if z.is_empty() {
        None
    } else {
        match decode(z) {
            Some(v) if !v.is_empty() => Some(v),
            Some(_) => None,
            None => return plain(StatusCode::BAD_REQUEST, "bad zip entry encoding"),
        }
    };
    let wp = match st.roots.check(&path) {
        Ok(wp) => wp,
        Err(err) => {
            tracing::warn!(error = %err, "raw: rejected path");
            return plain(StatusCode::FORBIDDEN, "path not allowed");
        }
    };

    activity::STREAM.add(1);
    // a new playback (no range, or from the start); seeks are only counted
    let from_start = headers
        .get(axum::http::header::RANGE)
        .and_then(|v| v.to_str().ok())
        .map_or(true, |r| r.trim_start_matches("bytes=").starts_with("0-"));
    if from_start && method == Method::GET {
        tracing::info!("stream {wp}");
    }

    let native = wp.to_native();
    let resolved = match tokio::task::spawn_blocking(move || resolve(native, zip_entry)).await {
        Ok(Ok(r)) => r,
        Ok(Err(err)) => {
            return match err.kind() {
                io::ErrorKind::NotFound => plain(StatusCode::NOT_FOUND, "not found"),
                io::ErrorKind::PermissionDenied => plain(StatusCode::FORBIDDEN, "access denied"),
                _ => {
                    tracing::warn!(path = %wp, error = %err, "raw: cannot open");
                    plain(StatusCode::INTERNAL_SERVER_ERROR, "cannot open file")
                }
            }
        }
        Err(_) => return plain(StatusCode::INTERNAL_SERVER_ERROR, "internal error"),
    };

    serve(method, &headers, resolved).await
}

async fn serve(method: Method, headers: &HeaderMap, r: Resolved) -> Response {
    let v = Validators::new(r.size, r.mtime);
    let ctype = content_type(&r.name);
    let ranges_ok = matches!(r.source, Source::File(..));

    if v.not_modified(headers) {
        return v
            .apply(Response::builder().status(StatusCode::NOT_MODIFIED))
            .body(Body::empty())
            .unwrap();
    }

    let range = if ranges_ok && v.range_allowed(headers) {
        headers
            .get(header::RANGE)
            .and_then(|h| h.to_str().ok())
            .map(|h| parse_range(h, r.size))
            .unwrap_or(RangeSpec::Full)
    } else {
        RangeSpec::Full
    };

    let mut b = v
        .apply(Response::builder())
        .header(header::CONTENT_TYPE, ctype)
        .header(header::ACCEPT_RANGES, if ranges_ok { "bytes" } else { "none" });

    let (start, len) = match range {
        RangeSpec::Unsatisfiable => {
            return b
                .status(StatusCode::RANGE_NOT_SATISFIABLE)
                .header(header::CONTENT_RANGE, format!("bytes */{}", r.size))
                .body(Body::empty())
                .unwrap();
        }
        RangeSpec::Single(s, e) => {
            b = b
                .status(StatusCode::PARTIAL_CONTENT)
                .header(header::CONTENT_RANGE, format!("bytes {s}-{e}/{}", r.size));
            (s, e - s + 1)
        }
        RangeSpec::Full => {
            b = b.status(StatusCode::OK);
            (0, r.size)
        }
    };
    b = b.header(header::CONTENT_LENGTH, HeaderValue::from(len));

    if method == Method::HEAD {
        return b.body(Body::empty()).unwrap();
    }

    let body = match r.source {
        Source::File(path, base) => match file_body(&path, base + start, len).await {
            Ok(body) => body,
            Err(err) => {
                tracing::warn!(path = %path.display(), error = %err, "raw: read failed");
                return plain(StatusCode::INTERNAL_SERVER_ERROR, "cannot read file");
            }
        },
        Source::ZipCompressed(path, idx) => zip_body(path, idx),
    };
    b.body(body).unwrap()
}

async fn file_body(path: &std::path::Path, offset: u64, len: u64) -> io::Result<Body> {
    let mut f = tokio::fs::File::open(path).await?;
    f.seek(io::SeekFrom::Start(offset)).await?;
    Ok(Body::from_stream(ReaderStream::with_capacity(f.take(len), BODY_CHUNK)))
}

/// Streams a decompressed zip entry from a blocking thread.
fn zip_body(path: std::path::PathBuf, idx: usize) -> Body {
    let (tx, rx) = tokio::sync::mpsc::channel::<io::Result<bytes_alias::Bytes>>(4);
    tokio::task::spawn_blocking(move || {
        let res = (|| -> io::Result<()> {
            let f = std::fs::File::open(&path)?;
            let mut archive = zip::ZipArchive::new(io::BufReader::new(f)).map_err(zip_io)?;
            let mut entry = archive.by_index(idx).map_err(zip_io)?;
            let mut buf = vec![0u8; BODY_CHUNK];
            loop {
                let n = entry.read(&mut buf)?;
                if n == 0 {
                    return Ok(());
                }
                if tx.blocking_send(Ok(bytes_alias::Bytes::copy_from_slice(&buf[..n]))).is_err() {
                    return Ok(());
                }
            }
        })();
        if let Err(e) = res {
            let _ = tx.blocking_send(Err(e));
        }
    });
    Body::from_stream(tokio_stream::wrappers::ReceiverStream::new(rx))
}

/// `bytes` is re-exported by axum's dependencies; keep the alias local.
mod bytes_alias {
    pub use axum::body::Bytes;
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::config::Config;
    use crate::paths::{native_to_wire, Roots};
    use axum::http::Request;
    use http_body_util::BodyExt;
    use std::io::Write;
    use tower::ServiceExt;

    #[test]
    fn signature_roundtrip() {
        let s = sign("secret", "VTovYS5tcDQ", "", "1700000000");
        assert_eq!(s.len(), 64);
        assert!(s.chars().all(|c| c.is_ascii_hexdigit() && !c.is_ascii_uppercase()));
        assert!(verify("secret", "VTovYS5tcDQ", "", "1700000000", &s));
        assert!(!verify("secret2", "VTovYS5tcDQ", "", "1700000000", &s));
        assert!(!verify("secret", "VTovYS5tcDR", "", "1700000000", &s));
        assert!(!verify("secret", "VTovYS5tcDQ", "eg", "1700000000", &s));
        assert!(!verify("secret", "VTovYS5tcDQ", "", "1700000001", &s));
        assert!(!verify("secret", "VTovYS5tcDQ", "", "1700000000", "zz"));
        assert!(!verify("secret", "VTovYS5tcDQ", "", "1700000000", ""));
    }

    #[test]
    fn signature_known_vector() {
        // HMAC-SHA256("key", "raw\np\nz\n1") computed independently (python hmac).
        assert_eq!(
            sign("key", "p", "z", "1"),
            hex::encode({
                let mut m = Hmac::<Sha256>::new_from_slice(b"key").unwrap();
                m.update(b"raw\np\nz\n1");
                m.finalize().into_bytes()
            })
        );
    }

    #[test]
    fn ranges() {
        use RangeSpec::*;
        assert_eq!(parse_range("bytes=0-99", 1000), Single(0, 99));
        assert_eq!(parse_range("bytes=100-", 1000), Single(100, 999));
        assert_eq!(parse_range("bytes=-100", 1000), Single(900, 999));
        assert_eq!(parse_range("bytes=-5000", 1000), Single(0, 999));
        assert_eq!(parse_range("bytes=900-5000", 1000), Single(900, 999));
        assert_eq!(parse_range("bytes=999-999", 1000), Single(999, 999));
        assert_eq!(parse_range("bytes=1000-", 1000), Unsatisfiable);
        assert_eq!(parse_range("bytes=-0", 1000), Unsatisfiable);
        assert_eq!(parse_range("bytes=0-", 0), Unsatisfiable);
        assert_eq!(parse_range("bytes=5-1", 1000), Full);
        assert_eq!(parse_range("bytes=0-1,5-6", 1000), Full);
        assert_eq!(parse_range("items=0-1", 1000), Full);
        assert_eq!(parse_range("bytes=abc", 1000), Full);
        assert_eq!(parse_range("bytes=x-1", 1000), Full);
    }

    #[test]
    fn content_types() {
        assert_eq!(content_type("a.MP4"), "video/mp4");
        assert_eq!(content_type("a.m4v"), "video/mp4");
        assert_eq!(content_type("a.mkv"), "video/x-matroska");
        assert_eq!(content_type("a.webm"), "video/webm");
        assert_eq!(content_type("a.wmv"), "video/x-ms-wmv");
        assert_eq!(content_type("a.m2ts"), "video/mp2t");
        assert_eq!(content_type("a.jpeg"), "image/jpeg");
        assert_eq!(content_type("a.avif"), "image/avif");
        assert_eq!(content_type("a.svg"), "image/svg+xml");
        assert_eq!(content_type("a.vtt"), "text/vtt; charset=utf-8");
        assert_eq!(content_type("a.funscript"), "application/json");
        assert_eq!(content_type("a.zip"), "application/zip");
        assert_eq!(content_type("noext"), "application/octet-stream");
    }

    #[test]
    fn etags() {
        assert!(etag_matches("\"1-2\"", "\"1-2\""));
        assert!(etag_matches("W/\"1-2\"", "\"1-2\""));
        assert!(etag_matches("\"x\", \"1-2\"", "\"1-2\""));
        assert!(etag_matches("*", "\"1-2\""));
        assert!(!etag_matches("\"1-3\"", "\"1-2\""));
    }

    struct Fixture {
        _dir: tempfile::TempDir,
        root: String,
        app: Router,
        secret: String,
    }

    fn fixture() -> Fixture {
        let dir = tempfile::tempdir().unwrap();
        let media = dir.path().join("media");
        std::fs::create_dir(&media).unwrap();
        std::fs::write(media.join("clip.mp4"), (0..1000u32).map(|i| (i % 256) as u8).collect::<Vec<_>>()).unwrap();
        std::fs::write(dir.path().join("secret.txt"), b"nope").unwrap();
        {
            let mut z = zip::ZipWriter::new(std::fs::File::create(media.join("g.zip")).unwrap());
            let stored = zip::write::SimpleFileOptions::default().compression_method(zip::CompressionMethod::Stored);
            let deflate = zip::write::SimpleFileOptions::default().compression_method(zip::CompressionMethod::Deflated);
            z.start_file("img/a.jpg", stored).unwrap();
            z.write_all(b"0123456789").unwrap();
            z.start_file("b.png", deflate).unwrap();
            z.write_all(&[7u8; 5000]).unwrap();
            z.finish().unwrap();
        }
        let root = native_to_wire(&media);
        let secret = "test-secret-0123456789".to_string();
        let mut cfg = Config::default();
        cfg.server.shared_secret = secret.clone();
        cfg.access.roots = vec![root.clone()];
        let roots = Roots::new(&cfg.access.roots, false).unwrap();
        let st = AppState::new(cfg, roots);
        Fixture {
            _dir: dir,
            root,
            app: router(st),
            secret,
        }
    }

    fn url(f: &Fixture, path: &str, zip: &str, expiry: u64) -> String {
        format!("/raw?{}", signed_query(&f.secret, path, zip, expiry))
    }

    fn future() -> u64 {
        SystemTime::now().duration_since(UNIX_EPOCH).unwrap().as_secs() + 600
    }

    async fn send(f: &Fixture, method: &str, uri: &str, hdrs: &[(&str, &str)]) -> (StatusCode, HeaderMap, Vec<u8>) {
        let mut rb = Request::builder().method(method).uri(uri);
        for (k, v) in hdrs {
            rb = rb.header(*k, *v);
        }
        let resp = f.app.clone().oneshot(rb.body(Body::empty()).unwrap()).await.unwrap();
        let status = resp.status();
        let headers = resp.headers().clone();
        let body = resp.into_body().collect().await.unwrap().to_bytes().to_vec();
        (status, headers, body)
    }

    #[tokio::test]
    async fn healthz() {
        let f = fixture();
        let (s, _, b) = send(&f, "GET", "/healthz", &[]).await;
        assert_eq!(s, StatusCode::OK);
        assert_eq!(b, b"ok");
    }

    #[tokio::test]
    async fn raw_full_range_head_conditional() {
        let f = fixture();
        let u = url(&f, &format!("{}/clip.mp4", f.root), "", future());
        let (s, h, b) = send(&f, "GET", &u, &[]).await;
        assert_eq!(s, StatusCode::OK);
        assert_eq!(b.len(), 1000);
        assert_eq!(h[header::CONTENT_TYPE], "video/mp4");
        assert_eq!(h[header::ACCEPT_RANGES], "bytes");
        assert_eq!(h[header::CONTENT_LENGTH], "1000");
        let etag = h[header::ETAG].to_str().unwrap().to_string();
        assert!(etag.starts_with("\"1000-"));

        let (s, h, b) = send(&f, "GET", &u, &[("range", "bytes=10-19")]).await;
        assert_eq!(s, StatusCode::PARTIAL_CONTENT);
        assert_eq!(h[header::CONTENT_RANGE], "bytes 10-19/1000");
        assert_eq!(b, (10..20u8).collect::<Vec<_>>());

        let (s, h, _) = send(&f, "GET", &u, &[("range", "bytes=5000-")]).await;
        assert_eq!(s, StatusCode::RANGE_NOT_SATISFIABLE);
        assert_eq!(h[header::CONTENT_RANGE], "bytes */1000");

        let (s, h, b) = send(&f, "HEAD", &u, &[]).await;
        assert_eq!(s, StatusCode::OK);
        assert_eq!(h[header::CONTENT_LENGTH], "1000");
        assert!(b.is_empty());

        let (s, _, b) = send(&f, "GET", &u, &[("if-none-match", &etag)]).await;
        assert_eq!(s, StatusCode::NOT_MODIFIED);
        assert!(b.is_empty());
        let lm = h[header::LAST_MODIFIED].to_str().unwrap().to_string();
        let (s, _, _) = send(&f, "GET", &u, &[("if-modified-since", &lm)]).await;
        assert_eq!(s, StatusCode::NOT_MODIFIED);
        let (s, _, b) = send(&f, "GET", &u, &[("range", "bytes=0-1"), ("if-range", "\"other\"")]).await;
        assert_eq!(s, StatusCode::OK);
        assert_eq!(b.len(), 1000);
    }

    #[tokio::test]
    async fn raw_rejections() {
        let f = fixture();
        let good = url(&f, &format!("{}/clip.mp4", f.root), "", future());
        let last = if good.ends_with('0') { "1" } else { "0" };
        let bad_sig = format!("{}{last}", &good[..good.len() - 1]);
        assert_eq!(send(&f, "GET", &bad_sig, &[]).await.0, StatusCode::FORBIDDEN);
        let expired = url(&f, &format!("{}/clip.mp4", f.root), "", 1000);
        assert_eq!(send(&f, "GET", &expired, &[]).await.0, StatusCode::FORBIDDEN);
        let outside = url(&f, &format!("{}/../secret.txt", f.root), "", future());
        assert_eq!(send(&f, "GET", &outside, &[]).await.0, StatusCode::FORBIDDEN);
        let missing = url(&f, &format!("{}/nope.mp4", f.root), "", future());
        assert_eq!(send(&f, "GET", &missing, &[]).await.0, StatusCode::NOT_FOUND);
        let dir = url(&f, &f.root, "", future());
        assert_eq!(send(&f, "GET", &dir, &[]).await.0, StatusCode::NOT_FOUND);
        assert_eq!(send(&f, "GET", "/raw?p=x", &[]).await.0, StatusCode::BAD_REQUEST);
    }

    #[tokio::test]
    async fn raw_zip_entries() {
        let f = fixture();
        let zp = format!("{}/g.zip", f.root);
        let stored = url(&f, &zp, "img/a.jpg", future());
        let (s, h, b) = send(&f, "GET", &stored, &[]).await;
        assert_eq!(s, StatusCode::OK);
        assert_eq!(b, b"0123456789");
        assert_eq!(h[header::CONTENT_TYPE], "image/jpeg");
        let (s, _, b) = send(&f, "GET", &stored, &[("range", "bytes=2-4")]).await;
        assert_eq!(s, StatusCode::PARTIAL_CONTENT);
        assert_eq!(b, b"234");

        let deflated = url(&f, &zp, "b.png", future());
        let (s, h, b) = send(&f, "GET", &deflated, &[("range", "bytes=2-4")]).await;
        assert_eq!(s, StatusCode::OK); // no range support for compressed entries
        assert_eq!(h[header::ACCEPT_RANGES], "none");
        assert_eq!(h[header::CONTENT_LENGTH], "5000");
        assert_eq!(b, vec![7u8; 5000]);

        let missing = url(&f, &zp, "nope.jpg", future());
        assert_eq!(send(&f, "GET", &missing, &[]).await.0, StatusCode::NOT_FOUND);
    }
}
