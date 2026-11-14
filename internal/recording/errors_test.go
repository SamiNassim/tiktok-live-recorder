package recording

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"tiktok-live-recorder/internal/fault"
	"tiktok-live-recorder/internal/ffmpeg"
	"tiktok-live-recorder/internal/state"
)

func TestFailedFFmpegProcessingRetainsStderrAndRawFile(t *testing.T) {
	path, err := ffmpeg.Find("")
	if err != nil {
		t.Skip("FFmpeg is not installed")
	}
	source := filepath.Join(t.TempDir(), "invalid.flv")
	raw := []byte("synthetic invalid media")
	if err := os.WriteFile(source, raw, 0600); err != nil {
		t.Fatal(err)
	}
	cfg := state.Defaults()
	cfg.FFmpegPath, cfg.DeleteRawAfterProcessing = path, true
	_, err = Process(context.Background(), source, cfg)
	if err == nil || !fault.RequiresAction(err) || !strings.Contains(fault.Details(err), "FFmpeg output:") || !strings.Contains(err.Error(), "raw FLV kept") {
		t.Fatal("processing failure lost its diagnostics or raw-preservation message", err)
	}
	got, err := os.ReadFile(source)
	if err != nil || !bytes.Equal(got, raw) {
		t.Fatal("logging changes affected raw preservation", err)
	}
}

func TestProcessingMessagesDistinguishFailureStageAndCause(t *testing.T) {
	for _, operation := range []string{"create the processed copy", "verify the processed copy"} {
		err := processingFailure(nil, errors.New("synthetic exit failure"), "No space left on device", operation)
		if !fault.RequiresAction(err) || !strings.Contains(err.Error(), operation) || !strings.Contains(err.Error(), "disk is full") || !strings.Contains(fault.Details(err), "No space left on device") {
			t.Fatal("processing stage or cause was lost", err)
		}
	}
	err := processingFailure(context.DeadlineExceeded, errors.New("process killed"), "partial output", "verify the processed copy")
	if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "30-minute") || !strings.Contains(err.Error(), "raw FLV kept") {
		t.Fatal("processing deadline was not explained", err)
	}
	err = processingFailure(nil, errors.New("synthetic exit failure"), "", "create the processed copy")
	if strings.Contains(err.Error(), "FFmpeg's error output") || !strings.Contains(fault.Details(err), "synthetic exit failure") {
		t.Fatal("processing invented FFmpeg output or lost the underlying failure", err)
	}
}

func TestProcessingReportsCodecChangeEvenWhenFFmpegExitsWithError(t *testing.T) {
	path, err := ffmpeg.Find("")
	if err != nil {
		t.Skip("FFmpeg is not installed")
	}
	raw := []byte{'F', 'L', 'V', 1, 1, 0, 0, 0, 9, 0, 0, 0, 0}
	raw = appendFLVTag(raw, 9, 0, []byte{0x17, 0, 0, 0, 0, 1, 66, 0, 30, 0xff, 0xe1, 0, 1, 0x67, 1, 0, 1, 0x68})
	raw = appendFLVTag(raw, 9, 1000, []byte{0x1c, 0, 0, 0, 0})
	source := filepath.Join(t.TempDir(), "changing.flv")
	if err := os.WriteFile(source, raw, 0600); err != nil {
		t.Fatal(err)
	}
	cfg := state.Defaults()
	cfg.FFmpegPath = path
	output, err := Process(context.Background(), source, cfg)
	details := fault.Details(err)
	if output.Path != "" || !fault.RequiresAction(err) || !strings.Contains(details, "FLV video tag at byte 46: FLV video codec changed from h264 to hevc") {
		t.Fatalf("lost the FLV input failure: %s", details)
	}
	if !strings.Contains(details, "FFmpeg output:\n") {
		t.Fatalf("lost FFmpeg stderr: %s", details)
	}
	if got, err := os.ReadFile(source); err != nil || !bytes.Equal(got, raw) {
		t.Fatal("failed processing changed the original FLV", err)
	}
}

func TestRecordingFolderFailureIsActionable(t *testing.T) {
	parent := filepath.Join(t.TempDir(), "existing-file")
	if err := os.WriteFile(parent, []byte("keep this file"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg := state.Defaults()
	cfg.RecordingPath = parent
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, err := start(ctx, cancel, state.Streamer{ID: "demo_creator"}, cfg, io.NopCloser(strings.NewReader("raw video")))
	if err == nil || !fault.RequiresAction(err) || !strings.Contains(err.Error(), "recording folder") {
		t.Fatal("local recording failure was not actionable", err)
	}
}
