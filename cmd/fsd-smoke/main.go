// Command fsd-smoke is a development tool that exercises a running stash-fsd
// over gRPC and checks the results against the Go reference implementations
// (pkg/hash/md5, pkg/hash/oshash).
//
// It expects a scratch root on the Windows side that contains at least one
// regular file; it creates and removes a "smoke" subdirectory inside it.
// If a file named smoke.mp4 exists in the root, Probe/Run are exercised too.
//
//	go run ./cmd/fsd-smoke -addr host.docker.internal:17700 -secret ... -root C:/temp/fsd-root
package main

import (
	"bytes"
	"context"
	"crypto/md5"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/stashapp/stash/pkg/hash/oshash"
	pb "github.com/stashapp/stash/pkg/remotefs/stashfsv1"
)

var failures int

func check(ok bool, format string, args ...any) {
	if ok {
		fmt.Printf("  PASS  "+format+"\n", args...)
		return
	}
	failures++
	fmt.Printf("  FAIL  "+format+"\n", args...)
}

func codeOf(err error) codes.Code { return status.Code(err) }

func must[T any](v T, err error) T {
	if err != nil {
		fmt.Printf("  FATAL %v\n", err)
		os.Exit(2)
	}
	return v
}

func readAll(ctx context.Context, fs pb.FileSystemClient, path string) ([]byte, error) {
	st, err := fs.ReadFile(ctx, &pb.ReadFileRequest{Path: path})
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	for {
		c, err := st.Recv()
		if errors.Is(err, io.EOF) {
			return buf.Bytes(), nil
		}
		if err != nil {
			return nil, err
		}
		buf.Write(c.Data)
	}
}

func runTool(ctx context.Context, m pb.MediaClient, req *pb.RunRequest) ([]byte, *pb.RunStatus, int, error) {
	st, err := m.Run(ctx, req)
	if err != nil {
		return nil, nil, 0, err
	}
	var out bytes.Buffer
	var final *pb.RunStatus
	statuses := 0
	for {
		msg, err := st.Recv()
		if errors.Is(err, io.EOF) {
			return out.Bytes(), final, statuses, nil
		}
		if err != nil {
			return out.Bytes(), final, statuses, err
		}
		switch p := msg.Payload.(type) {
		case *pb.RunOutput_Stdout:
			out.Write(p.Stdout)
		case *pb.RunOutput_Status:
			final = p.Status
			statuses++
		}
	}
}

func main() {
	addr := flag.String("addr", "host.docker.internal:7700", "stash-fsd gRPC address")
	secret := flag.String("secret", os.Getenv("STASH_FSD_SECRET"), "shared secret")
	root := flag.String("root", "", "scratch root on the server (forward-slash Windows path)")
	outside := flag.String("outside", "C:/Windows/win.ini", "a path that must be outside the roots")
	flag.Parse()
	if *root == "" || *secret == "" {
		fmt.Fprintln(os.Stderr, "-root and -secret are required")
		os.Exit(2)
	}
	r := strings.TrimRight(*root, "/")

	conn := must(grpc.NewClient(*addr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(16<<20)),
	))
	defer conn.Close()
	fs := pb.NewFileSystemClient(conn)
	hasher := pb.NewHasherClient(conn)
	media := pb.NewMediaClient(conn)

	base := context.Background()
	ctx, cancel := context.WithTimeout(metadata.AppendToOutgoingContext(base, "authorization", "Bearer "+*secret), 5*time.Minute)
	defer cancel()

	fmt.Println("== auth")
	_, err := fs.Stat(base, &pb.PathRequest{Path: r})
	check(codeOf(err) == codes.Unauthenticated, "no token -> %v", codeOf(err))
	bad := metadata.AppendToOutgoingContext(base, "authorization", "Bearer wrong")
	_, err = fs.Stat(bad, &pb.PathRequest{Path: r})
	check(codeOf(err) == codes.Unauthenticated, "wrong token -> %v", codeOf(err))

	fmt.Println("== capabilities")
	caps := must(media.Capabilities(ctx, &pb.Empty{}))
	fmt.Printf("  service=%s\n  ffmpeg=%q\n  ffprobe=%q\n  hw=%v\n  roots=%v\n", caps.ServiceVersion, caps.FfmpegVersion, caps.FfprobeVersion, caps.HardwareCodecs, caps.Roots)
	check(caps.ServiceVersion != "" && len(caps.Roots) > 0, "capabilities populated")

	fmt.Println("== stat / errors")
	fi := must(fs.Stat(ctx, &pb.PathRequest{Path: r}))
	check(fi.IsDir && fi.Mode&(1<<31) != 0, "root is dir, mode=%#x path=%s", fi.Mode, fi.Path)
	_, err = fs.Stat(ctx, &pb.PathRequest{Path: *outside})
	check(codeOf(err) == codes.PermissionDenied, "outside roots -> %v", codeOf(err))
	_, err = fs.Stat(ctx, &pb.PathRequest{Path: r + "/../../../Windows"})
	check(codeOf(err) == codes.PermissionDenied, "dot-dot escape -> %v", codeOf(err))
	_, err = fs.Stat(ctx, &pb.PathRequest{Path: r + "/does-not-exist"})
	check(codeOf(err) == codes.NotFound, "missing -> %v", codeOf(err))
	bs := strings.ReplaceAll(r, "/", `\`)
	fi2, err := fs.Stat(ctx, &pb.PathRequest{Path: bs})
	check(err == nil && fi2.IsDir, "backslash form accepted (%v)", err)

	fmt.Println("== readdir")
	rd := must(fs.ReadDir(ctx, &pb.PathRequest{Path: r}))
	names := []string{}
	for _, e := range rd.Entries {
		names = append(names, e.Name)
	}
	check(sort.StringsAreSorted(names) && len(names) > 0, "sorted entries: %v", names)

	fmt.Println("== mutations")
	smoke := r + "/smoke"
	_, _ = fs.RemoveAll(ctx, &pb.PathRequest{Path: smoke})
	_, err = fs.Mkdir(ctx, &pb.MkdirRequest{Path: smoke + "/a/b", All: true})
	check(err == nil, "mkdir all (%v)", err)
	_, err = fs.Mkdir(ctx, &pb.MkdirRequest{Path: smoke + "/a"})
	check(codeOf(err) == codes.AlreadyExists, "mkdir existing -> %v", codeOf(err))
	_, err = fs.Remove(ctx, &pb.PathRequest{Path: smoke + "/a"})
	check(codeOf(err) == codes.FailedPrecondition, "remove non-empty dir -> %v", codeOf(err))

	// pick a regular file from the root to work with
	var sample *pb.FileInfo
	for _, e := range rd.Entries {
		if !e.IsDir && !e.IsSymlink && e.Size > 8 && (sample == nil || e.Size > sample.Size) {
			sample = e
		}
	}
	if sample == nil {
		fmt.Println("  FATAL no regular file > 8 bytes in root")
		os.Exit(2)
	}

	fmt.Println("== reads + fingerprint parity (Go pkg/hash vs stash-fsd)")
	data := must(readAll(ctx, fs, sample.Path))
	check(int64(len(data)) == sample.Size, "ReadFile %s: %d bytes", sample.Name, len(data))
	goMD5 := fmt.Sprintf("%x", md5.Sum(data))
	goOS := must(oshash.FromReader(bytes.NewReader(data), int64(len(data))))
	fp := must(hasher.Fingerprint(ctx, &pb.FingerprintRequest{Path: sample.Path}))
	check(fp.Md5 == goMD5, "md5 %s == %s", fp.Md5, goMD5)
	check(fp.Oshash == goOS, "oshash %s == %s", fp.Oshash, goOS)
	check(fp.Size == sample.Size, "size %d", fp.Size)
	ra := must(fs.ReadAt(ctx, &pb.ReadAtRequest{Path: sample.Path, Offset: 3, Length: 5}))
	check(bytes.Equal(ra.Data, data[3:8]) && !ra.Eof, "ReadAt(3,5)")
	ra = must(fs.ReadAt(ctx, &pb.ReadAtRequest{Path: sample.Path, Offset: sample.Size - 2, Length: 100}))
	check(len(ra.Data) == 2 && ra.Eof, "ReadAt at EOF returns 2 bytes, eof=%v", ra.Eof)
	big := must(fs.ReadAt(ctx, &pb.ReadAtRequest{Path: sample.Path, Offset: 0, Length: 8 << 20}))
	check(len(big.Data) == min(len(data), 8<<20), "ReadAt 8 MiB request -> %d bytes", len(big.Data))
	_, err = fs.ReadAt(ctx, &pb.ReadAtRequest{Path: sample.Path, Length: 8<<20 + 1})
	check(codeOf(err) == codes.InvalidArgument, "ReadAt > 8 MiB -> %v", codeOf(err))
	rf, err := fs.ReadFile(ctx, &pb.ReadFileRequest{Path: sample.Path, MaxSize: 1})
	if err == nil {
		_, err = rf.Recv()
	}
	check(codeOf(err) == codes.FailedPrecondition, "ReadFile max_size -> %v", codeOf(err))
	for _, e := range rd.Entries {
		if strings.HasSuffix(e.Name, ".zip") {
			zfp, err := hasher.Fingerprint(ctx, &pb.FingerprintRequest{Path: e.Path, ZipEntry: "sub/f102400.bin"})
			if err == nil {
				fmt.Printf("  info  zip entry fingerprint md5=%s oshash=%s size=%d\n", zfp.Md5, zfp.Oshash, zfp.Size)
			} else {
				fmt.Printf("  info  zip entry fingerprint: %v\n", err)
			}
		}
	}
	cs := must(fs.IsPathCaseSensitive(ctx, &pb.PathRequest{Path: sample.Path}))
	check(!cs.Value, "NTFS is case-insensitive (value=%v)", cs.Value)
	ev := must(fs.EvalSymlinks(ctx, &pb.PathRequest{Path: sample.Path}))
	check(strings.EqualFold(ev.Path, sample.Path), "EvalSymlinks(%s) = %s", sample.Path, ev.Path)

	fmt.Println("== walk")
	collectWalk := func(batch uint32) ([]*pb.FileInfo, int, int) {
		ws := must(fs.Walk(ctx, &pb.WalkRequest{Root: r, BatchSize: batch}))
		var all []*pb.FileInfo
		batches, errs := 0, 0
		for {
			b, err := ws.Recv()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				fmt.Printf("  FAIL  walk: %v\n", err)
				failures++
				break
			}
			batches++
			errs += len(b.Errors)
			all = append(all, b.Entries...)
		}
		return all, batches, errs
	}
	all, batches, errs := collectWalk(0)
	check(len(all) > 0 && strings.EqualFold(all[0].Path, r), "root first (%s), %d entries in %d batches, %d errors", all[0].Path, len(all), batches, errs)
	all2, batches2, _ := collectWalk(2)
	check(len(all2) == len(all) && batches2 >= (len(all)+1)/2, "batch_size=2: %d entries in %d batches", len(all2), batches2)

	fmt.Println("== rename / remove / trash")
	_, err = fs.Rename(ctx, &pb.RenameRequest{OldPath: smoke + "/a/b", NewPath: smoke + "/c"})
	check(err == nil, "rename dir (%v)", err)
	_, err = fs.Mkdir(ctx, &pb.MkdirRequest{Path: smoke + "/d"})
	check(err == nil, "mkdir d (%v)", err)
	_, err = fs.Rename(ctx, &pb.RenameRequest{OldPath: smoke + "/c", NewPath: smoke + "/d"})
	check(codeOf(err) == codes.AlreadyExists, "rename onto existing -> %v", codeOf(err))
	_, err = fs.Rename(ctx, &pb.RenameRequest{OldPath: smoke + "/c", NewPath: smoke + "/C"})
	check(err == nil, "case-only rename (%v)", err)
	_, err = fs.Rename(ctx, &pb.RenameRequest{OldPath: smoke + "/C", NewPath: *outside + ".x"})
	check(codeOf(err) == codes.PermissionDenied, "rename outside roots -> %v", codeOf(err))
	_, err = fs.Remove(ctx, &pb.PathRequest{Path: smoke + "/C"})
	check(err == nil, "remove empty dir (%v)", err)
	_, err = fs.Remove(ctx, &pb.PathRequest{Path: r})
	check(codeOf(err) == codes.PermissionDenied, "remove root -> %v", codeOf(err))
	trash := smoke + "/.trash"
	t1, err := fs.MoveToTrash(ctx, &pb.TrashRequest{Path: smoke + "/d", TrashDir: trash})
	// returned paths are canonical (upper-case drive), the input may not be
	check(err == nil && strings.EqualFold(t1.GetPath(), trash+"/d"), "trash -> %v (%v)", t1.GetPath(), err)
	_, _ = fs.Mkdir(ctx, &pb.MkdirRequest{Path: smoke + "/d"})
	t2, err := fs.MoveToTrash(ctx, &pb.TrashRequest{Path: smoke + "/d", TrashDir: trash})
	check(err == nil && strings.HasPrefix(strings.ToLower(t2.GetPath()), strings.ToLower(trash+"/d_")) && len(t2.GetPath()) == len(trash+"/d_20060102-150405"), "trash collision -> %v (%v)", t2.GetPath(), err)
	if len(r) > 1 && r[1] == ':' {
		check(fi.Path[0] == strings.ToUpper(r[:1])[0], "returned drive letter is upper-case (%s)", fi.Path[:2])
	}

	fmt.Println("== media")
	video := r + "/smoke.mp4"
	if _, err := fs.Stat(ctx, &pb.PathRequest{Path: video}); err != nil || caps.FfprobeVersion == "" {
		fmt.Println("  SKIP  no smoke.mp4 in root or no ffprobe")
	} else {
		pr, err := media.Probe(ctx, &pb.ProbeRequest{Path: video})
		check(err == nil && bytes.Contains(pr.GetJson(), []byte(`"streams"`)) && pr.GetSize() > 0, "probe: %d bytes json, size=%d (%v)", len(pr.GetJson()), pr.GetSize(), err)
		pr2, err := media.Probe(ctx, &pb.ProbeRequest{Path: video, ExtraArgs: []string{"-count_frames"}})
		check(err == nil && bytes.Contains(pr2.GetJson(), []byte("nb_read_frames")), "probe -count_frames (%v)", err)
		_, err = media.Probe(ctx, &pb.ProbeRequest{Path: *outside})
		check(codeOf(err) == codes.PermissionDenied, "probe outside -> %v", codeOf(err))

		jpg, st, n, err := runTool(ctx, media, &pb.RunRequest{
			Tool:     pb.Tool_TOOL_FFMPEG,
			Args:     []string{"-hide_banner", "-ss", "1", "-i", "{input:0}", "-frames:v", "1", "-f", "image2pipe", "-c:v", "mjpeg", "pipe:1"},
			Inputs:   []string{video},
			Priority: pb.Priority_PRIORITY_INTERACTIVE,
		})
		check(err == nil && n == 1 && st.ExitCode == 0 && len(jpg) > 100 && jpg[0] == 0xFF && jpg[1] == 0xD8, "screenshot to stdout: %d bytes, statuses=%d, status=%v (%v)", len(jpg), n, st, err)

		_, st, _, err = runTool(ctx, media, &pb.RunRequest{
			Tool:  pb.Tool_TOOL_FFPROBE,
			Args:  []string{"-v", "error", "-show_entries", "stream=codec_name", "-of", "csv=p=0", "-i", "pipe:0"},
			Stdin: jpg,
		})
		check(err == nil && st.ExitCode == 0, "ffprobe with stdin (%v, %v)", st, err)

		ws := "smoke-ws_1"
		_, st, _, err = runTool(ctx, media, &pb.RunRequest{
			Tool:      pb.Tool_TOOL_FFMPEG,
			Args:      []string{"-hide_banner", "-y", "-i", "{input:0}", "-vf", "fps=1,scale=160:-2", "-frames:v", "2", "{workspace}/thumb%02d.jpg"},
			Inputs:    []string{video},
			Workspace: ws,
		})
		check(err == nil && st.ExitCode == 0, "ffmpeg into workspace (%v)", err)
		lw := must(media.ListWorkspace(ctx, &pb.WorkspaceRequest{Workspace: ws}))
		check(len(lw.Entries) == 2, "workspace has %d files", len(lw.Entries))
		if len(lw.Entries) > 0 {
			fs2, err := media.FetchWorkspaceFile(ctx, &pb.WorkspaceFileRequest{Workspace: ws, Name: lw.Entries[0].Name})
			var got bytes.Buffer
			for err == nil {
				var c *pb.DataChunk
				c, err = fs2.Recv()
				if c != nil {
					got.Write(c.Data)
				}
			}
			check(errors.Is(err, io.EOF) && int64(got.Len()) == lw.Entries[0].Size, "fetch %s: %d bytes", lw.Entries[0].Name, got.Len())
		}
		trav, err := media.FetchWorkspaceFile(ctx, &pb.WorkspaceFileRequest{Workspace: ws, Name: "../../x"})
		if err == nil {
			_, err = trav.Recv()
		}
		check(codeOf(err) == codes.InvalidArgument, "workspace traversal -> %v", codeOf(err))
		_, err = media.DeleteWorkspace(ctx, &pb.WorkspaceRequest{Workspace: ws})
		check(err == nil, "delete workspace (%v)", err)
		_, err = media.ListWorkspace(ctx, &pb.WorkspaceRequest{Workspace: "bad/id"})
		check(codeOf(err) == codes.InvalidArgument, "bad workspace id -> %v", codeOf(err))

		start := time.Now()
		_, st, _, err = runTool(ctx, media, &pb.RunRequest{
			Tool:           pb.Tool_TOOL_FFMPEG,
			Args:           []string{"-hide_banner", "-re", "-f", "lavfi", "-i", "testsrc=size=64x64:rate=5", "-f", "null", "-"},
			TimeoutSeconds: 2,
		})
		check(err == nil && st.TimedOut && time.Since(start) < 15*time.Second, "timeout kills ffmpeg after %v (timed_out=%v)", time.Since(start).Round(time.Millisecond), st.GetTimedOut())

		// Cancel a long-running Run that writes no stdout; the server must kill
		// ffmpeg (check the server log for "client went away; process killed").
		cctx, ccancel := context.WithTimeout(ctx, 1500*time.Millisecond)
		start = time.Now()
		_, _, _, err = runTool(cctx, media, &pb.RunRequest{
			Tool:           pb.Tool_TOOL_FFMPEG,
			Args:           []string{"-hide_banner", "-re", "-f", "lavfi", "-i", "testsrc=size=64x64:rate=5", "-f", "null", "-"},
			TimeoutSeconds: 120,
		})
		ccancel()
		check(codeOf(err) == codes.DeadlineExceeded || codeOf(err) == codes.Canceled, "client cancel after %v -> %v", time.Since(start).Round(time.Millisecond), codeOf(err))
		time.Sleep(time.Second)

		_, _, _, err = runTool(ctx, media, &pb.RunRequest{Tool: pb.Tool_TOOL_FFMPEG, Args: []string{"-i", "{input:0}"}, Inputs: []string{*outside}})
		check(codeOf(err) == codes.PermissionDenied, "run with input outside roots -> %v", codeOf(err))
	}

	_, err = fs.RemoveAll(ctx, &pb.PathRequest{Path: smoke})
	check(err == nil, "cleanup removeall (%v)", err)

	if failures > 0 {
		fmt.Printf("\n%d FAILURES\n", failures)
		os.Exit(1)
	}
	fmt.Println("\nALL PASSED")
}
