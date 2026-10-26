package recording

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"testing"
	"time"

	"tiktok-live-recorder/internal/ffmpeg"
	"tiktok-live-recorder/internal/state"
)

func TestRawRecordingIsUnmodifiedAndNeedsNoFFmpeg(t *testing.T) {
	cfg := state.Defaults()
	cfg.RecordingPath = t.TempDir()
	cfg.FFmpegPath = filepath.Join(t.TempDir(), "not-installed")
	raw := append([]byte{'F', 'L', 'V', 1, 5, 0, 0, 0, 9, 0, 0, 0, 0}, bytes.Repeat([]byte{1, 2, 3, 4}, 10000)...)
	ctx, cancel := context.WithCancel(context.Background())
	h, err := start(ctx, cancel, state.Streamer{ID: "demo_creator"}, cfg, io.NopCloser(bytes.NewReader(raw)))
	if err != nil {
		t.Fatal(err)
	}
	r := h.Result()
	got, err := os.ReadFile(r.Path)
	if err != nil || r.Err != nil || !bytes.Equal(got, raw) || filepath.Ext(r.Path) != ".flv" || r.Bytes != int64(len(raw)) {
		t.Fatal("raw recording changed", r, err)
	}
}

func TestPauseClosesStalledMediaAndKeepsPartialFile(t *testing.T) {
	cfg := state.Defaults()
	cfg.RecordingPath = t.TempDir()
	reader, writer := io.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	h, err := start(ctx, cancel, state.Streamer{ID: "demo_creator"}, cfg, reader)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = writer.Write([]byte("preserve this partial stream")); err != nil {
		t.Fatal(err)
	}
	h.Stop()
	select {
	case <-h.Done():
	case <-time.After(time.Second):
		t.Fatal("pause did not unblock media")
	}
	r := h.Result()
	if r.Err != nil || r.Bytes == 0 {
		t.Fatal(r)
	}
	got, err := os.ReadFile(r.Path)
	if err != nil || string(got) != "preserve this partial stream" {
		t.Fatal(string(got), err)
	}
	writer.Close()
}

type interruptedBody struct {
	reader *bytes.Reader
	err    error
}

func (b interruptedBody) Read(p []byte) (int, error) {
	if b.reader.Len() == 0 {
		return 0, b.err
	}
	return b.reader.Read(p)
}
func (b interruptedBody) Close() error { return nil }

func TestEarlyHTTPClosureKeepsRawBytesAndClassifiesTransportEnd(t *testing.T) {
	for _, cause := range []error{io.ErrUnexpectedEOF, errors.New("read timeout")} {
		cfg := state.Defaults()
		cfg.RecordingPath = t.TempDir()
		raw := []byte("original video payload")
		ctx, cancel := context.WithCancel(context.Background())
		h, err := start(ctx, cancel, state.Streamer{ID: "demo_creator"}, cfg, interruptedBody{bytes.NewReader(raw), cause})
		if err != nil {
			t.Fatal(err)
		}
		r := h.Result()
		if r.TransportEnded != errors.Is(cause, io.ErrUnexpectedEOF) || !errors.Is(r.Err, cause) || r.Bytes != int64(len(raw)) {
			t.Fatal("transport closure was not classified correctly", r)
		}
		got, err := os.ReadFile(r.Path)
		if err != nil || !bytes.Equal(got, raw) {
			t.Fatal("raw bytes changed", err)
		}
	}
}

func TestOutputOptionsCannotReplaceInputOrCreateExtraOutput(t *testing.T) {
	for _, raw := range []string{`-i other.flv -c copy`, `-c copy alternate.mp4`, `ffmpeg -c copy`, `-c copy {output}`, `-filter_complex_script script.txt`, `-vf "movie=other.mp4"`, `-y`, `-c "unfinished`} {
		if _, err := OutputArgs(raw); err == nil {
			t.Fatal("accepted", raw)
		}
	}
	got, err := OutputArgs(`-map 0:v? -map 0:a? -c copy -metadata title="Processed copy" -movflags +faststart`)
	if err != nil || !slices.Contains(got, "title=Processed copy") {
		t.Fatal(got, err)
	}
}

func TestFailedProcessingNeverRemovesRaw(t *testing.T) {
	source := filepath.Join(t.TempDir(), "raw.flv")
	raw := []byte("original FLV")
	if err := os.WriteFile(source, raw, 0600); err != nil {
		t.Fatal(err)
	}
	cfg := state.Defaults()
	cfg.FFmpegPath = filepath.Join(t.TempDir(), "missing")
	if _, err := Process(context.Background(), source, cfg); err == nil {
		t.Fatal("processing unexpectedly succeeded")
	}
	got, err := os.ReadFile(source)
	if err != nil || !bytes.Equal(got, raw) {
		t.Fatal("raw file lost", err)
	}
}

func TestProcessingCreatesSeparateCopyAndPreservesRawHash(t *testing.T) {
	path, err := ffmpeg.Find("")
	if err != nil {
		t.Skip("FFmpeg is not installed; raw recording tests remain independent")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	source := filepath.Join(t.TempDir(), "raw.flv")
	cmd := exec.CommandContext(ctx, path, "-hide_banner", "-loglevel", "error", "-nostdin", "-n", "-f", "lavfi", "-i", "sine=frequency=440:sample_rate=8000", "-t", "0.3", "-c:a", "aac", "-output_ts_offset", "5400", "-flvflags", "no_duration_filesize", "-f", "flv", source)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("create synthetic FLV: %v: %s", err, out)
	}
	before, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	cfg := state.Defaults()
	cfg.FFmpegPath = path
	processed, err := Process(ctx, source, cfg)
	if err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	if sha256.Sum256(before) != sha256.Sum256(after) || processed == source || filepath.Ext(processed) != ".mkv" {
		t.Fatal("post-processing modified its raw input")
	}
	if info, err := os.Stat(processed); err != nil || info.Size() == 0 {
		t.Fatal("processed copy missing", err)
	}
	t.Run("playback timeline starts at zero", func(t *testing.T) {
		probe, err := exec.LookPath("ffprobe")
		if err != nil {
			t.Skip("FFprobe is not installed; raw preservation was still checked")
		}
		timing := func(name string) (float64, float64) {
			t.Helper()
			out, err := exec.CommandContext(ctx, probe, "-v", "error", "-show_entries", "format=start_time,duration", "-of", "json", name).Output()
			if err != nil {
				t.Fatal("probe playback timeline", err)
			}
			var result struct {
				Format struct {
					Start    string `json:"start_time"`
					Duration string `json:"duration"`
				} `json:"format"`
			}
			if err := json.Unmarshal(out, &result); err != nil {
				t.Fatal(err)
			}
			start, err := strconv.ParseFloat(result.Format.Start, 64)
			if err != nil {
				t.Fatal(err)
			}
			duration, err := strconv.ParseFloat(result.Format.Duration, 64)
			if err != nil {
				t.Fatal(err)
			}
			return start, duration
		}
		if start, duration := timing(source); start < 5000 || duration < 5000 {
			t.Fatal("synthetic FLV lacks the live timestamp offset", start, duration)
		}
		if start, duration := timing(processed); start < 0 || start > 0.1 || duration < 0.2 || duration > 1 {
			t.Fatal("MKV retained the inflated live timeline", start, duration)
		}
	})
}
