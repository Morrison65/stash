//! md5 and oshash, bit-identical to `pkg/hash/md5` and `pkg/hash/oshash`.

use std::io::{self, Read, Seek, SeekFrom};

use md5::{Digest, Md5};

pub const OSHASH_CHUNK: u64 = 64 * 1024;
const MD5_BUF: usize = 1 << 20;

/// Sum of little-endian u64 words (wrapping), as Go's `sumBytes`.
fn sum_words(buf: &[u8]) -> u64 {
    debug_assert!(buf.len() % 8 == 0);
    buf.chunks_exact(8)
        .fold(0u64, |acc, w| acc.wrapping_add(u64::from_le_bytes(w.try_into().unwrap())))
}

/// Chunk length used for a file of `size` bytes; error when `size <= 8`.
pub fn oshash_chunk_len(size: u64) -> io::Result<u64> {
    if size <= 8 {
        return Err(io::Error::new(
            io::ErrorKind::InvalidInput,
            format!("cannot calculate oshash where size < 8 ({size})"),
        ));
    }
    Ok(if size < OSHASH_CHUNK { (size / 8) * 8 } else { OSHASH_CHUNK })
}

/// Final hash from head and tail chunks (each `oshash_chunk_len` bytes).
pub fn oshash_from_parts(size: u64, head: &[u8], tail: &[u8]) -> String {
    let r = sum_words(head).wrapping_add(sum_words(tail)).wrapping_add(size);
    format!("{r:016x}")
}

/// oshash of a seekable source of known size (Go `oshash.FromReader`).
pub fn oshash_reader<R: Read + Seek>(r: &mut R, size: u64) -> io::Result<String> {
    let chunk = oshash_chunk_len(size)?;
    let mut head = vec![0u8; chunk as usize];
    let mut tail = vec![0u8; chunk as usize];
    r.seek(SeekFrom::Start(0))?;
    r.read_exact(&mut head)?;
    r.seek(SeekFrom::End(-(chunk as i64)))?;
    r.read_exact(&mut tail)?;
    Ok(oshash_from_parts(size, &head, &tail))
}

/// md5 of a stream, lower-case hex.
pub fn md5_reader<R: Read>(r: &mut R) -> io::Result<(String, u64)> {
    let mut h = Md5::new();
    let mut buf = vec![0u8; MD5_BUF];
    let mut total = 0u64;
    loop {
        let n = match r.read(&mut buf) {
            Ok(0) => break,
            Ok(n) => n,
            Err(e) if e.kind() == io::ErrorKind::Interrupted => continue,
            Err(e) => return Err(e),
        };
        total += n as u64;
        h.update(&buf[..n]);
    }
    Ok((hex::encode(h.finalize()), total))
}

/// Single pass over a non-seekable stream (zip entries) computing md5 and/or
/// oshash. `size` is the declared uncompressed size.
pub struct StreamHasher {
    md5: Option<Md5>,
    want_oshash: bool,
    head: Vec<u8>,
    head_cap: usize,
    /// Ring buffer of the last `head_cap` bytes.
    tail: Vec<u8>,
    tail_pos: usize,
    total: u64,
}

impl StreamHasher {
    pub fn new(want_md5: bool, want_oshash: bool) -> StreamHasher {
        let cap = OSHASH_CHUNK as usize;
        StreamHasher {
            md5: want_md5.then(Md5::new),
            want_oshash,
            head: Vec::with_capacity(if want_oshash { cap } else { 0 }),
            head_cap: cap,
            tail: if want_oshash { vec![0u8; cap] } else { Vec::new() },
            tail_pos: 0,
            total: 0,
        }
    }

    pub fn update(&mut self, data: &[u8]) {
        if let Some(h) = self.md5.as_mut() {
            h.update(data);
        }
        if self.want_oshash {
            if self.head.len() < self.head_cap {
                let n = (self.head_cap - self.head.len()).min(data.len());
                self.head.extend_from_slice(&data[..n]);
            }
            // keep the last head_cap bytes
            let cap = self.head_cap;
            let src = if data.len() > cap { &data[data.len() - cap..] } else { data };
            let first = (cap - self.tail_pos).min(src.len());
            self.tail[self.tail_pos..self.tail_pos + first].copy_from_slice(&src[..first]);
            let rest = src.len() - first;
            if rest > 0 {
                self.tail[..rest].copy_from_slice(&src[first..]);
            }
            self.tail_pos = (self.tail_pos + src.len()) % cap;
        }
        self.total += data.len() as u64;
    }

    pub fn total(&self) -> u64 {
        self.total
    }

    /// Returns (md5, oshash). oshash uses the number of bytes actually read.
    pub fn finish(self) -> io::Result<(Option<String>, Option<String>)> {
        let md5 = self.md5.map(|h| hex::encode(h.finalize()));
        let oshash = if self.want_oshash {
            let size = self.total;
            let chunk = oshash_chunk_len(size)? as usize;
            let head = &self.head[..chunk];
            // last `chunk` bytes from the ring buffer
            let cap = self.head_cap;
            let mut last = Vec::with_capacity(chunk);
            let available = (size as usize).min(cap);
            let start = (self.tail_pos + cap - available) % cap;
            for i in 0..available {
                last.push(self.tail[(start + i) % cap]);
            }
            let tail = &last[last.len() - chunk..];
            Some(oshash_from_parts(size, head, tail))
        } else {
            None
        };
        Ok((md5, oshash))
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::io::Cursor;

    fn os(data: &[u8]) -> io::Result<String> {
        oshash_reader(&mut Cursor::new(data), data.len() as u64)
    }

    fn os_stream(data: &[u8], piece: usize) -> io::Result<String> {
        let mut h = StreamHasher::new(false, true);
        for c in data.chunks(piece.max(1)) {
            h.update(c);
        }
        h.finish().map(|(_, o)| o.unwrap())
    }

    fn make_byte_array(base: &[u8], mag: u32) -> Vec<u8> {
        let mut r = base.to_vec();
        for _ in 0..mag {
            let c = r.clone();
            r.extend_from_slice(&c);
        }
        r
    }

    fn make_tail_array(tail: &[u8]) -> Vec<u8> {
        let mut r = vec![0u8; OSHASH_CHUNK as usize];
        let mut t = vec![0u8; OSHASH_CHUNK as usize];
        let n = t.len();
        t[n - tail.len()..].copy_from_slice(tail);
        r.extend_from_slice(&t);
        r
    }

    // Vectors from pkg/hash/oshash/oshash_test.go.
    #[test]
    fn oshash_go_vectors() {
        assert!(os(b"").is_err());
        assert!(os(b"hello").is_err());
        assert!(os(b"12345678").is_err());
        assert_eq!(os(&make_byte_array(b"this is a test", 15)).unwrap(), "6a0eba04654d0b9b");
        assert_eq!(os(b"hello world").unwrap(), "d3e392dee38cd4df");
        assert_eq!(os(&make_tail_array(b"this is dumb")).unwrap(), "d5d6ddd820756920");
        assert_eq!(os(&make_tail_array(b"dumb is this")).unwrap(), "d5d6ddd820756920");
    }

    #[test]
    fn stream_matches_seek() {
        let mut data = Vec::new();
        let mut x: u32 = 12345;
        for _ in 0..300_001 {
            x = x.wrapping_mul(1_103_515_245).wrapping_add(12345);
            data.push((x >> 16) as u8);
        }
        for size in [9usize, 11, 200, 1001, 65535, 65536, 65537, 131_072, 200_000, 300_001] {
            let d = &data[..size];
            let want = os(d).unwrap();
            for piece in [1usize, 7, 4096, 65536, 1 << 20] {
                assert_eq!(os_stream(d, piece).unwrap(), want, "size {size} piece {piece}");
            }
        }
        assert!(os_stream(b"hello", 2).is_err());
    }

    #[test]
    fn md5_known() {
        let (h, n) = md5_reader(&mut Cursor::new(b"hello world")).unwrap();
        assert_eq!(h, "5eb63bbbe01eeed093cb22bb8f5acdc3");
        assert_eq!(n, 11);
        let (h, _) = md5_reader(&mut Cursor::new(b"")).unwrap();
        assert_eq!(h, "d41d8cd98f00b204e9800998ecf8427e");
        let mut s = StreamHasher::new(true, false);
        s.update(b"hello ");
        s.update(b"world");
        assert_eq!(s.finish().unwrap().0.unwrap(), "5eb63bbbe01eeed093cb22bb8f5acdc3");
    }
}
