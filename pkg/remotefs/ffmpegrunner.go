package remotefs

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/stashapp/stash/pkg/ffmpeg"
)

// FFmpegRunner adapts a Client to ffmpeg.RemoteRunner: ffprobe and ffmpeg
// run on the machine that holds the media library.
type FFmpegRunner struct {
	C *Client
	// Priority of Run calls (PriorityNormal when zero).
	Priority Priority
}

var _ ffmpeg.RemoteRunner = (*FFmpegRunner)(nil)

func (r *FFmpegRunner) Probe(ctx context.Context, path string, extraArgs []string) ([]byte, int64, error) {
	ret, err := r.C.Probe(ctx, path, extraArgs...)
	if err != nil {
		return nil, 0, err
	}
	return ret.JSON, ret.Size, nil
}

func (r *FFmpegRunner) Run(ctx context.Context, ffprobe bool, args []string, inputs []string, stdin []byte, workspace string, w io.Writer) ([]byte, error) {
	priority := r.Priority
	if priority == 0 {
		priority = PriorityNormal
	}
	stderr, err := r.C.Run(ctx, RunRequest{
		FFprobe:   ffprobe,
		Args:      args,
		Inputs:    inputs,
		Stdin:     stdin,
		Workspace: workspace,
		Priority:  priority,
	}, w)

	var runErr *RunError
	if errors.As(err, &runErr) {
		return []byte(stderr), &ffmpeg.RemoteExitError{
			ExitCode: runErr.ExitCode,
			Stderr:   []byte(runErr.Stderr),
			TimedOut: runErr.TimedOut,
		}
	}
	if err != nil {
		return []byte(stderr), fmt.Errorf("stash-fsd: %w", err)
	}
	return []byte(stderr), nil
}

func (r *FFmpegRunner) FetchWorkspaceFile(ctx context.Context, workspace, name string, w io.Writer) error {
	return r.C.FetchWorkspaceFile(ctx, workspace, name, w)
}

func (r *FFmpegRunner) DeleteWorkspace(ctx context.Context, workspace string) error {
	return r.C.DeleteWorkspace(ctx, workspace)
}
