package monitor

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
	"tiktok-live-recorder/internal/recording"
	"tiktok-live-recorder/internal/state"
)

func TestAutomaticRepairKeepsJobCancellationAndOriginalFiles(t *testing.T) {
	path, err := ffmpeg.Find("")
	if err != nil {
		t.Skip("FFmpeg is not installed")
	}
	for _, name := range []string{"recording completed", "manual retry", "repair failed", "canceled", "shutdown"} {
		t.Run(name, func(t *testing.T) {
			s, log := fixture(t)
			cfg := s.Config()
			cfg.PollingEnabled, cfg.PostProcess, cfg.DeleteRawAfterProcessing = false, true, true
			cfg.FFmpegPath, cfg.FFmpegArgs = path, "-c copy"
			if err := s.SaveConfig(cfg); err != nil {
				t.Fatal(err)
			}
			dir := t.TempDir()
			raw, previous, output := filepath.Join(dir, "source.flv"), filepath.Join(dir, "previous.mp4"), filepath.Join(dir, "source.repaired.mp4")
			const original = "synthetic invalid media"
			for _, file := range []string{raw, previous} {
				if err := os.WriteFile(file, []byte(original), 0600); err != nil {
					t.Fatal(err)
				}
			}
			_, normalErr := recording.Process(t.Context(), raw, cfg)
			if !recording.CanRepair(normalErr) {
				t.Fatal("fixture did not produce a repairable conversion failure", normalErr)
			}
			now := time.Now()
			row := state.Recording{ID: "completed", StreamerID: "demo_creator", Path: raw, ProcessedPath: previous, StartedAt: now.Add(-4 * time.Second), EndedAt: now}
			if err := s.Update(func(d *state.Data) error { d.Recordings = []state.Recording{row}; return nil }); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(t.Context())
			e := &Engine{store: s, log: log, ctx: ctx, cancel: cancel, processing: make(chan struct{}, 2)}
			t.Cleanup(e.Close)
			started, finish := make(chan state.Config, 1), make(chan struct{})
			e.process = func(_ context.Context, _ string, _ state.Config, report func(recording.Progress)) (recording.ProcessedCopy, error) {
				report(recording.Progress{Phase: "Creating MP4", MediaTime: 4 * time.Second})
				report(recording.Progress{Phase: "Verifying MP4", MediaTime: 2 * time.Second})
				return recording.ProcessedCopy{}, normalErr
			}
			const repairReason = "Synthetic repair failure; raw FLV kept"
			e.repair = func(ctx context.Context, _ string, cfg state.Config, report func(recording.Progress)) (recording.ProcessedCopy, error) {
				report(recording.Progress{Phase: "Repairing MP4", MediaTime: 2 * time.Second})
				started <- cfg
				select {
				case <-ctx.Done():
					return recording.ProcessedCopy{}, ctx.Err()
				case <-finish:
				}
				if name == "repair failed" {
					return recording.ProcessedCopy{}, fault.Action(repairReason, errors.New("synthetic repair diagnostic"))
				}
				if err := os.WriteFile(output, []byte("synthetic verified repair"), 0600); err != nil {
					return recording.ProcessedCopy{}, err
				}
				// Queue-level protection must not depend on the encoder's flag.
				return recording.ProcessedCopy{Path: output, Diagnostics: "synthetic tolerated input error"}, nil
			}
			if name == "recording completed" {
				e.processCopy(row.StreamerID, raw, cfg)
			} else if err := e.RetryProcessing(row.ID); err != nil {
				t.Fatal(err)
			}
			select {
			case observed := <-started:
				if observed.DeleteRawAfterProcessing || observed.FFmpegArgs != "" {
					t.Fatal("automatic repair inherited custom options or raw cleanup")
				}
			case <-time.After(10 * time.Second):
				t.Fatal("normal conversion did not switch to repair")
			}
			job := e.Snapshot().Processing[raw]
			if !job.Repair || !job.Cancelable || job.Pass != 3 || job.Passes != 4 || len(e.processing) != 1 {
				t.Fatal("automatic repair lost its mode, work estimate or shared queue slot", job)
			}
			if e.RetryProcessing(row.ID) == nil || e.RepairRecording(row.ID) == nil {
				t.Fatal("fallback allowed a duplicate conversion")
			}
			switch name {
			case "canceled":
				if err := e.CancelProcessing(row.ID); err != nil {
					t.Fatal(err)
				}
			case "shutdown":
				e.Close()
			default:
				close(finish)
			}
			waitFor(t, func() bool { return !e.IsProcessing(raw) })
			got := s.Data().Recordings[0]
			success := name == "recording completed" || name == "manual retry"
			if success && got.ProcessedPath != output || !success && got.ProcessedPath != previous || got.RawDeleted {
				t.Fatal("automatic repair replaced a valid previous copy or deleted raw media", got)
			}
			for _, file := range []string{raw, previous} {
				if data, err := os.ReadFile(file); err != nil || string(data) != original {
					t.Fatal("automatic repair changed an existing file", err)
				}
			}
			failures := e.TakeProcessingFailures()
			if name == "repair failed" {
				if len(failures) != 1 || !failures[0].Repair || failures[0].Message != "Normal conversion and automatic repair failed: "+repairReason || got.Error != failures[0].Message {
					t.Fatal("combined failure lost its final readable explanation", failures)
				}
			} else if len(failures) != 0 {
				t.Fatal("intermediate conversion failure or cancellation produced a toast", failures)
			}
			transition, combined := false, false
			for _, entry := range log.Entries() {
				if strings.Contains(entry.Message, "Trying tolerant repair") && entry.Level == "WARN" && strings.Contains(entry.Details, "FFmpeg output:") {
					transition = true
				}
				if strings.HasPrefix(entry.Message, "Recording repair failed:") && strings.Contains(entry.Details, "FFmpeg output:") && strings.Contains(entry.Details, "synthetic repair diagnostic") {
					combined = true
				}
			}
			if !transition || name == "repair failed" && !combined {
				t.Fatal("logs lost the fallback reason or one of the failures")
			}
			if s.Config().FFmpegArgs != cfg.FFmpegArgs || !s.Config().DeleteRawAfterProcessing {
				t.Fatal("automatic repair changed global preferences")
			}
		})
	}
}

func TestAutomaticRepairCreatesVerifiedPlaybackCopyOnlyForDamagedMedia(t *testing.T) {
	path, err := ffmpeg.Find("")
	if err != nil {
		t.Skip("FFmpeg is not installed")
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	fixturePath := filepath.Join(t.TempDir(), "fixture.flv")
	args := []string{"-hide_banner", "-loglevel", "error", "-f", "lavfi", "-i", "testsrc2=size=96x128:rate=20", "-f", "lavfi", "-i", "sine=sample_rate=48000", "-t", "4", "-c:v", "libx264", "-threads:v", "1", "-preset", "ultrafast", "-g", "20", "-bf", "0", "-c:a", "aac", fixturePath}
	if out, err := exec.CommandContext(ctx, path, args...).CombinedOutput(); err != nil {
		if bytes.Contains(out, []byte("Unknown encoder")) {
			t.Skip("synthetic fixture needs libx264")
		}
		t.Fatalf("create fixture: %v: %s", err, out)
	}
	clean, err := os.ReadFile(fixturePath)
	if err != nil {
		t.Fatal(err)
	}
	for _, damaged := range []bool{false, true} {
		t.Run(map[bool]string{false: "healthy stream copy", true: "damaged media repair"}[damaged], func(t *testing.T) {
			raw := bytes.Clone(clean)
			if damaged {
				frames := 0
				for offset := int(binary.BigEndian.Uint32(raw[5:9])) + 4; offset+11 < len(raw); {
					h := raw[offset : offset+11]
					size := int(h[1])<<16 | int(h[2])<<8 | int(h[3])
					payload := raw[offset+11 : offset+11+size]
					if h[0] == 9 && payload[1] == 1 {
						frames++
						if frames == 35 {
							// Damage a coded picture without changing its packet framing.
							for data := payload[5:]; len(data) > 4; {
								n := int(binary.BigEndian.Uint32(data[:4]))
								if data[4]&31 == 1 || data[4]&31 == 5 {
									clear(data[5 : 4+n])
									break
								}
								data = data[4+n:]
							}
						}
					}
					offset += size + 15
				}
			}
			s, log := fixture(t)
			cfg := s.Config()
			cfg.FFmpegPath, cfg.DeleteRawAfterProcessing, cfg.PollingEnabled = path, true, false
			if err := s.SaveConfig(cfg); err != nil {
				t.Fatal(err)
			}
			source := filepath.Join(t.TempDir(), "source.flv")
			if err := os.WriteFile(source, raw, 0600); err != nil {
				t.Fatal(err)
			}
			now := time.Now()
			if err := s.Update(func(d *state.Data) error {
				d.Recordings = []state.Recording{{ID: "completed", Path: source, StartedAt: now.Add(-4 * time.Second), EndedAt: now}}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			jobCtx, stop := context.WithCancel(ctx)
			e := &Engine{store: s, log: log, ctx: jobCtx, cancel: stop, processing: make(chan struct{}, 2), process: recording.ProcessWithProgress, repair: recording.RepairWithProgress}
			t.Cleanup(e.Close)
			if err := e.RetryProcessing("completed"); err != nil {
				t.Fatal(err)
			}
			waitFor(t, func() bool { return !e.IsProcessing(source) })
			got := s.Data().Recordings[0]
			suffix := ".processed.mp4"
			if damaged {
				suffix = ".repaired.mp4"
				if data, err := os.ReadFile(source); err != nil || !bytes.Equal(data, raw) {
					t.Fatal("automatic repair changed the raw media", err)
				}
			}
			if !strings.HasSuffix(got.ProcessedPath, suffix) || got.Error != "" || got.RawDeleted == damaged || len(e.TakeProcessingFailures()) != 0 {
				t.Fatal("conversion did not publish the expected verified copy", got)
			}
			if out, err := exec.CommandContext(ctx, path, "-hide_banner", "-loglevel", "error", "-xerror", "-err_detect", "explode", "-i", got.ProcessedPath, "-f", "null", "-").CombinedOutput(); err != nil || len(out) != 0 {
				t.Fatalf("playback copy did not decode cleanly: %v: %s", err, out)
			}
		})
	}
}
