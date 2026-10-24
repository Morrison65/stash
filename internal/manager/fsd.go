package manager

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/stashapp/stash/pkg/ffmpeg"
	"github.com/stashapp/stash/pkg/file"
	"github.com/stashapp/stash/pkg/logger"
	"github.com/stashapp/stash/pkg/remotefs"
)

// Environment variables configuring the connection to stash-fsd, the
// Windows service that owns the media library. These are system settings of
// the container deployment and are not part of the user-editable config.
const (
	envFsdAddress   = "STASH_FSD_ADDRESS"    // host:port of the gRPC listener
	envFsdSecret    = "STASH_FSD_SECRET"     // shared bearer token
	envFsdTLS       = "STASH_FSD_TLS"        // "true" to use TLS
	envFsdTLSVerify = "STASH_FSD_TLS_VERIFY" // "false" to accept self-signed certs
	// Comma-separated path prefixes that are served by stash-fsd in addition
	// to Windows paths. Only needed when running the Go reference server
	// (pkg/remotefs/localserver) against a Linux media folder.
	envFsdPrefixes = "STASH_FSD_REMOTE_PREFIXES"
)

// initRemoteLibrary connects to stash-fsd if configured and routes media
// library access (file system, hashing, ffprobe/ffmpeg) through it. Without
// configuration the local file system and ffmpeg are used as before.
func initRemoteLibrary() (*remotefs.Client, error) {
	addr := os.Getenv(envFsdAddress)
	if addr == "" {
		logger.Info("stash-fsd not configured: using the local file system for the media library")
		return nil, nil
	}

	useTLS, _ := strconv.ParseBool(os.Getenv(envFsdTLS))
	verify := true
	if v := os.Getenv(envFsdTLSVerify); v != "" {
		verify, _ = strconv.ParseBool(v)
	}

	client, err := remotefs.Dial(remotefs.Config{
		Address:               addr,
		Secret:                os.Getenv(envFsdSecret),
		TLS:                   useTLS,
		TLSInsecureSkipVerify: !verify,
	})
	if err != nil {
		return nil, err
	}

	isRemote := file.IsWindowsPath
	if v := os.Getenv(envFsdPrefixes); v != "" {
		var prefixes []string
		for _, p := range strings.Split(v, ",") {
			if p = strings.TrimSpace(p); p != "" {
				prefixes = append(prefixes, strings.TrimSuffix(p, "/"))
			}
		}
		isRemote = func(path string) bool {
			if file.IsWindowsPath(path) {
				return true
			}
			for _, p := range prefixes {
				if path == p || strings.HasPrefix(path, p+"/") {
					return true
				}
			}
			return false
		}
	}

	file.SetLibrary(&file.RoutingFS{
		Remote:   remotefs.NewFS(client),
		Local:    &file.OsFS{},
		IsRemote: isRemote,
	})

	ffmpeg.SetRemote(&fsdRunner{c: client}, isRemote)
	ffmpeg.OpenMediaFile = func(path string) (io.ReadCloser, error) {
		return file.Library().Open(path)
	}

	// Connectivity is checked but not required at startup: the Windows host
	// may come up after the containers. Calls fail until it is reachable.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if caps, err := client.Ping(ctx); err != nil {
		logger.Warnf("stash-fsd at %s is not reachable yet: %v", addr, err)
	} else {
		logger.Infof("Connected to stash-fsd %s at %s (ffmpeg %s, roots %v)",
			caps.ServiceVersion, addr, caps.FfmpegVersion, caps.Roots)
	}

	return client, nil
}

// fsdRunner adapts remotefs.Client to ffmpeg.RemoteRunner.
type fsdRunner struct {
	c *remotefs.Client
}

func (r *fsdRunner) Probe(ctx context.Context, path string, extraArgs []string) ([]byte, int64, error) {
	ret, err := r.c.Probe(ctx, path, extraArgs...)
	if err != nil {
		return nil, 0, err
	}
	return ret.JSON, ret.Size, nil
}

func (r *fsdRunner) Run(ctx context.Context, ffprobe bool, args []string, inputs []string, stdin []byte, workspace string, w io.Writer) ([]byte, error) {
	stderr, err := r.c.Run(ctx, remotefs.RunRequest{
		FFprobe:   ffprobe,
		Args:      args,
		Inputs:    inputs,
		Stdin:     stdin,
		Workspace: workspace,
		Priority:  remotefs.PriorityNormal,
	}, w)

	var runErr *remotefs.RunError
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

func (r *fsdRunner) FetchWorkspaceFile(ctx context.Context, workspace, name string, w io.Writer) error {
	return r.c.FetchWorkspaceFile(ctx, workspace, name, w)
}

func (r *fsdRunner) DeleteWorkspace(ctx context.Context, workspace string) error {
	return r.c.DeleteWorkspace(ctx, workspace)
}
