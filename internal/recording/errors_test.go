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
	if !strings.Contains(fault.Reason(err), "damaged or incomplete media data") {
		t.Fatal("invalid media was not explained clearly", err)
	}
	if outputs, _ := filepath.Glob(filepath.Join(filepath.Dir(source), "*.mp4")); len(outputs) != 0 {
		t.Fatal("failed conversion published an MP4", outputs)
	}
	got, err := os.ReadFile(source)
	if err != nil || !bytes.Equal(got, raw) {
		t.Fatal("logging changes affected raw preservation", err)
	}
}

func TestProcessingMessagesExplainDamagedMediaWithoutExposingDiagnostics(t *testing.T) {
	for _, stderr := range []string{
		"Invalid data found when processing input",
		"[h264] error while decoding MB 2 3, bytestream -5",
		"corrupt decoded frame in stream 0",
		"Packet corrupt (stream = 0, dts = 100)",
		"Invalid NAL unit size (0 > 12)",
	} {
		err := processingFailure(nil, errors.New("synthetic exit failure"), stderr, "verify the processed copy")
		if got := fault.Reason(err); got != "The recording contains damaged or incomplete media data; raw FLV kept" {
			t.Fatalf("unexpected media error: %q", got)
		}
		if !strings.Contains(fault.Details(err), stderr) {
			t.Fatal("media diagnostics were lost", err)
		}
		if !strings.Contains(fault.Details(err), "verify the processed copy") {
			t.Fatal("readable media error lost the diagnostic failure stage", err)
		}
	}
	// Local setup errors take precedence even when FFmpeg also mentions input.
	for _, test := range []struct{ stderr, want string }{
		{"No space left on device\nInvalid data found when processing input", "disk is full"},
		{"Permission denied\nInvalid data found when processing input", "access to the file was denied"},
		{"Unknown encoder 'synthetic'", "selected encoder is unavailable"},
		{"Unrecognized option 'synthetic'", "check the custom output options"},
	} {
		err := processingFailure(nil, errors.New("synthetic exit failure"), test.stderr, "create the processed copy")
		if got := fault.Reason(err); !strings.Contains(got, test.want) || strings.Contains(got, "damaged") {
			t.Fatal("setup failure was mislabeled as media damage", got)
		}
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
	if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "timed out") || !strings.Contains(err.Error(), "raw FLV kept") {
		t.Fatal("processing deadline was not explained", err)
	}
	err = processingFailure(nil, errors.New("synthetic exit failure"), "", "create the processed copy")
	if strings.Contains(err.Error(), "FFmpeg's error output") || !strings.Contains(fault.Details(err), "synthetic exit failure") {
		t.Fatal("processing invented FFmpeg output or lost the underlying failure", err)
	}
}

func TestAutomaticRepairRequiresMediaFailure(t *testing.T) {
	for _, tc := range []struct {
		stderr string
		want   bool
	}{
		{"Invalid data found when processing input", true},
		{"error while decoding MB 2 3", true},
		{"[h264] pps_id 42 out of range", true},
		{"Error decoding AAC frame header.", true},
		{"Error submitting packet to decoder", true},
		{"corrupt decoded frame in stream 0", true},
		{"Invalid NAL unit size", true},
		{"non monotonically increasing dts", true},
		{"not currently supported in container", true},
		{"No space left on device\nInvalid data found when processing input", false},
		{"Permission denied\nInvalid NAL unit size", false},
		{"Input/output error\nerror while decoding", false},
		{"Read-only file system\nPacket corrupt", false},
		{"Unknown encoder 'synthetic'\nPacket corrupt", false},
		{"No such filter: 'synthetic'\nPacket corrupt", false},
		{"Unable to parse option value\nError decoding AAC frame header.", false},
		{"Unrecognized option 'synthetic'", false},
		{"unclassified FFmpeg failure", false},
	} {
		t.Run(tc.stderr, func(t *testing.T) {
			err := processingFailure(nil, errors.New("synthetic exit failure"), tc.stderr, "create the processed copy")
			if got := CanRepair(err); got != tc.want {
				t.Fatalf("CanRepair() = %v, want %v", got, tc.want)
			}
		})
	}
	media := mediaInputFailure(errors.New("synthetic incomplete packet"))
	if !CanRepair(fault.Action("input parsing failed", media)) {
		t.Fatal("input parsing failure was not eligible for repair")
	}
	for _, cause := range []error{context.Canceled, context.DeadlineExceeded, &os.PathError{Op: "read", Path: "synthetic.flv", Err: os.ErrPermission}} {
		if CanRepair(errors.Join(media, cause)) || CanRepair(mediaInputFailure(cause)) {
			t.Fatal("operating-system or cancellation failure triggered repair", cause)
		}
	}
	// A later recovery failure takes precedence over its original media error.
	if CanRepair(&processingError{error: errors.Join(errors.New("encoder unavailable"), media)}) {
		t.Fatal("an earlier input error hid the failed recovery stage")
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
