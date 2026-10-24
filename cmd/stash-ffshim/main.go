// Command stash-ffshim stands in for ffmpeg and ffprobe in the API image
// (installed as /usr/local/bin/ffmpeg and /usr/local/bin/ffprobe, ahead of
// the real binaries in /usr/bin).
//
// Media library files (U:/..., UNC:/server/share/...) exist only on Windows.
// When a call names such a file - typically a plugin probing a scene file,
// e.g. Video Chapter Markers' `ffprobe -show_chapters <path>` - it runs on
// Windows through stash-fsd, the same way Stash's own probes do. Every other
// call is handed to the real binary unchanged.
//
// ffprobe: every argument that is a library path is an input. ffmpeg: inputs
// given with -i; an output file in the container is fetched back afterwards.
// stdout is streamed, stderr is printed when the process ends, and the exit
// code is passed on.
//
// Installed as stash-libfs it gives plugin code read-only access to library
// files instead (see libfs.go).
//
// Environment: STASH_FSD_ADDRESS, STASH_FSD_SECRET, STASH_FSD_TLS,
// STASH_FSD_TLS_VERIFY and STASH_FSD_REMOTE_PREFIXES as for Stash itself;
// STASH_FFSHIM_REAL_DIR is where the real binaries are (default /usr/bin).
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/stashapp/stash/pkg/ffmpeg"
	"github.com/stashapp/stash/pkg/file"
	"github.com/stashapp/stash/pkg/remotefs"
)

func main() {
	tool := strings.TrimSuffix(filepath.Base(os.Args[0]), ".exe")
	if tool == "stash-libfs" {
		os.Exit(libfsMain(os.Args[1:]))
	}
	if tool != "ffprobe" && tool != "ffmpeg" {
		fmt.Fprintln(os.Stderr, "stash-ffshim: install as ffmpeg, ffprobe or stash-libfs")
		os.Exit(2)
	}
	args := os.Args[1:]

	isRemote := remoteTest()
	if os.Getenv("STASH_FSD_ADDRESS") == "" || !hasRemoteInput(tool, args, isRemote) {
		execReal(tool, args)
	}
	os.Exit(runRemote(tool, args, isRemote))
}

// remoteTest decides which paths live on Windows, like Stash does.
func remoteTest() func(string) bool {
	var prefixes []string
	for _, p := range strings.Split(os.Getenv("STASH_FSD_REMOTE_PREFIXES"), ",") {
		if p = strings.TrimSpace(p); p != "" {
			prefixes = append(prefixes, strings.TrimSuffix(p, "/"))
		}
	}
	return func(path string) bool {
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

func hasRemoteInput(tool string, args []string, isRemote func(string) bool) bool {
	_, inputs := probeInputs(args, isRemote)
	if tool == "ffprobe" {
		return len(inputs) > 0
	}
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "-i" && isRemote(args[i+1]) {
			return true
		}
	}
	return false
}

// probeInputs replaces the library paths in an ffprobe command line with
// {input:N} placeholders.
func probeInputs(args []string, isRemote func(string) bool) ([]string, []string) {
	out := make([]string, len(args))
	var inputs []string
	for i, a := range args {
		if isRemote(a) {
			out[i] = "{input:" + strconv.Itoa(len(inputs)) + "}"
			inputs = append(inputs, file.NormalizeLibraryPath(a))
			continue
		}
		out[i] = a
	}
	return out, inputs
}

func runRemote(tool string, args []string, isRemote func(string) bool) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	client, err := dialFsd()
	if err != nil {
		fmt.Fprintf(os.Stderr, "stash-ffshim: connecting to stash-fsd: %v\n", err)
		return 1
	}
	defer client.Close()

	runner := &remotefs.FFmpegRunner{C: client}
	var stderr []byte
	if tool == "ffprobe" {
		remoteArgs, inputs := probeInputs(args, isRemote)
		stderr, err = runner.Run(ctx, true, remoteArgs, inputs, nil, "", os.Stdout)
	} else {
		ffmpeg.SetRemote(runner, isRemote)
		var handled bool
		handled, stderr, err = ffmpeg.RunRemoteIfNeeded(ctx, args, nil, os.Stdout)
		if !handled {
			execReal(tool, args)
		}
	}
	os.Stderr.Write(stderr)

	var exitErr *ffmpeg.RemoteExitError
	switch {
	case errors.As(err, &exitErr):
		if exitErr.TimedOut || exitErr.ExitCode == 0 {
			return 1
		}
		return exitErr.ExitCode
	case err != nil:
		fmt.Fprintf(os.Stderr, "stash-ffshim: %v\n", err)
		return 1
	}
	return 0
}

// dialFsd connects to stash-fsd with Stash's environment.
func dialFsd() (*remotefs.Client, error) {
	if os.Getenv("STASH_FSD_ADDRESS") == "" {
		return nil, errors.New("STASH_FSD_ADDRESS is not set")
	}
	verify := true
	if v := os.Getenv("STASH_FSD_TLS_VERIFY"); v != "" {
		verify, _ = strconv.ParseBool(v)
	}
	useTLS, _ := strconv.ParseBool(os.Getenv("STASH_FSD_TLS"))
	return remotefs.Dial(remotefs.Config{
		Address:               os.Getenv("STASH_FSD_ADDRESS"),
		Secret:                os.Getenv("STASH_FSD_SECRET"),
		TLS:                   useTLS,
		TLSInsecureSkipVerify: !verify,
	})
}

// execReal replaces this process with the real binary.
func execReal(tool string, args []string) {
	dir := os.Getenv("STASH_FFSHIM_REAL_DIR")
	if dir == "" {
		dir = "/usr/bin"
	}
	real := filepath.Join(dir, tool)
	err := syscall.Exec(real, append([]string{tool}, args...), os.Environ())
	fmt.Fprintf(os.Stderr, "stash-ffshim: running %s: %v\n", real, err)
	os.Exit(127)
}
