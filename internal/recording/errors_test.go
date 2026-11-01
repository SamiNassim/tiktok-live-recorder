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
