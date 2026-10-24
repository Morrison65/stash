package ffmpeg

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
)

// RemoteRunner executes ffprobe/ffmpeg on the machine that holds the media
// library (stash-fsd). It is implemented by an adapter around
// pkg/remotefs.Client so that this package stays free of gRPC.
type RemoteRunner interface {
	// Probe returns the ffprobe JSON for path and the file size.
	Probe(ctx context.Context, path string, extraArgs []string) (json []byte, size int64, err error)
	// Run executes ffmpeg (or ffprobe) with args where {input:N} refers to
	// inputs[N] and {workspace} to a per-call scratch directory. stdout is
	// copied to w and the tail of stderr is returned. A failing process
	// returns a *RemoteExitError.
	Run(ctx context.Context, ffprobe bool, args []string, inputs []string, stdin []byte, workspace string, w io.Writer) (stderr []byte, err error)
	FetchWorkspaceFile(ctx context.Context, workspace, name string, w io.Writer) error
	DeleteWorkspace(ctx context.Context, workspace string) error
}

// RemoteExitError is returned when a remote ffmpeg process fails.
type RemoteExitError struct {
	ExitCode int
	Stderr   []byte
	TimedOut bool
}

func (e *RemoteExitError) Error() string {
	if e.TimedOut {
		return "remote ffmpeg timed out"
	}
	return fmt.Sprintf("remote ffmpeg exit status %d", e.ExitCode)
}

// StderrOf returns the captured stderr of a failed local or remote ffmpeg
// run, if available.
func StderrOf(err error) []byte {
	var remoteErr *RemoteExitError
	if errors.As(err, &remoteErr) {
		return remoteErr.Stderr
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.Stderr
	}
	return nil
}

var (
	remoteMu       sync.RWMutex
	remoteRunner   RemoteRunner
	remotePathTest func(string) bool
)

// SetRemote configures remote execution. isRemote decides which paths live
// on the remote machine (normally file.IsWindowsPath).
func SetRemote(r RemoteRunner, isRemote func(path string) bool) {
	remoteMu.Lock()
	defer remoteMu.Unlock()
	remoteRunner = r
	remotePathTest = isRemote
}

// remoteFor returns the runner if path must be processed remotely.
func remoteFor(path string) RemoteRunner {
	remoteMu.RLock()
	defer remoteMu.RUnlock()
	if remoteRunner == nil || remotePathTest == nil || !remotePathTest(path) {
		return nil
	}
	return remoteRunner
}

// IsRemotePath reports whether path is processed by the remote runner.
func IsRemotePath(path string) bool {
	return remoteFor(path) != nil
}

// remoteInvocation is an ffmpeg command line rewritten for remote execution.
type remoteInvocation struct {
	runner    RemoteRunner
	args      []string
	inputs    []string
	outFile   string // local destination of the output file, if any
	outName   string // name of the output inside the workspace
	workspace string
}

func newWorkspaceID() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return "gen-" + hex.EncodeToString(b)
}

// planRemote decides whether args must run remotely: that is the case when
// any input (-i) is a remote library path. Remote inputs become {input:N}
// placeholders; a local output file (the last argument) is redirected into a
// workspace and fetched afterwards; "-" / pipe outputs stream back.
func planRemote(args []string) *remoteInvocation {
	var inv *remoteInvocation

	out := make([]string, len(args))
	copy(out, args)

	for i := 0; i < len(out)-1; i++ {
		if out[i] != "-i" {
			continue
		}
		in := out[i+1]
		r := remoteFor(in)
		if r == nil {
			continue
		}
		if inv == nil {
			inv = &remoteInvocation{runner: r}
		}
		out[i+1] = fmt.Sprintf("{input:%d}", len(inv.inputs))
		inv.inputs = append(inv.inputs, in)
		i++
	}

	if inv == nil {
		return nil
	}

	if len(out) > 0 {
		last := out[len(out)-1]
		if last != "-" && !strings.HasPrefix(last, "pipe:") && filepath.IsAbs(last) && remoteFor(last) == nil {
			inv.workspace = newWorkspaceID()
			inv.outFile = last
			inv.outName = "out" + filepath.Ext(last)
			out[len(out)-1] = "{workspace}/" + inv.outName
		}
	}

	inv.args = out
	return inv
}

// run executes the invocation, streaming stdout to w and fetching the output
// file (if any) to its local destination. It returns the tail of stderr.
func (inv *remoteInvocation) run(ctx context.Context, stdin io.Reader, w io.Writer) ([]byte, error) {
	var stdinData []byte
	if stdin != nil {
		var err error
		if stdinData, err = io.ReadAll(stdin); err != nil {
			return nil, fmt.Errorf("reading stdin for remote ffmpeg: %w", err)
		}
	}

	if inv.workspace != "" {
		defer func() {
			// use a fresh context: ctx may already be cancelled
			_ = inv.runner.DeleteWorkspace(context.Background(), inv.workspace)
		}()
	}

	stderr, err := inv.runner.Run(ctx, false, inv.args, inv.inputs, stdinData, inv.workspace, w)
	if err != nil {
		return stderr, err
	}

	if inv.outFile == "" {
		return stderr, nil
	}

	f, err := os.Create(inv.outFile)
	if err != nil {
		return stderr, fmt.Errorf("creating %s: %w", inv.outFile, err)
	}

	if err := inv.runner.FetchWorkspaceFile(ctx, inv.workspace, inv.outName, f); err != nil {
		f.Close()
		_ = os.Remove(inv.outFile)
		return stderr, fmt.Errorf("fetching remote ffmpeg output: %w", err)
	}

	return stderr, f.Close()
}

// RunRemoteIfNeeded runs args remotely if any input is a remote library path.
// It returns handled=false if the command should run locally. stderr holds
// the tail of the remote process's stderr.
func RunRemoteIfNeeded(ctx context.Context, args []string, stdin io.Reader, stdout io.Writer) (handled bool, stderr []byte, err error) {
	inv := planRemote(args)
	if inv == nil {
		return false, nil, nil
	}
	stderr, err = inv.run(ctx, stdin, stdout)
	return true, stderr, err
}
