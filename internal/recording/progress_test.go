package recording

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"tiktok-live-recorder/internal/ffmpeg"
	"tiktok-live-recorder/internal/state"
)

func TestProgressParsesChunkedRecordsAndRejectsInvalidValues(t *testing.T) {
	var got []Progress
	w := &progressWriter{value: Progress{Phase: "Encoding MP4"}, report: func(p Progress) { got = append(got, p) }}
	for _, b := range []byte("out_time_us=1500000\nspeed=1.25x\nprogress=continue\r\nout_time_us=-1\nspeed=NaNx\nprogress=continue\nout_time_us=9223372036854775807\nspeed=Infx\nprogress=end\n") {
		if n, err := w.Write([]byte{b}); n != 1 || err != nil {
			t.Fatal(n, err)
		}
	}
	if len(got) != 3 {
		t.Fatal("missing progress records", got)
	}
	for _, p := range got {
		if p.Phase != "Encoding MP4" || p.MediaTime != 1500*time.Millisecond || p.Speed != 1.25 {
			t.Fatal("invalid progress value was accepted", p)
		}
	}
	if _, err := w.Write([]byte(strings.Repeat("x", 10000) + "\nout_time_us=3000000\nprogress=end\n")); err != nil || len(w.line) > 4096 || got[len(got)-1].MediaTime != 3*time.Second {
		t.Fatal("oversized progress input was not bounded", err)
	}
}

func TestConversionProgressAndReconversionPreservePreviousFiles(t *testing.T) {
	path, err := ffmpeg.Find("")
	if err != nil {
		t.Skip("FFmpeg is not installed")
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	source := filepath.Join(t.TempDir(), "source.flv")
	args := []string{"-hide_banner", "-loglevel", "error", "-f", "lavfi", "-i", "testsrc2=size=64x48:rate=5", "-t", "1", "-c:v", "libx264", "-threads:v", "1", "-preset", "ultrafast", "-bf", "0", "-flvflags", "no_duration_filesize", source}
	if out, err := exec.CommandContext(ctx, path, args...).CombinedOutput(); err != nil {
		if bytes.Contains(out, []byte("Unknown encoder")) {
			t.Skip("synthetic fixture needs libx264")
		}
		t.Fatalf("generate fixture: %v: %s", err, out)
	}
	raw, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	cfg := state.Defaults()
	cfg.FFmpegPath = path
	var updates []Progress
	first, err := ProcessWithProgress(ctx, source, cfg, func(p Progress) { updates = append(updates, p) })
	if err != nil {
		t.Fatal(err)
	}
	phases := make(map[string]bool)
	mediaTime := time.Duration(0)
	for _, p := range updates {
		phases[p.Phase] = true
		mediaTime = max(mediaTime, p.MediaTime)
	}
	for _, phase := range []string{"Creating MP4", "Verifying MP4", "Saving MP4"} {
		if !phases[phase] {
			t.Fatal("conversion phase was not reported", phase, updates)
		}
	}
	if mediaTime <= 0 {
		t.Fatal("FFmpeg's media clock was not reported")
	}
	previous, err := os.ReadFile(first.Path)
	if err != nil {
		t.Fatal(err)
	}
	second, err := ProcessWithProgress(ctx, source, cfg, nil)
	if err != nil || second.Path == first.Path || !strings.HasSuffix(second.Path, ".processed-2.mp4") {
		t.Fatal("reconversion did not choose a separate output", second, err)
	}
	canceled, stop := context.WithCancel(ctx)
	stop()
	if _, err := ProcessWithProgress(canceled, source, cfg, nil); !errors.Is(err, context.Canceled) {
		t.Fatal("cancellation was not honored", err)
	}
	if got, err := os.ReadFile(first.Path); err != nil || !bytes.Equal(got, previous) {
		t.Fatal("reconversion or cancellation changed the previous copy", err)
	}
	if got, err := os.ReadFile(source); err != nil || !bytes.Equal(got, raw) {
		t.Fatal("reconversion changed the original", err)
	}
}
