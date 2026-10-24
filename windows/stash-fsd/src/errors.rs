//! Mapping of I/O errors to gRPC status codes (see the proto header).

use std::io;

use tonic::{Code, Status};

/// Windows error codes that std does not map to a useful `ErrorKind`.
pub fn windows_code(raw: i32) -> Option<Code> {
    Some(match raw {
        2 | 3 | 15 | 53 | 67 | 161 => Code::NotFound, // file/path/drive/network path not found, bad pathname
        5 | 1920 => Code::PermissionDenied,          // access denied, cannot access file
        80 | 183 => Code::AlreadyExists,             // file exists, already exists
        32 | 33 => Code::FailedPrecondition,         // sharing / lock violation (file in use)
        145 => Code::FailedPrecondition,             // directory not empty
        267 => Code::FailedPrecondition,             // the directory name is invalid (not a directory)
        17 => Code::FailedPrecondition,              // not same device
        21 | 1167 => Code::Unavailable,              // device not ready, device not connected
        123 => Code::InvalidArgument,                // invalid name syntax
        _ => return None,
    })
}

pub fn io_code(e: &io::Error) -> Code {
    if cfg!(windows) {
        if let Some(c) = e.raw_os_error().and_then(windows_code) {
            return c;
        }
    }
    kind_code(e.kind())
}

pub fn kind_code(kind: io::ErrorKind) -> Code {
    use io::ErrorKind as K;
    match kind {
        K::NotFound => Code::NotFound,
        K::PermissionDenied => Code::PermissionDenied,
        K::AlreadyExists => Code::AlreadyExists,
        K::DirectoryNotEmpty
        | K::NotADirectory
        | K::IsADirectory
        | K::ReadOnlyFilesystem
        | K::CrossesDevices
        | K::ResourceBusy
        | K::ExecutableFileBusy
        | K::FileTooLarge
        | K::StorageFull => Code::FailedPrecondition,
        K::InvalidInput | K::InvalidFilename | K::InvalidData => Code::InvalidArgument,
        K::TimedOut => Code::DeadlineExceeded,
        K::Interrupted => Code::Aborted,
        K::Unsupported => Code::Unimplemented,
        K::UnexpectedEof => Code::OutOfRange,
        _ => Code::Internal,
    }
}

/// Status for an I/O error with operation and path context.
pub fn io_status(op: &str, path: &str, e: &io::Error) -> Status {
    Status::new(io_code(e), format!("{op} {path}: {e}"))
}

/// The gRPC code as the integer used in `WalkError.code`.
pub fn code_i32(c: Code) -> i32 {
    c as i32
}

/// Converts a join error of a blocking task into a status.
pub fn join_status(e: tokio::task::JoinError) -> Status {
    Status::internal(format!("blocking task failed: {e}"))
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn kinds_map_to_proto_codes() {
        use io::ErrorKind as K;
        assert_eq!(kind_code(K::NotFound), Code::NotFound);
        assert_eq!(kind_code(K::PermissionDenied), Code::PermissionDenied);
        assert_eq!(kind_code(K::AlreadyExists), Code::AlreadyExists);
        assert_eq!(kind_code(K::DirectoryNotEmpty), Code::FailedPrecondition);
        assert_eq!(kind_code(K::NotADirectory), Code::FailedPrecondition);
        assert_eq!(kind_code(K::IsADirectory), Code::FailedPrecondition);
        assert_eq!(kind_code(K::InvalidInput), Code::InvalidArgument);
        assert_eq!(kind_code(K::Other), Code::Internal);
    }

    #[test]
    fn windows_codes() {
        assert_eq!(windows_code(2), Some(Code::NotFound));
        assert_eq!(windows_code(3), Some(Code::NotFound));
        assert_eq!(windows_code(5), Some(Code::PermissionDenied));
        assert_eq!(windows_code(80), Some(Code::AlreadyExists));
        assert_eq!(windows_code(183), Some(Code::AlreadyExists));
        assert_eq!(windows_code(145), Some(Code::FailedPrecondition));
        assert_eq!(windows_code(267), Some(Code::FailedPrecondition));
        assert_eq!(windows_code(32), Some(Code::FailedPrecondition));
        assert_eq!(windows_code(9999), None);
    }

    #[test]
    fn real_errors() {
        let e = std::fs::metadata("/definitely/not/here").unwrap_err();
        assert_eq!(io_code(&e), Code::NotFound);
        let s = io_status("stat", "/definitely/not/here", &e);
        assert_eq!(s.code(), Code::NotFound);
        assert!(s.message().contains("/definitely/not/here"));

        let dir = tempfile::tempdir().unwrap();
        std::fs::write(dir.path().join("f"), b"x").unwrap();
        let e = std::fs::create_dir(dir.path().join("f")).unwrap_err();
        assert_eq!(io_code(&e), Code::AlreadyExists);
        std::fs::create_dir(dir.path().join("d")).unwrap();
        std::fs::write(dir.path().join("d/x"), b"x").unwrap();
        let e = std::fs::remove_dir(dir.path().join("d")).unwrap_err();
        assert_eq!(io_code(&e), Code::FailedPrecondition);
        let e = std::fs::read_dir(dir.path().join("f")).unwrap_err();
        assert_eq!(io_code(&e), Code::FailedPrecondition);
    }
}
