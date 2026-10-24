package remotefs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	pb "github.com/stashapp/stash/pkg/remotefs/stashfsv1"
)

// Priority is the scheduling class of an ffmpeg run on the service.
type Priority = pb.Priority

const (
	PriorityInteractive = pb.Priority_PRIORITY_INTERACTIVE
	PriorityNormal      = pb.Priority_PRIORITY_NORMAL
	PriorityBackground  = pb.Priority_PRIORITY_BACKGROUND
)

// ProbeResult is the raw ffprobe JSON plus file stats.
type ProbeResult struct {
	JSON    []byte
	Size    int64
	ModTime time.Time
}

// Probe runs ffprobe on the service.
func (c *Client) Probe(ctx context.Context, path string, extraArgs ...string) (*ProbeResult, error) {
	ret, err := c.media.Probe(ctx, &pb.ProbeRequest{Path: path, ExtraArgs: extraArgs})
	if err != nil {
		return nil, mapError("probe", path, err)
	}
	return &ProbeResult{
		JSON:    ret.Json,
		Size:    ret.Size,
		ModTime: time.Unix(0, ret.ModTimeUnixNano),
	}, nil
}

// RunRequest describes an ffmpeg/ffprobe invocation on the service. Args may
// reference {input:N} (inputs[N]) and {workspace}.
type RunRequest struct {
	FFprobe   bool
	Args      []string
	Inputs    []string
	Stdin     []byte
	Workspace string
	Timeout   time.Duration
	Priority  Priority
}

// RunError is returned when the remote process exits unsuccessfully.
type RunError struct {
	ExitCode int
	Stderr   string
	TimedOut bool
}

func (e *RunError) Error() string {
	if e.TimedOut {
		return "remote ffmpeg timed out"
	}
	return fmt.Sprintf("remote ffmpeg exited with code %d: %s", e.ExitCode, e.Stderr)
}

// Run executes the request and copies stdout to w (which may be nil to
// discard). It returns the tail of stderr, and a *RunError if the process
// fails.
func (c *Client) Run(ctx context.Context, req RunRequest, w io.Writer) (string, error) {
	tool := pb.Tool_TOOL_FFMPEG
	if req.FFprobe {
		tool = pb.Tool_TOOL_FFPROBE
	}

	stream, err := c.media.Run(ctx, &pb.RunRequest{
		Tool:           tool,
		Args:           req.Args,
		Inputs:         req.Inputs,
		Stdin:          req.Stdin,
		Workspace:      req.Workspace,
		TimeoutSeconds: int32(req.Timeout / time.Second),
		Priority:       req.Priority,
	})
	if err != nil {
		return "", mapError("run", "", err)
	}

	for {
		out, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return "", errors.New("remote ffmpeg: stream ended without status")
		}
		if err != nil {
			return "", mapError("run", "", err)
		}

		switch p := out.Payload.(type) {
		case *pb.RunOutput_Stdout:
			if w != nil {
				if _, err := w.Write(p.Stdout); err != nil {
					return "", err
				}
			}
		case *pb.RunOutput_Status:
			if p.Status.ExitCode != 0 || p.Status.TimedOut {
				return p.Status.Stderr, &RunError{
					ExitCode: int(p.Status.ExitCode),
					Stderr:   p.Status.Stderr,
					TimedOut: p.Status.TimedOut,
				}
			}
			return p.Status.Stderr, nil
		}
	}
}

// FetchWorkspaceFile copies a workspace file to w.
func (c *Client) FetchWorkspaceFile(ctx context.Context, workspace, name string, w io.Writer) error {
	stream, err := c.media.FetchWorkspaceFile(ctx, &pb.WorkspaceFileRequest{Workspace: workspace, Name: name})
	if err != nil {
		return mapError("fetch", name, err)
	}

	for {
		chunk, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return mapError("fetch", name, err)
		}
		if _, err := w.Write(chunk.Data); err != nil {
			return err
		}
	}
}

// ListWorkspace lists the files in a workspace.
func (c *Client) ListWorkspace(ctx context.Context, workspace string) ([]string, error) {
	ret, err := c.media.ListWorkspace(ctx, &pb.WorkspaceRequest{Workspace: workspace})
	if err != nil {
		return nil, mapError("list", workspace, err)
	}

	names := make([]string, len(ret.Entries))
	for i, e := range ret.Entries {
		names[i] = e.Name
	}
	return names, nil
}

// DeleteWorkspace removes a workspace and its files.
func (c *Client) DeleteWorkspace(ctx context.Context, workspace string) error {
	_, err := c.media.DeleteWorkspace(ctx, &pb.WorkspaceRequest{Workspace: workspace})
	return mapError("delete", workspace, err)
}
