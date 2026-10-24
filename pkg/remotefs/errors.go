package remotefs

import (
	"errors"
	"io/fs"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// ErrUnavailable is returned (wrapped) when stash-fsd cannot be reached.
var ErrUnavailable = errors.New("stash-fsd unavailable")

// ErrUnauthenticated is returned (wrapped) when the shared secret is rejected.
var ErrUnauthenticated = errors.New("stash-fsd rejected the shared secret")

// mapError converts a gRPC status into the fs error that the equivalent os
// call would have returned, wrapped in an *fs.PathError so that
// errors.Is(err, fs.ErrNotExist) and friends keep working in callers.
func mapError(op, path string, err error) error {
	if err == nil {
		return nil
	}

	st, ok := status.FromError(err)
	if !ok {
		return &fs.PathError{Op: op, Path: path, Err: err}
	}

	var base error
	switch st.Code() {
	case codes.NotFound:
		base = fs.ErrNotExist
	case codes.PermissionDenied:
		base = fs.ErrPermission
	case codes.AlreadyExists:
		base = fs.ErrExist
	case codes.InvalidArgument:
		base = fs.ErrInvalid
	case codes.Unavailable, codes.DeadlineExceeded:
		base = ErrUnavailable
	case codes.Unauthenticated:
		base = ErrUnauthenticated
	default:
		base = errors.New(st.Message())
	}

	// os.IsNotExist and friends only unwrap one level, so the fs sentinels
	// must be the direct Err of the PathError. Other errors keep the remote
	// message for logs.
	switch base {
	case fs.ErrNotExist, fs.ErrPermission, fs.ErrExist, fs.ErrInvalid:
	default:
		if st.Message() != "" && base.Error() != st.Message() {
			base = &detailedError{base: base, msg: st.Message()}
		}
	}

	return &fs.PathError{Op: op, Path: path, Err: base}
}

// detailedError keeps the sentinel for errors.Is while carrying the remote
// message for logs.
type detailedError struct {
	base error
	msg  string
}

func (e *detailedError) Error() string { return e.base.Error() + ": " + e.msg }
func (e *detailedError) Unwrap() error { return e.base }
