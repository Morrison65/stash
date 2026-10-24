package main

import (
	"slices"
	"testing"
)

func TestRemoteInputs(t *testing.T) {
	isRemote := func(p string) bool { return p == "/data/x.mp4" || len(p) > 2 && p[1] == ':' }

	args := []string{"-v", "quiet", "-print_format", "json", "-show_chapters", "U:/Lib/a b.mp4"}
	out, inputs := probeInputs(args, isRemote)
	if !slices.Equal(out, []string{"-v", "quiet", "-print_format", "json", "-show_chapters", "{input:0}"}) || !slices.Equal(inputs, []string{"U:/Lib/a b.mp4"}) {
		t.Errorf("probeInputs = %v %v", out, inputs)
	}
	if !hasRemoteInput("ffprobe", args, isRemote) {
		t.Error("ffprobe with a library path not remote")
	}
	if hasRemoteInput("ffprobe", []string{"-version"}, isRemote) {
		t.Error("ffprobe -version remote")
	}
	if !hasRemoteInput("ffmpeg", []string{"-i", "U:/a.mp4", "-f", "null", "-"}, isRemote) {
		t.Error("ffmpeg with a library input not remote")
	}
	if hasRemoteInput("ffmpeg", []string{"-i", "/generated/a.mp4", "-y", "U:/out.mp4"}, isRemote) {
		t.Error("ffmpeg writing to a library path counted as remote input")
	}
	if !hasRemoteInput("ffmpeg", []string{"-i", "/data/x.mp4", "out.jpg"}, isRemote) {
		t.Error("remote prefix not honoured")
	}
}
