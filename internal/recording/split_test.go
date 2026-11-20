package recording

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"tiktok-live-recorder/internal/fault"
	"tiktok-live-recorder/internal/ffmpeg"
	"tiktok-live-recorder/internal/state"
)

func splitFixture(enhanced, audioOnly bool) (raw, prefix []byte) {
	raw = []byte{'F', 'L', 'V', 1, 5, 0, 0, 0, 9, 0, 0, 0, 0}
	if audioOnly {
		raw[4] = 4
	}
	raw = appendFLVTag(raw, 18, 0, []byte("synthetic metadata"))
	raw = appendFLVTag(raw, 8, 0, []byte{0xaf, 0, 0x12, 0x10})
	if !audioOnly {
		config := []byte{0x17, 0, 0, 0, 0, 1}
		if enhanced {
			config = []byte{0x90, 'h', 'v', 'c', '1', 1}
		}
		raw = appendFLVTag(raw, 9, 0, config)
	}
	prefix = bytes.Clone(raw)
	for _, stamp := range []uint32{1000, 31000, 61000, 91000, 121000} {
		if audioOnly {
			raw = appendFLVTag(raw, 8, stamp, []byte{0xaf, 1, 1, 2, 3})
			continue
		}
		key := stamp == 1000 || stamp == 61000 || stamp == 121000
		frame := byte(0x27)
		if key {
			frame = 0x17
		}
		payload := []byte{frame, 1, 0, 0, 0, 1, 2, 3}
		if enhanced {
			frame = 0xa3
			if key {
				frame = 0x93
			}
			payload = []byte{frame, 'h', 'v', 'c', '1', 1, 2, 3}
		}
		raw = appendFLVTag(raw, 9, stamp, payload)
	}
	return raw, prefix
}

func TestSplitPreservesOriginalTagsTimestampsAndTail(t *testing.T) {
	for _, enhanced := range []bool{false, true} {
		for _, audioOnly := range []bool{false, true} {
			for _, sizeMode := range []bool{false, true} {
				raw, prefix := splitFixture(enhanced, audioOnly)
				raw = append(raw, 9, 0, 0, 30, 0) // A partial final tag must remain on disk.
				cfg := state.Defaults()
				cfg.RecordingPath = t.TempDir()
				cfg.SplitMinutes = 1
				if sizeMode {
					cfg.SplitMinutes = 0
					cfg.SplitBytes = int64(len(prefix) + 35)
				}
				ctx, cancel := context.WithCancel(t.Context())
				h, err := start(ctx, cancel, state.Streamer{ID: "demo_creator"}, cfg, io.NopCloser(bytes.NewReader(raw)))
				if err != nil {
					t.Fatal(err)
				}
				result := h.Result()
				if !errors.Is(result.Err, io.ErrUnexpectedEOF) || !result.TransportEnded {
					t.Fatal("partial tail misclassified", result.Err)
				}
				clips := h.TakeClips()
				if len(clips) < 2 || len(h.TakeClips()) != 0 {
					t.Fatal("completed clips missing or delivered twice", len(clips))
				}
				var reconstructed []byte
				for i, clip := range append(clips, Clip{Result: result}) {
					got, err := os.ReadFile(clip.Path)
					if err != nil || clip.Bytes != int64(len(got)) || !bytes.HasPrefix(got, prefix) {
						t.Fatal("clip initialization changed", err, clip)
					}
					if i == 0 {
						reconstructed = append(reconstructed, got...)
					} else {
						reconstructed = append(reconstructed, got[len(prefix):]...)
					}
					if !audioOnly && len(got) > len(prefix)+11 && got[len(prefix)] == 9 {
						payload := got[len(prefix)+11:]
						_, _, _, key := videoPacket(payload)
						if !key {
							t.Fatal("clip did not begin on a video keyframe")
						}
					}
				}
				if !bytes.Equal(reconstructed, raw) {
					t.Fatal("splitting rewrote, lost, or reordered source bytes")
				}
			}
		}
	}
}

func TestSplitFoldersAreIndependent(t *testing.T) {
	for _, streamerFolder := range []bool{false, true} {
		for _, splitFolder := range []bool{false, true} {
			cfg := state.Defaults()
			cfg.RecordingPath = t.TempDir()
			cfg.FolderPerStreamer = streamerFolder
			cfg.FolderPerSplitRecording = splitFolder
			cfg.SplitBytes = state.BytesPerMB
			path := recordingPath(state.Streamer{ID: "demo_creator", Name: "Demo Creator"}, cfg, time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC), "synthetic")
			dir := cfg.RecordingPath
			if streamerFolder {
				dir = filepath.Join(dir, "@demo_creator_Demo Creator")
			}
			if splitFolder {
				dir = filepath.Join(dir, "@demo_creator_2026-01-02_03-04-05_synthetic")
			}
			if filepath.Dir(path) != dir || filepath.Base(path) != "@demo_creator_2026-01-02_03-04-05_synthetic_part001.flv" {
				t.Fatal("wrong split folder", path)
			}
		}
	}
}

func TestSplitPausePreservesPartialPacketAndPublishesBeforeEOF(t *testing.T) {
	raw, _ := splitFixture(false, false)
	cfg := state.Defaults()
	cfg.RecordingPath = t.TempDir()
	cfg.SplitMinutes = 1
	reader, writer := io.Pipe()
	ctx, cancel := context.WithCancel(t.Context())
	h, err := start(ctx, cancel, state.Streamer{ID: "demo_creator"}, cfg, reader)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = writer.Write(raw); err != nil {
		t.Fatal(err)
	}
	var clips []Clip
	deadline := time.After(5 * time.Second)
	for len(clips) < 2 {
		clips = append(clips, h.TakeClips()...)
		time.Sleep(time.Millisecond)
		select {
		case <-deadline:
			t.Fatal("clips unavailable while recording")
		default:
		}
	}
	select {
	case <-h.Done():
		t.Fatal("splitting ended the media connection")
	default:
	}
	tail := []byte{8, 0, 0, 20, 0, 0}
	if _, err = writer.Write(tail); err != nil {
		t.Fatal(err)
	}
	h.Stop()
	select {
	case <-h.Done():
	case <-time.After(time.Second):
		t.Fatal("pause did not unblock split recorder")
	}
	result := h.Result()
	got, err := os.ReadFile(result.Path)
	if err != nil || result.Err != nil || !bytes.HasSuffix(got, tail) {
		t.Fatal("pause discarded partial raw bytes", result, err)
	}
	_ = writer.Close()
}

func TestSplitCreationFailureKeepsAlreadyReadPacket(t *testing.T) {
	raw, _ := splitFixture(false, false)
	cfg := state.Defaults()
	cfg.RecordingPath = t.TempDir()
	cfg.SplitMinutes = 1
	reader, writer := io.Pipe()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	h, err := start(ctx, cancel, state.Streamer{ID: "demo_creator"}, cfg, reader)
	if err != nil {
		t.Fatal(err)
	}
	next := filepath.Join(filepath.Dir(h.File()), filepath.Base(h.File())[:len(filepath.Base(h.File()))-len("001.flv")]+"002.flv")
	if err := os.Mkdir(next, 0700); err != nil {
		t.Fatal(err)
	}
	go func() { _, _ = writer.Write(raw); _ = writer.Close() }()
	result := h.Result()
	got, err := os.ReadFile(result.Path)
	// The boundary keyframe is the third source video packet; no packet already
	// consumed from the network may disappear when the next file cannot open.
	_, prefix := splitFixture(false, false)
	want := raw[:len(prefix)+3*(15+8)]
	if err != nil || !fault.RequiresAction(result.Err) || !bytes.Equal(got, want) {
		t.Fatal("rotation failure lost the pending keyframe", len(got), len(want), result.Err, err)
	}
}

func TestSplitClipsDecodeAndProcessIndependently(t *testing.T) {
	executable, err := ffmpeg.Find("")
	if err != nil {
		t.Skip("FFmpeg is not installed")
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	fixture := filepath.Join(t.TempDir(), "fixture.flv")
	args := []string{"-v", "error", "-f", "lavfi", "-i", "testsrc2=size=128x96:rate=10", "-f", "lavfi", "-i", "sine=sample_rate=48000", "-t", "4", "-c:v", "libx264", "-threads:v", "1", "-preset", "ultrafast", "-g", "10", "-sc_threshold", "0", "-c:a", "aac", "-flvflags", "no_duration_filesize", fixture}
	if out, err := exec.CommandContext(ctx, executable, args...).CombinedOutput(); err != nil {
		t.Fatalf("generate synthetic FLV: %v: %s", err, out)
	}
	raw, err := os.ReadFile(fixture)
	if err != nil {
		t.Fatal(err)
	}
	cfg := state.Defaults()
	cfg.RecordingPath = t.TempDir()
	cfg.SplitBytes = 1000
	recordCtx, recordCancel := context.WithCancel(ctx)
	h, err := start(recordCtx, recordCancel, state.Streamer{ID: "demo_creator"}, cfg, io.NopCloser(bytes.NewReader(raw)))
	if err != nil {
		t.Fatal(err)
	}
	result := h.Result()
	if result.Err != nil {
		t.Fatal(result.Err)
	}
	clips := append(h.TakeClips(), Clip{Result: result})
	if len(clips) < 3 {
		t.Fatal("fixture was not split", len(clips))
	}
	for _, clip := range clips {
		if out, err := exec.CommandContext(ctx, executable, "-v", "error", "-xerror", "-i", clip.Path, "-f", "null", "-").CombinedOutput(); err != nil {
			t.Fatalf("decode split clip: %v: %s", err, out)
		}
		cfg.FFmpegPath = executable
		if _, err := Process(ctx, clip.Path, cfg); err != nil {
			t.Fatal("process split clip", fault.Details(err))
		}
	}
}
