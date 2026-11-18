package recording

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"tiktok-live-recorder/internal/fault"
	"tiktok-live-recorder/internal/ffmpeg"
	"tiktok-live-recorder/internal/state"
)

func TestRepairToleratesDamageAndVerifiesSeparateCopies(t *testing.T) {
	path, err := ffmpeg.Find("")
	if err != nil {
		t.Skip("FFmpeg is not installed")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	fixture := filepath.Join(t.TempDir(), "fixture.flv")
	args := []string{"-hide_banner", "-loglevel", "error", "-f", "lavfi", "-i", "testsrc2=size=96x128:rate=20", "-f", "lavfi", "-i", "sine=sample_rate=48000", "-t", "4", "-c:v", "libx264", "-threads:v", "1", "-preset", "ultrafast", "-g", "20", "-bf", "0", "-c:a", "aac", fixture}
	if out, err := exec.CommandContext(ctx, path, args...).CombinedOutput(); err != nil {
		if bytes.Contains(out, []byte("Unknown encoder")) {
			t.Skip("synthetic repair fixture needs libx264")
		}
		t.Fatalf("create fixture: %v: %s", err, out)
	}
	clean, err := os.ReadFile(fixture)
	if err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"video", "audio", "timestamp reset"} {
		t.Run(kind, func(t *testing.T) {
			raw := bytes.Clone(clean)
			video, audio := 0, 0
			var previous uint32
			for offset := int(binary.BigEndian.Uint32(raw[5:9])) + 4; offset+11 < len(raw); {
				h := raw[offset : offset+11]
				size := int(h[1])<<16 | int(h[2])<<8 | int(h[3])
				payload := raw[offset+11 : offset+11+size]
				if h[0] == 9 && payload[1] == 1 {
					video++
					if kind == "video" && video == 35 {
						// Keep packet framing and NAL lengths intact while damaging
						// one coded picture. A later keyframe remains recoverable.
						for data := payload[5:]; len(data) > 4; {
							n := int(binary.BigEndian.Uint32(data[:4]))
							if data[4]&31 == 1 || data[4]&31 == 5 {
								clear(data[5 : 4+n])
								break
							}
							data = data[4+n:]
						}
					}
					if kind == "timestamp reset" && video == 60 {
						stamp := previous - 100
						h[4], h[5], h[6], h[7] = byte(stamp>>16), byte(stamp>>8), byte(stamp), byte(stamp>>24)
					}
					previous = flvTimestamp(h)
				}
				if h[0] == 8 && len(payload) > 2 && payload[1] == 1 {
					audio++
					if kind == "audio" && audio == 30 {
						for i := 2; i < len(payload); i++ {
							payload[i] = 0xff
						}
					}
				}
				offset += size + 15
			}
			source := filepath.Join(t.TempDir(), "source.flv")
			if err := os.WriteFile(source, raw, 0600); err != nil {
				t.Fatal(err)
			}
			cfg := state.Defaults()
			cfg.FFmpegPath, cfg.DeleteRawAfterProcessing = path, true
			if _, err := Process(ctx, source, cfg); err == nil {
				t.Fatal("synthetic damage did not exercise a strict conversion failure")
			}
			previousCopy := strings.TrimSuffix(source, ".flv") + ".processed.mp4"
			previousRepair := strings.TrimSuffix(source, ".flv") + ".repaired.mp4"
			for _, name := range []string{previousCopy, previousRepair} {
				if err := os.WriteFile(name, []byte("previous playback copy"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			cfg.FFmpegArgs = "-unsupported-output-option" // Repair uses its own profile.
			phases := make(map[string]bool)
			copy, err := RepairWithProgress(ctx, source, cfg, func(p Progress) { phases[p.Phase] = true })
			if err != nil {
				t.Fatal(fault.Details(err))
			}
			if !copy.Reencoded || !strings.HasSuffix(copy.Path, ".repaired-2.mp4") {
				t.Fatal("repair was not marked as re-encoded or overwrote a prior copy", copy)
			}
			if kind != "timestamp reset" && copy.Diagnostics == "" {
				t.Fatal("tolerated decoder errors were not retained for the log")
			}
			for _, phase := range []string{"Preparing repair", "Repairing MP4", "Verifying MP4", "Saving MP4"} {
				if !phases[phase] {
					t.Fatal("repair skipped a required phase", phase)
				}
			}
			out, err := exec.CommandContext(ctx, path, "-hide_banner", "-loglevel", "error", "-xerror", "-i", copy.Path, "-map", "0:v:0", "-fps_mode", "passthrough", "-enc_time_base:v", "demux", "-f", "framemd5", "-").CombinedOutput()
			if n := bytes.Count(out, []byte("\n0,")); err != nil || n < 60 || n > 80 {
				t.Fatalf("repair did not preserve useful, decodable video: frames=%d, err=%v: %s", n, err, out)
			}
			out, err = exec.CommandContext(ctx, path, "-hide_banner", "-loglevel", "error", "-xerror", "-i", copy.Path, "-map", "0:a:0", "-c:a", "pcm_s16le", "-ar", "48000", "-ac", "1", "-f", "s16le", "-").Output()
			if seconds := float64(len(out)) / (48000 * 2); err != nil || seconds < 3.8 || seconds > 4.3 {
				t.Fatal("repair lost audio coverage", seconds, err)
			}
			if got, err := os.ReadFile(source); err != nil || !bytes.Equal(got, raw) {
				t.Fatal("repair changed the original FLV", err)
			}
			for _, name := range []string{previousCopy, previousRepair} {
				if got, err := os.ReadFile(name); err != nil || string(got) != "previous playback copy" {
					t.Fatal("repair changed a previous playback copy", err)
				}
			}
		})
	}
}

func TestRepairFailureAndCancellationNeverPublishOrDeleteRaw(t *testing.T) {
	path, err := ffmpeg.Find("")
	if err != nil {
		t.Skip("FFmpeg is not installed")
	}
	for _, canceled := range []bool{false, true} {
		t.Run(map[bool]string{false: "unrecoverable", true: "canceled"}[canceled], func(t *testing.T) {
			source := filepath.Join(t.TempDir(), "source.flv")
			raw := []byte("synthetic invalid media")
			if err := os.WriteFile(source, raw, 0600); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			cfg := state.Defaults()
			cfg.FFmpegPath, cfg.DeleteRawAfterProcessing = path, true
			copy, err := RepairWithProgress(ctx, source, cfg, func(p Progress) {
				if canceled && p.Phase == "Repairing MP4" {
					cancel()
				}
			})
			if err == nil || copy.Path != "" || canceled && !errors.Is(err, context.Canceled) {
				t.Fatal("failed or canceled repair was accepted", copy, err)
			}
			if outputs, _ := filepath.Glob(filepath.Join(filepath.Dir(source), "*.mp4")); len(outputs) != 0 {
				t.Fatal("failed repair published an MP4", outputs)
			}
			if got, err := os.ReadFile(source); err != nil || !bytes.Equal(got, raw) {
				t.Fatal("failed repair changed the raw file", err)
			}
		})
	}
}
