//! gRPC FileSystem and Hasher services.

use std::fs::{self, File};
use std::io::{self, BufReader, Read, Seek, SeekFrom};
use std::sync::Arc;

use futures::StreamExt;
use tokio::io::{AsyncReadExt, AsyncSeekExt};
use tokio::sync::mpsc;
use tokio_stream::wrappers::ReceiverStream;
use tokio_util::io::ReaderStream;
use tonic::{Request, Response, Status};

use crate::errors::{io_status, join_status};
use crate::fileinfo::file_info;
use crate::hash::{self, StreamHasher};
use crate::ops;
use crate::paths::{self, WirePath};
use crate::pb::file_system_server::FileSystem;
use crate::pb::hasher_server::Hasher;
use crate::pb::{
    BoolResponse, DataChunk, Empty, FileInfo, FingerprintRequest, FingerprintResponse, HashAlgorithm, MkdirRequest,
    PathRequest, PathResponse, ReadAtRequest, ReadAtResponse, ReadDirResponse, ReadFileRequest, RenameRequest,
    TrashRequest, WalkBatch, WalkRequest,
};
use crate::state::AppState;
use crate::walk::{self, Walker};

pub const MAX_READ_AT: i32 = 8 << 20;
pub const STREAM_CHUNK: usize = 1 << 20;

pub type BoxStream<T> = std::pin::Pin<Box<dyn futures::Stream<Item = Result<T, Status>> + Send + 'static>>;

/// Runs blocking filesystem work on the blocking pool.
pub async fn blocking<T, F>(f: F) -> Result<T, Status>
where
    F: FnOnce() -> Result<T, Status> + Send + 'static,
    T: Send + 'static,
{
    tokio::task::spawn_blocking(f).await.map_err(join_status)?
}

#[derive(Clone)]
pub struct FsService {
    st: Arc<AppState>,
}

impl FsService {
    pub fn new(st: Arc<AppState>) -> FsService {
        FsService { st }
    }

    fn check(&self, p: &str) -> Result<WirePath, Status> {
        Ok(self.st.roots.check(p)?)
    }

    fn deny_root(&self, p: &WirePath, op: &str) -> Result<(), Status> {
        if self.st.roots.is_root(p) {
            return Err(Status::permission_denied(format!("refusing to {op} a configured root: {p}")));
        }
        Ok(())
    }
}

fn stat_info(p: &WirePath, follow: bool) -> Result<FileInfo, Status> {
    let native = p.to_native();
    let md = if follow { fs::metadata(&native) } else { fs::symlink_metadata(&native) };
    let md = md.map_err(|e| io_status(if follow { "stat" } else { "lstat" }, &p.to_wire(), &e))?;
    Ok(file_info(p, &md))
}

#[tonic::async_trait]
impl FileSystem for FsService {
    async fn stat(&self, req: Request<PathRequest>) -> Result<Response<FileInfo>, Status> {
        let p = self.check(&req.into_inner().path)?;
        Ok(Response::new(blocking(move || stat_info(&p, true)).await?))
    }

    async fn lstat(&self, req: Request<PathRequest>) -> Result<Response<FileInfo>, Status> {
        let p = self.check(&req.into_inner().path)?;
        Ok(Response::new(blocking(move || stat_info(&p, false)).await?))
    }

    async fn read_dir(&self, req: Request<PathRequest>) -> Result<Response<ReadDirResponse>, Status> {
        let p = self.check(&req.into_inner().path)?;
        let entries = blocking(move || {
            let list = walk::read_dir_sorted(&p.to_native()).map_err(|e| io_status("readdir", &p.to_wire(), &e))?;
            let mut out = Vec::with_capacity(list.len());
            for (name, md) in list {
                let child = p.join(&name);
                match md {
                    Ok(md) => out.push(file_info(&child, &md)),
                    Err(e) => tracing::warn!(path = %child, error = %e, "readdir: cannot stat entry"),
                }
            }
            Ok(out)
        })
        .await?;
        Ok(Response::new(ReadDirResponse { entries }))
    }

    type WalkStream = BoxStream<WalkBatch>;

    async fn walk(&self, req: Request<WalkRequest>) -> Result<Response<Self::WalkStream>, Status> {
        let r = req.into_inner();
        let root = self.check(&r.root)?;
        let batch = if r.batch_size == 0 {
            self.st.cfg.walk_batch_size()
        } else {
            (r.batch_size as usize).clamp(1, 65536)
        };
        let (tx, rx) = mpsc::channel::<Result<WalkBatch, Status>>(4);
        let follow = r.follow_symlinks;
        tokio::task::spawn_blocking(move || {
            Walker::new(follow, batch, |b| tx.blocking_send(Ok(b)).is_ok()).run(&root);
        });
        Ok(Response::new(Box::pin(ReceiverStream::new(rx))))
    }

    async fn eval_symlinks(&self, req: Request<PathRequest>) -> Result<Response<PathResponse>, Status> {
        let p = self.check(&req.into_inner().path)?;
        let out = blocking(move || {
            walk::eval_symlinks(&p, paths::host_windows_style()).map_err(|e| io_status("evalsymlinks", &p.to_wire(), &e))
        })
        .await?;
        Ok(Response::new(PathResponse { path: out.to_wire() }))
    }

    async fn is_path_case_sensitive(&self, req: Request<PathRequest>) -> Result<Response<BoolResponse>, Status> {
        let p = self.check(&req.into_inner().path)?;
        let v = blocking(move || {
            ops::is_path_case_sensitive(&p).map_err(|e| io_status("case-sensitivity", &p.to_wire(), &e))
        })
        .await?;
        Ok(Response::new(BoolResponse { value: v }))
    }

    async fn read_at(&self, req: Request<ReadAtRequest>) -> Result<Response<ReadAtResponse>, Status> {
        let r = req.into_inner();
        let p = self.check(&r.path)?;
        if r.offset < 0 {
            return Err(Status::invalid_argument("offset must be >= 0"));
        }
        if r.length < 0 || r.length > MAX_READ_AT {
            return Err(Status::invalid_argument(format!("length must be between 0 and {MAX_READ_AT}")));
        }
        let resp = blocking(move || {
            let w = p.to_wire();
            let mut f = File::open(p.to_native()).map_err(|e| io_status("open", &w, &e))?;
            let size = f.metadata().map_err(|e| io_status("stat", &w, &e))?.len();
            f.seek(SeekFrom::Start(r.offset as u64)).map_err(|e| io_status("seek", &w, &e))?;
            let mut buf = vec![0u8; r.length as usize];
            let mut n = 0;
            while n < buf.len() {
                match f.read(&mut buf[n..]) {
                    Ok(0) => break,
                    Ok(k) => n += k,
                    Err(e) if e.kind() == io::ErrorKind::Interrupted => {}
                    Err(e) => return Err(io_status("read", &w, &e)),
                }
            }
            buf.truncate(n);
            let eof = n < r.length as usize || r.offset as u64 + n as u64 >= size;
            Ok(ReadAtResponse { data: buf, eof })
        })
        .await?;
        Ok(Response::new(resp))
    }

    type ReadFileStream = BoxStream<DataChunk>;

    async fn read_file(&self, req: Request<ReadFileRequest>) -> Result<Response<Self::ReadFileStream>, Status> {
        let r = req.into_inner();
        let p = self.check(&r.path)?;
        if r.offset < 0 || r.length < 0 {
            return Err(Status::invalid_argument("offset and length must be >= 0"));
        }
        let w = p.to_wire();
        let mut f = tokio::fs::File::open(p.to_native()).await.map_err(|e| io_status("open", &w, &e))?;
        let md = f.metadata().await.map_err(|e| io_status("stat", &w, &e))?;
        if md.is_dir() {
            return Err(Status::failed_precondition(format!("{w} is a directory")));
        }
        if r.max_size > 0 && md.len() > r.max_size as u64 {
            return Err(Status::failed_precondition(format!(
                "{w} is larger than max_size ({} > {})",
                md.len(),
                r.max_size
            )));
        }
        f.seek(SeekFrom::Start(r.offset as u64)).await.map_err(|e| io_status("seek", &w, &e))?;
        let limit = if r.length == 0 { u64::MAX } else { r.length as u64 };
        let stream = ReaderStream::with_capacity(f.take(limit), STREAM_CHUNK).map(move |res| match res {
            Ok(b) => Ok(DataChunk { data: b.to_vec() }),
            Err(e) => Err(io_status("read", &w, &e)),
        });
        Ok(Response::new(Box::pin(stream)))
    }

    async fn mkdir(&self, req: Request<MkdirRequest>) -> Result<Response<Empty>, Status> {
        let r = req.into_inner();
        let p = self.check(&r.path)?;
        blocking(move || {
            let native = p.to_native();
            let res = if r.all { fs::create_dir_all(&native) } else { fs::create_dir(&native) };
            res.map_err(|e| io_status("mkdir", &p.to_wire(), &e))?;
            if r.all && !native.is_dir() {
                return Err(Status::already_exists(format!("{p} exists and is not a directory")));
            }
            Ok(())
        })
        .await?;
        Ok(Response::new(Empty {}))
    }

    async fn rename(&self, req: Request<RenameRequest>) -> Result<Response<Empty>, Status> {
        let r = req.into_inner();
        let src = self.check(&r.old_path)?;
        let dst = self.check(&r.new_path)?;
        self.deny_root(&src, "rename")?;
        let ci = self.st.roots.case_insensitive();
        blocking(move || {
            let (sn, dn) = (src.to_native(), dst.to_native());
            fs::symlink_metadata(&sn).map_err(|e| io_status("rename", &src.to_wire(), &e))?;
            // A case-only rename on a case-insensitive volume targets the same
            // entry; anything else must not overwrite an existing destination.
            if !src.same_path(&dst, ci) && fs::symlink_metadata(&dn).is_ok() {
                return Err(Status::already_exists(format!("rename {src} -> {dst}: destination exists")));
            }
            ops::safe_move(&sn, &dn).map_err(|e| io_status("rename", &format!("{src} -> {dst}"), &e))
        })
        .await?;
        Ok(Response::new(Empty {}))
    }

    async fn remove(&self, req: Request<PathRequest>) -> Result<Response<Empty>, Status> {
        let p = self.check(&req.into_inner().path)?;
        self.deny_root(&p, "remove")?;
        blocking(move || ops::remove(&p.to_native()).map_err(|e| io_status("remove", &p.to_wire(), &e))).await?;
        Ok(Response::new(Empty {}))
    }

    async fn remove_all(&self, req: Request<PathRequest>) -> Result<Response<Empty>, Status> {
        let p = self.check(&req.into_inner().path)?;
        self.deny_root(&p, "remove")?;
        blocking(move || ops::remove_all(&p.to_native()).map_err(|e| io_status("removeall", &p.to_wire(), &e)))
            .await?;
        Ok(Response::new(Empty {}))
    }

    async fn move_to_trash(&self, req: Request<TrashRequest>) -> Result<Response<PathResponse>, Status> {
        let r = req.into_inner();
        let src = self.check(&r.path)?;
        let trash = self.check(&r.trash_dir)?;
        self.deny_root(&src, "trash")?;
        let dest = blocking(move || {
            fs::symlink_metadata(src.to_native()).map_err(|e| io_status("trash", &src.to_wire(), &e))?;
            fs::create_dir_all(trash.to_native()).map_err(|e| io_status("mkdir", &trash.to_wire(), &e))?;
            let ts = ops::trash_timestamp();
            let dest = ops::trash_destination(&src, &trash, |d| fs::metadata(d.to_native()).is_ok(), &ts);
            if fs::symlink_metadata(dest.to_native()).is_ok() {
                return Err(Status::already_exists(format!("trash destination {dest} exists")));
            }
            ops::safe_move(&src.to_native(), &dest.to_native())
                .map_err(|e| io_status("move to trash", &format!("{src} -> {dest}"), &e))?;
            Ok(dest)
        })
        .await?;
        Ok(Response::new(PathResponse { path: dest.to_wire() }))
    }
}

#[derive(Clone)]
pub struct HashService {
    st: Arc<AppState>,
}

impl HashService {
    pub fn new(st: Arc<AppState>) -> HashService {
        HashService { st }
    }
}

fn zip_status(path: &str, e: zip::result::ZipError) -> Status {
    use zip::result::ZipError;
    match e {
        ZipError::FileNotFound => Status::not_found(format!("{path}: zip entry not found")),
        ZipError::Io(e) => io_status("zip", path, &e),
        other => Status::failed_precondition(format!("{path}: {other}")),
    }
}

/// Opens `entry` (forward-slash relative path) inside an open archive.
pub fn find_zip_entry<R: Read + Seek>(archive: &mut zip::ZipArchive<R>, entry: &str) -> Option<usize> {
    let want = entry.trim_start_matches('/');
    archive.index_for_name(want).or_else(|| {
        let alt = want.replace('/', "\\");
        archive.index_for_name(&alt)
    })
}

pub fn fingerprint_blocking(p: &WirePath, zip_entry: &str, want_md5: bool, want_oshash: bool) -> Result<FingerprintResponse, Status> {
    let w = p.to_wire();
    let mut f = File::open(p.to_native()).map_err(|e| io_status("open", &w, &e))?;
    let md = f.metadata().map_err(|e| io_status("stat", &w, &e))?;
    if md.is_dir() {
        return Err(Status::failed_precondition(format!("{w} is a directory")));
    }
    let mut resp = FingerprintResponse::default();
    if zip_entry.is_empty() {
        let size = md.len();
        resp.size = size as i64;
        if want_oshash {
            resp.oshash = hash::oshash_reader(&mut f, size).map_err(|e| {
                if e.kind() == io::ErrorKind::InvalidInput {
                    Status::failed_precondition(format!("{w}: {e}"))
                } else {
                    io_status("oshash", &w, &e)
                }
            })?;
        }
        if want_md5 {
            f.seek(SeekFrom::Start(0)).map_err(|e| io_status("seek", &w, &e))?;
            resp.md5 = hash::md5_reader(&mut f).map_err(|e| io_status("md5", &w, &e))?.0;
        }
        return Ok(resp);
    }

    let label = format!("{w}#{zip_entry}");
    let mut archive = zip::ZipArchive::new(BufReader::new(f)).map_err(|e| zip_status(&w, e))?;
    let idx = find_zip_entry(&mut archive, zip_entry).ok_or_else(|| Status::not_found(format!("{label}: zip entry not found")))?;
    let mut entry = archive.by_index(idx).map_err(|e| zip_status(&label, e))?;
    if entry.is_dir() {
        return Err(Status::failed_precondition(format!("{label} is a directory")));
    }
    let mut h = StreamHasher::new(want_md5, want_oshash);
    let mut buf = vec![0u8; 1 << 20];
    loop {
        let n = match entry.read(&mut buf) {
            Ok(0) => break,
            Ok(n) => n,
            Err(e) if e.kind() == io::ErrorKind::Interrupted => continue,
            Err(e) => return Err(io_status("read zip entry", &label, &e)),
        };
        h.update(&buf[..n]);
    }
    resp.size = h.total() as i64;
    let (m, o) = h.finish().map_err(|e| Status::failed_precondition(format!("{label}: {e}")))?;
    resp.md5 = m.unwrap_or_default();
    resp.oshash = o.unwrap_or_default();
    Ok(resp)
}

#[tonic::async_trait]
impl Hasher for HashService {
    async fn fingerprint(&self, req: Request<FingerprintRequest>) -> Result<Response<FingerprintResponse>, Status> {
        let r = req.into_inner();
        let p = self.st.roots.check(&r.path)?;
        let mut want_md5 = r.algorithms.contains(&(HashAlgorithm::Md5 as i32));
        let mut want_oshash = r.algorithms.contains(&(HashAlgorithm::Oshash as i32));
        if !want_md5 && !want_oshash {
            want_md5 = true;
            want_oshash = true;
        }
        let _permit = self
            .st
            .hash_sem
            .clone()
            .acquire_owned()
            .await
            .map_err(|_| Status::unavailable("shutting down"))?;
        let entry = r.zip_entry;
        let resp = blocking(move || fingerprint_blocking(&p, &entry, want_md5, want_oshash)).await?;
        Ok(Response::new(resp))
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::io::Write;

    fn wp(p: &std::path::Path) -> WirePath {
        WirePath::parse(&paths::native_to_wire(p)).unwrap()
    }

    fn data(n: usize) -> Vec<u8> {
        (0..n).map(|i| (i * 31 % 251) as u8).collect()
    }

    #[test]
    fn fingerprint_file_and_zip_entries_agree() {
        let d = tempfile::tempdir().unwrap();
        let content = data(150_001);
        let file = d.path().join("v.mp4");
        fs::write(&file, &content).unwrap();
        let plain = fingerprint_blocking(&wp(&file), "", true, true).unwrap();
        assert_eq!(plain.size, 150_001);
        assert_eq!(plain.oshash, hash::oshash_reader(&mut io::Cursor::new(&content), 150_001).unwrap());
        assert_eq!(plain.md5, hash::md5_reader(&mut io::Cursor::new(&content)).unwrap().0);

        let zpath = d.path().join("g.zip");
        {
            let mut z = zip::ZipWriter::new(File::create(&zpath).unwrap());
            let deflate = zip::write::SimpleFileOptions::default().compression_method(zip::CompressionMethod::Deflated);
            let stored = zip::write::SimpleFileOptions::default().compression_method(zip::CompressionMethod::Stored);
            z.start_file("dir/a.mp4", deflate).unwrap();
            z.write_all(&content).unwrap();
            z.start_file("b.mp4", stored).unwrap();
            z.write_all(&content).unwrap();
            z.finish().unwrap();
        }
        let a = fingerprint_blocking(&wp(&zpath), "dir/a.mp4", true, true).unwrap();
        let b = fingerprint_blocking(&wp(&zpath), "b.mp4", true, true).unwrap();
        assert_eq!((a.md5.as_str(), a.oshash.as_str(), a.size), (plain.md5.as_str(), plain.oshash.as_str(), plain.size));
        assert_eq!((b.md5.as_str(), b.oshash.as_str()), (plain.md5.as_str(), plain.oshash.as_str()));
        let e = fingerprint_blocking(&wp(&zpath), "missing.jpg", true, false).unwrap_err();
        assert_eq!(e.code(), tonic::Code::NotFound);
    }

    fn svc(root: &std::path::Path) -> FsService {
        let mut cfg = crate::config::Config::default();
        cfg.server.shared_secret = "test-secret-0123456789".into();
        cfg.access.roots = vec![paths::native_to_wire(root)];
        let roots = crate::paths::Roots::new(&cfg.access.roots, false).unwrap();
        FsService::new(AppState::new(cfg, roots))
    }

    fn pr(p: String) -> Request<PathRequest> {
        Request::new(PathRequest { path: p })
    }

    #[tokio::test]
    async fn rpc_metadata_and_reads() {
        let d = tempfile::tempdir().unwrap();
        let root = d.path().join("lib");
        fs::create_dir(&root).unwrap();
        fs::write(root.join("b.mp4"), data(1000)).unwrap();
        fs::write(root.join("a.mp4"), b"x").unwrap();
        let s = svc(&root);
        let r = paths::native_to_wire(&root);

        let fi = s.stat(pr(format!("{r}/b.mp4"))).await.unwrap().into_inner();
        assert_eq!((fi.size, fi.name.as_str()), (1000, "b.mp4"));
        assert_eq!(s.stat(pr(format!("{r}/nope"))).await.unwrap_err().code(), tonic::Code::NotFound);
        assert_eq!(s.stat(pr(format!("{r}/../x"))).await.unwrap_err().code(), tonic::Code::PermissionDenied);
        assert_eq!(s.stat(pr("relative".into())).await.unwrap_err().code(), tonic::Code::PermissionDenied);

        let rd = s.read_dir(pr(r.clone())).await.unwrap().into_inner();
        let names: Vec<_> = rd.entries.iter().map(|e| e.name.as_str()).collect();
        assert_eq!(names, vec!["a.mp4", "b.mp4"]);

        let ra = |off, len| Request::new(ReadAtRequest { path: format!("{r}/b.mp4"), offset: off, length: len });
        let x = s.read_at(ra(990, 100)).await.unwrap().into_inner();
        assert_eq!((x.data.len(), x.eof), (10, true));
        let x = s.read_at(ra(0, 10)).await.unwrap().into_inner();
        assert_eq!((x.data, x.eof), (data(10), false));
        assert_eq!(s.read_at(ra(0, MAX_READ_AT + 1)).await.unwrap_err().code(), tonic::Code::InvalidArgument);
        assert_eq!(s.read_at(ra(-1, 1)).await.unwrap_err().code(), tonic::Code::InvalidArgument);

        let rf = |off, len, max| Request::new(ReadFileRequest { path: format!("{r}/b.mp4"), offset: off, length: len, max_size: max });
        let mut st = s.read_file(rf(100, 50, 0)).await.unwrap().into_inner();
        let mut got = Vec::new();
        while let Some(c) = st.next().await {
            got.extend(c.unwrap().data);
        }
        assert_eq!(got, data(1000)[100..150].to_vec());
        assert_eq!(s.read_file(rf(0, 0, 999)).await.err().unwrap().code(), tonic::Code::FailedPrecondition);

        let cs = s.is_path_case_sensitive(pr(format!("{r}/b.mp4"))).await.unwrap().into_inner();
        assert!(cs.value);
    }

    #[tokio::test]
    async fn rpc_mutations() {
        let d = tempfile::tempdir().unwrap();
        let root = d.path().join("lib");
        fs::create_dir(&root).unwrap();
        let s = svc(&root);
        let r = paths::native_to_wire(&root);
        let mk = |p: String, all| Request::new(MkdirRequest { path: p, all });
        let mv = |a: String, b: String| Request::new(RenameRequest { old_path: a, new_path: b });

        s.mkdir(mk(format!("{r}/x/y/z"), true)).await.unwrap();
        s.mkdir(mk(format!("{r}/x/y/z"), true)).await.unwrap();
        assert_eq!(s.mkdir(mk(format!("{r}/x"), false)).await.unwrap_err().code(), tonic::Code::AlreadyExists);
        assert_eq!(s.mkdir(mk(format!("{r}/q/w"), false)).await.unwrap_err().code(), tonic::Code::NotFound);

        fs::write(root.join("a.mp4"), b"a").unwrap();
        fs::write(root.join("c.mp4"), b"c").unwrap();
        s.rename(mv(format!("{r}/a.mp4"), format!("{r}/x/b.mp4"))).await.unwrap();
        assert_eq!(fs::read(root.join("x/b.mp4")).unwrap(), b"a");
        assert_eq!(
            s.rename(mv(format!("{r}/c.mp4"), format!("{r}/x/b.mp4"))).await.unwrap_err().code(),
            tonic::Code::AlreadyExists
        );
        assert_eq!(
            s.rename(mv(format!("{r}/c.mp4"), format!("{r}/../escape.mp4"))).await.unwrap_err().code(),
            tonic::Code::PermissionDenied
        );
        assert_eq!(
            s.rename(mv(r.clone(), format!("{r}/inside"))).await.unwrap_err().code(),
            tonic::Code::PermissionDenied
        );
        assert_eq!(
            s.rename(mv(format!("{r}/nope"), format!("{r}/nope2"))).await.unwrap_err().code(),
            tonic::Code::NotFound
        );

        assert_eq!(s.remove(pr(format!("{r}/x"))).await.unwrap_err().code(), tonic::Code::FailedPrecondition);
        assert_eq!(s.remove_all(pr(r.clone())).await.unwrap_err().code(), tonic::Code::PermissionDenied);
        s.remove(pr(format!("{r}/x/y/z"))).await.unwrap();

        let tr = |p: String| Request::new(TrashRequest { path: p, trash_dir: format!("{r}/.trash") });
        let t1 = s.move_to_trash(tr(format!("{r}/c.mp4"))).await.unwrap().into_inner().path;
        assert_eq!(t1, format!("{r}/.trash/c.mp4"));
        fs::write(root.join("c.mp4"), b"c2").unwrap();
        let t2 = s.move_to_trash(tr(format!("{r}/c.mp4"))).await.unwrap().into_inner().path;
        assert!(t2.starts_with(&format!("{r}/.trash/c_")) && t2.ends_with(".mp4"), "{t2}");
        assert_eq!(t2.len(), t1.len() + 16);
        assert_eq!(fs::read(paths::WirePath::parse(&t2).unwrap().to_native()).unwrap(), b"c2");

        s.remove_all(pr(format!("{r}/x"))).await.unwrap();
        s.remove_all(pr(format!("{r}/x"))).await.unwrap();
        assert!(!root.join("x").exists());
    }

    #[test]
    fn fingerprint_tiny_file() {
        let d = tempfile::tempdir().unwrap();
        let f = d.path().join("t");
        fs::write(&f, b"hello").unwrap();
        let r = fingerprint_blocking(&wp(&f), "", true, false).unwrap();
        assert_eq!(r.md5, "5d41402abc4b2a76b9719d911017c592");
        assert_eq!(
            fingerprint_blocking(&wp(&f), "", false, true).unwrap_err().code(),
            tonic::Code::FailedPrecondition
        );
    }
}
