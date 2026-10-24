// Package localserver is a Go reference implementation of the stash-fsd
// gRPC contract backed by the local file system. It is used by tests and
// for running the container stack against a media folder on a Linux host
// (no Windows service). Paths are used as given (no Windows conversion).
package localserver

import (
	"context"
	"crypto/md5"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/stashapp/stash/pkg/fsutil"
	"github.com/stashapp/stash/pkg/hash/oshash"
	pb "github.com/stashapp/stash/pkg/remotefs/stashfsv1"
)

const (
	maxReadAt      = 8 << 20
	streamChunk    = 1 << 20
	defaultBatch   = 512
	maxStderrBytes = 64 << 10
)

var workspaceRE = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// Config configures the server.
type Config struct {
	Secret       string
	Roots        []string
	WorkspaceDir string
	FFmpegPath   string
	FFprobePath  string
}

// Server implements the FileSystem, Hasher and Media services.
type Server struct {
	pb.UnimplementedFileSystemServer
	pb.UnimplementedHasherServer
	pb.UnimplementedMediaServer

	cfg Config
}

// New returns a server for cfg.
func New(cfg Config) *Server {
	if cfg.FFmpegPath == "" {
		cfg.FFmpegPath = "ffmpeg"
	}
	if cfg.FFprobePath == "" {
		cfg.FFprobePath = "ffprobe"
	}
	return &Server{cfg: cfg}
}

// Register registers all services and the auth interceptors on s.
func (s *Server) Register(g *grpc.Server) {
	pb.RegisterFileSystemServer(g, s)
	pb.RegisterHasherServer(g, s)
	pb.RegisterMediaServer(g, s)
}

// ServerOptions returns grpc options enforcing the bearer token.
func (s *Server) ServerOptions() []grpc.ServerOption {
	return []grpc.ServerOption{
		grpc.UnaryInterceptor(func(ctx context.Context, req any, _ *grpc.UnaryServerInfo, h grpc.UnaryHandler) (any, error) {
			if err := s.auth(ctx); err != nil {
				return nil, err
			}
			return h(ctx, req)
		}),
		grpc.StreamInterceptor(func(srv any, ss grpc.ServerStream, _ *grpc.StreamServerInfo, h grpc.StreamHandler) error {
			if err := s.auth(ss.Context()); err != nil {
				return err
			}
			return h(srv, ss)
		}),
		grpc.MaxRecvMsgSize(16 << 20),
		grpc.MaxSendMsgSize(16 << 20),
	}
}

func (s *Server) auth(ctx context.Context) error {
	md, _ := metadata.FromIncomingContext(ctx)
	want := "Bearer " + s.cfg.Secret
	for _, v := range md.Get("authorization") {
		if subtle.ConstantTimeCompare([]byte(v), []byte(want)) == 1 {
			return nil
		}
	}
	return status.Error(codes.Unauthenticated, "invalid token")
}

// check validates that path is below one of the roots.
func (s *Server) check(path string) (string, error) {
	if path == "" {
		return "", status.Error(codes.InvalidArgument, "empty path")
	}
	clean := filepath.Clean(path)
	for _, r := range s.cfg.Roots {
		r = filepath.Clean(r)
		if clean == r || strings.HasPrefix(clean, strings.TrimSuffix(r, string(filepath.Separator))+string(filepath.Separator)) {
			return clean, nil
		}
	}
	return "", status.Errorf(codes.PermissionDenied, "%s is outside the configured roots", path)
}

func toStatus(err error) error {
	if err == nil {
		return nil
	}
	if _, ok := status.FromError(err); ok {
		return err
	}
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return status.Error(codes.NotFound, err.Error())
	case errors.Is(err, fs.ErrPermission):
		return status.Error(codes.PermissionDenied, err.Error())
	case errors.Is(err, fs.ErrExist):
		return status.Error(codes.AlreadyExists, err.Error())
	default:
		return status.Error(codes.FailedPrecondition, err.Error())
	}
}

func info(path string, fi fs.FileInfo) *pb.FileInfo {
	return &pb.FileInfo{
		Name:            fi.Name(),
		Path:            path,
		Size:            fi.Size(),
		Mode:            uint32(fi.Mode()),
		ModTimeUnixNano: fi.ModTime().UnixNano(),
		IsDir:           fi.IsDir(),
		IsSymlink:       fi.Mode()&fs.ModeSymlink != 0,
	}
}

// ---------------------------------------------------------------------------
// FileSystem
// ---------------------------------------------------------------------------

func (s *Server) Stat(_ context.Context, req *pb.PathRequest) (*pb.FileInfo, error) {
	p, err := s.check(req.Path)
	if err != nil {
		return nil, err
	}
	fi, err := os.Stat(p)
	if err != nil {
		return nil, toStatus(err)
	}
	return info(p, fi), nil
}

func (s *Server) Lstat(_ context.Context, req *pb.PathRequest) (*pb.FileInfo, error) {
	p, err := s.check(req.Path)
	if err != nil {
		return nil, err
	}
	fi, err := os.Lstat(p)
	if err != nil {
		return nil, toStatus(err)
	}
	return info(p, fi), nil
}

func readDir(p string) ([]*pb.FileInfo, error) {
	entries, err := os.ReadDir(p)
	if err != nil {
		return nil, err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	ret := make([]*pb.FileInfo, 0, len(entries))
	for _, e := range entries {
		fi, err := e.Info()
		if err != nil {
			continue
		}
		ret = append(ret, info(filepath.Join(p, e.Name()), fi))
	}
	return ret, nil
}

func (s *Server) ReadDir(_ context.Context, req *pb.PathRequest) (*pb.ReadDirResponse, error) {
	p, err := s.check(req.Path)
	if err != nil {
		return nil, err
	}
	entries, err := readDir(p)
	if err != nil {
		return nil, toStatus(err)
	}
	return &pb.ReadDirResponse{Entries: entries}, nil
}

func (s *Server) Walk(req *pb.WalkRequest, stream grpc.ServerStreamingServer[pb.WalkBatch]) error {
	root, err := s.check(req.Root)
	if err != nil {
		return err
	}
	batchSize := int(req.BatchSize)
	if batchSize <= 0 {
		batchSize = defaultBatch
	}

	batch := &pb.WalkBatch{}
	flush := func(force bool) error {
		if len(batch.Entries)+len(batch.Errors) == 0 || (!force && len(batch.Entries) < batchSize) {
			return nil
		}
		err := stream.Send(batch)
		batch = &pb.WalkBatch{}
		return err
	}

	visited := map[string]bool{}
	var walk func(p string, fi fs.FileInfo) error
	walk = func(p string, fi fs.FileInfo) error {
		batch.Entries = append(batch.Entries, info(p, fi))
		if err := flush(false); err != nil {
			return err
		}

		dir := fi.IsDir()
		if !dir && req.FollowSymlinks && fi.Mode()&fs.ModeSymlink != 0 {
			if target, err := os.Stat(p); err == nil && target.IsDir() {
				dir = true
			}
		}
		if !dir {
			return nil
		}

		if real, err := filepath.EvalSymlinks(p); err == nil {
			if visited[real] {
				return nil
			}
			visited[real] = true
		}

		entries, err := readDir(p)
		if err != nil {
			batch.Errors = append(batch.Errors, &pb.WalkError{Path: p, Message: err.Error(), Code: int32(status.Code(toStatus(err)))})
			return nil
		}
		for _, e := range entries {
			if err := stream.Context().Err(); err != nil {
				return err
			}
			if err := walk(e.Path, &protoInfo{e}); err != nil {
				return err
			}
		}
		return nil
	}

	fi, err := os.Lstat(root)
	if err != nil {
		return toStatus(err)
	}
	if err := walk(root, fi); err != nil {
		return err
	}
	return flush(true)
}

// protoInfo adapts a *pb.FileInfo back to fs.FileInfo for the walker.
type protoInfo struct{ i *pb.FileInfo }

func (p *protoInfo) Name() string       { return p.i.Name }
func (p *protoInfo) Size() int64        { return p.i.Size }
func (p *protoInfo) Mode() fs.FileMode  { return fs.FileMode(p.i.Mode) }
func (p *protoInfo) ModTime() time.Time { return time.Unix(0, p.i.ModTimeUnixNano) }
func (p *protoInfo) IsDir() bool        { return p.i.IsDir }
func (p *protoInfo) Sys() any           { return nil }

func (s *Server) EvalSymlinks(_ context.Context, req *pb.PathRequest) (*pb.PathResponse, error) {
	p, err := s.check(req.Path)
	if err != nil {
		return nil, err
	}
	ret, err := filepath.EvalSymlinks(p)
	if err != nil {
		return nil, toStatus(err)
	}
	return &pb.PathResponse{Path: ret}, nil
}

func (s *Server) IsPathCaseSensitive(_ context.Context, req *pb.PathRequest) (*pb.BoolResponse, error) {
	p, err := s.check(req.Path)
	if err != nil {
		return nil, err
	}
	ret, err := fsutil.IsFsPathCaseSensitive(p)
	if err != nil {
		return nil, toStatus(err)
	}
	return &pb.BoolResponse{Value: ret}, nil
}

func (s *Server) ReadAt(_ context.Context, req *pb.ReadAtRequest) (*pb.ReadAtResponse, error) {
	p, err := s.check(req.Path)
	if err != nil {
		return nil, err
	}
	if req.Length < 0 || req.Length > maxReadAt || req.Offset < 0 {
		return nil, status.Error(codes.InvalidArgument, "invalid offset/length")
	}
	f, err := os.Open(p)
	if err != nil {
		return nil, toStatus(err)
	}
	defer f.Close()

	buf := make([]byte, req.Length)
	n, err := f.ReadAt(buf, req.Offset)
	eof := errors.Is(err, io.EOF)
	if err != nil && !eof {
		return nil, toStatus(err)
	}
	return &pb.ReadAtResponse{Data: buf[:n], Eof: eof}, nil
}

func (s *Server) ReadFile(req *pb.ReadFileRequest, stream grpc.ServerStreamingServer[pb.DataChunk]) error {
	p, err := s.check(req.Path)
	if err != nil {
		return err
	}
	f, err := os.Open(p)
	if err != nil {
		return toStatus(err)
	}
	defer f.Close()

	if req.MaxSize > 0 {
		fi, err := f.Stat()
		if err != nil {
			return toStatus(err)
		}
		if fi.Size() > req.MaxSize {
			return status.Errorf(codes.FailedPrecondition, "file is larger than %d bytes", req.MaxSize)
		}
	}

	var r io.Reader = io.NewSectionReader(f, req.Offset, 1<<62)
	if req.Length > 0 {
		r = io.LimitReader(r, req.Length)
	}
	return sendChunks(r, stream)
}

func sendChunks(r io.Reader, stream grpc.ServerStreamingServer[pb.DataChunk]) error {
	buf := make([]byte, streamChunk)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			if serr := stream.Send(&pb.DataChunk{Data: append([]byte(nil), buf[:n]...)}); serr != nil {
				return serr
			}
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return toStatus(err)
		}
	}
}

func (s *Server) Mkdir(_ context.Context, req *pb.MkdirRequest) (*pb.Empty, error) {
	p, err := s.check(req.Path)
	if err != nil {
		return nil, err
	}
	if req.All {
		err = os.MkdirAll(p, 0755)
	} else {
		err = os.Mkdir(p, 0755)
	}
	return &pb.Empty{}, toStatus(err)
}

func (s *Server) Rename(_ context.Context, req *pb.RenameRequest) (*pb.Empty, error) {
	oldp, err := s.check(req.OldPath)
	if err != nil {
		return nil, err
	}
	newp, err := s.check(req.NewPath)
	if err != nil {
		return nil, err
	}
	return &pb.Empty{}, toStatus(fsutil.SafeMove(oldp, newp))
}

func (s *Server) Remove(_ context.Context, req *pb.PathRequest) (*pb.Empty, error) {
	p, err := s.check(req.Path)
	if err != nil {
		return nil, err
	}
	return &pb.Empty{}, toStatus(os.Remove(p))
}

func (s *Server) RemoveAll(_ context.Context, req *pb.PathRequest) (*pb.Empty, error) {
	p, err := s.check(req.Path)
	if err != nil {
		return nil, err
	}
	return &pb.Empty{}, toStatus(os.RemoveAll(p))
}

func (s *Server) MoveToTrash(_ context.Context, req *pb.TrashRequest) (*pb.PathResponse, error) {
	p, err := s.check(req.Path)
	if err != nil {
		return nil, err
	}
	trash, err := s.check(req.TrashDir)
	if err != nil {
		return nil, err
	}
	ret, err := fsutil.MoveToTrash(p, trash)
	if err != nil {
		return nil, toStatus(err)
	}
	return &pb.PathResponse{Path: ret}, nil
}

// ---------------------------------------------------------------------------
// Hasher
// ---------------------------------------------------------------------------

func (s *Server) Fingerprint(_ context.Context, req *pb.FingerprintRequest) (*pb.FingerprintResponse, error) {
	p, err := s.check(req.Path)
	if err != nil {
		return nil, err
	}
	if req.ZipEntry != "" {
		return nil, status.Error(codes.Unimplemented, "zip entries are not supported by localserver")
	}

	f, err := os.Open(p)
	if err != nil {
		return nil, toStatus(err)
	}
	defer f.Close()

	fi, err := f.Stat()
	if err != nil {
		return nil, toStatus(err)
	}

	ret := &pb.FingerprintResponse{Size: fi.Size()}
	for _, a := range req.Algorithms {
		// every algorithm reads from the start of the file
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			return nil, toStatus(err)
		}
		switch a {
		case pb.HashAlgorithm_HASH_ALGORITHM_OSHASH:
			h, err := oshash.FromReader(f, fi.Size())
			if err != nil {
				return nil, toStatus(err)
			}
			ret.Oshash = h
		case pb.HashAlgorithm_HASH_ALGORITHM_MD5:
			h := md5.New()
			if _, err := io.Copy(h, f); err != nil {
				return nil, toStatus(err)
			}
			ret.Md5 = hex.EncodeToString(h.Sum(nil))
		}
	}
	return ret, nil
}

// ---------------------------------------------------------------------------
// Media
// ---------------------------------------------------------------------------

func (s *Server) Capabilities(context.Context, *pb.Empty) (*pb.CapabilitiesResponse, error) {
	return &pb.CapabilitiesResponse{
		ServiceVersion:      "localserver",
		Roots:               s.cfg.Roots,
		MaxConcurrentFfmpeg: 2,
		MaxConcurrentHash:   4,
	}, nil
}

func (s *Server) Probe(ctx context.Context, req *pb.ProbeRequest) (*pb.ProbeResponse, error) {
	p, err := s.check(req.Path)
	if err != nil {
		return nil, err
	}
	fi, err := os.Stat(p)
	if err != nil {
		return nil, toStatus(err)
	}
	args := []string{"-v", "quiet", "-print_format", "json", "-show_format", "-show_streams", "-show_error"}
	args = append(args, req.ExtraArgs...)
	args = append(args, p)
	out, err := exec.CommandContext(ctx, s.cfg.FFprobePath, args...).Output()
	if err != nil {
		return nil, status.Errorf(codes.Internal, "ffprobe: %v", err)
	}
	return &pb.ProbeResponse{Json: out, Size: fi.Size(), ModTimeUnixNano: fi.ModTime().UnixNano()}, nil
}

var placeholderRE = regexp.MustCompile(`\{input:(\d+)\}|\{workspace\}`)

func (s *Server) workspace(id string, create bool) (string, error) {
	if !workspaceRE.MatchString(id) {
		return "", status.Error(codes.InvalidArgument, "invalid workspace id")
	}
	if s.cfg.WorkspaceDir == "" {
		return "", status.Error(codes.FailedPrecondition, "no workspace dir configured")
	}
	dir := filepath.Join(s.cfg.WorkspaceDir, id)
	if create {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return "", toStatus(err)
		}
	}
	return dir, nil
}

func (s *Server) Run(req *pb.RunRequest, stream grpc.ServerStreamingServer[pb.RunOutput]) error {
	inputs := make([]string, len(req.Inputs))
	for i, in := range req.Inputs {
		p, err := s.check(in)
		if err != nil {
			return err
		}
		inputs[i] = p
	}

	var ws string
	if req.Workspace != "" {
		var err error
		if ws, err = s.workspace(req.Workspace, true); err != nil {
			return err
		}
	}

	var subErr error
	args := make([]string, len(req.Args))
	for i, a := range req.Args {
		args[i] = placeholderRE.ReplaceAllStringFunc(a, func(m string) string {
			if m == "{workspace}" {
				if ws == "" {
					subErr = status.Error(codes.InvalidArgument, "{workspace} used without workspace")
				}
				return ws
			}
			n, _ := strconv.Atoi(m[len("{input:") : len(m)-1])
			if n >= len(inputs) {
				subErr = status.Errorf(codes.InvalidArgument, "input %d not provided", n)
				return ""
			}
			return inputs[n]
		})
	}
	if subErr != nil {
		return subErr
	}

	tool := s.cfg.FFmpegPath
	if req.Tool == pb.Tool_TOOL_FFPROBE {
		tool = s.cfg.FFprobePath
	}

	ctx := stream.Context()
	if req.TimeoutSeconds > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Duration(req.TimeoutSeconds)*time.Second)
		defer cancel()
	}

	cmd := exec.CommandContext(ctx, tool, args...)
	if len(req.Stdin) > 0 {
		cmd.Stdin = strings.NewReader(string(req.Stdin))
	}
	var stderr tailBuffer
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return status.Error(codes.Internal, err.Error())
	}
	if err := cmd.Start(); err != nil {
		return status.Errorf(codes.FailedPrecondition, "starting %s: %v", tool, err)
	}

	buf := make([]byte, streamChunk)
	for {
		n, rerr := stdout.Read(buf)
		if n > 0 {
			if err := stream.Send(&pb.RunOutput{Payload: &pb.RunOutput_Stdout{Stdout: append([]byte(nil), buf[:n]...)}}); err != nil {
				_ = cmd.Process.Kill()
				return err
			}
		}
		if rerr != nil {
			break
		}
	}

	werr := cmd.Wait()
	st := &pb.RunStatus{Stderr: stderr.String()}
	if werr != nil {
		var ee *exec.ExitError
		if errors.As(werr, &ee) {
			st.ExitCode = int32(ee.ExitCode())
		} else {
			st.ExitCode = -1
		}
		st.TimedOut = errors.Is(ctx.Err(), context.DeadlineExceeded)
	}
	return stream.Send(&pb.RunOutput{Payload: &pb.RunOutput_Status{Status: st}})
}

func (s *Server) FetchWorkspaceFile(req *pb.WorkspaceFileRequest, stream grpc.ServerStreamingServer[pb.DataChunk]) error {
	dir, err := s.workspace(req.Workspace, false)
	if err != nil {
		return err
	}
	name := filepath.Clean(req.Name)
	if filepath.IsAbs(name) || strings.HasPrefix(name, "..") {
		return status.Error(codes.InvalidArgument, "invalid name")
	}
	f, err := os.Open(filepath.Join(dir, name))
	if err != nil {
		return toStatus(err)
	}
	defer f.Close()
	return sendChunks(f, stream)
}

func (s *Server) ListWorkspace(_ context.Context, req *pb.WorkspaceRequest) (*pb.ListWorkspaceResponse, error) {
	dir, err := s.workspace(req.Workspace, false)
	if err != nil {
		return nil, err
	}
	entries, err := readDir(dir)
	if err != nil {
		return nil, toStatus(err)
	}
	return &pb.ListWorkspaceResponse{Entries: entries}, nil
}

func (s *Server) DeleteWorkspace(_ context.Context, req *pb.WorkspaceRequest) (*pb.Empty, error) {
	dir, err := s.workspace(req.Workspace, false)
	if err != nil {
		return nil, err
	}
	return &pb.Empty{}, toStatus(os.RemoveAll(dir))
}

// tailBuffer keeps the last maxStderrBytes written to it.
type tailBuffer struct {
	b []byte
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.b = append(t.b, p...)
	if len(t.b) > maxStderrBytes {
		t.b = t.b[len(t.b)-maxStderrBytes:]
	}
	return len(p), nil
}

func (t *tailBuffer) String() string { return string(t.b) }

// String implements fmt.Stringer for debugging.
func (s *Server) String() string {
	return fmt.Sprintf("localserver(roots=%v)", s.cfg.Roots)
}
