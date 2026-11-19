package app

import (
	"context"
	"fmt"
	"image/png"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/egoist/mygo/ui"
	"tiktok-live-recorder/internal/ffmpeg"
	"tiktok-live-recorder/internal/monitor"
	"tiktok-live-recorder/internal/state"
)

func TestFailedConversionShowsToastOnEveryPage(t *testing.T) {
	if _, err := ffmpeg.Find(""); err != nil {
		t.Skip("FFmpeg is not installed")
	}
	for _, page := range []string{"streamers", "recordings", "logs", "settings"} {
		t.Run(page, func(t *testing.T) {
			a := accountFixture(t)
			a.page = page
			cfg := a.store.Config()
			cfg.PollingEnabled, cfg.PostProcess = false, false
			if err := a.store.SaveConfig(cfg); err != nil {
				t.Fatal(err)
			}
			raw := filepath.Join(t.TempDir(), "damaged.flv")
			const original = "synthetic invalid media"
			if err := os.WriteFile(raw, []byte(original), 0600); err != nil {
				t.Fatal(err)
			}
			row := state.Recording{ID: "failed", StreamerID: "demo_creator", Name: "Synthetic recording", Path: raw, EndedAt: time.Now(), Error: "Previous conversion failed"}
			if err := a.store.Update(func(d *state.Data) error { d.Recordings = []state.Recording{row}; return nil }); err != nil {
				t.Fatal(err)
			}
			a.engine = monitor.New(a.store, a.log)
			t.Cleanup(a.engine.Close)
			a.refresh()
			tt := ui.NewTester(a.view, 1050, 650)
			retry := func() {
				t.Helper()
				if err := a.engine.RetryProcessing(row.ID); err != nil {
					t.Fatal(err)
				}
				ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
				defer cancel()
				for a.engine.IsProcessing(raw) {
					select {
					case <-ctx.Done():
						t.Fatal("conversion did not finish")
					case <-time.After(10 * time.Millisecond):
					}
				}
			}
			retry()
			if !a.refreshVisible() {
				t.Fatal("conversion failure did not request a repaint", page)
			}
			const message = "Normal conversion and automatic repair failed: The recording contains damaged or incomplete media data; raw FLV kept"
			if len(a.processingFailures) != 1 || !a.processingFailures[0].Repair || a.processingFailures[0].Message != message {
				t.Fatal("conversion failure did not reach the UI", a.processingFailures)
			}
			logged := false
			for _, entry := range a.logs {
				if entry.Message == "Recording repair failed: "+message && entry.Streamer == row.StreamerID {
					if !strings.Contains(entry.Details, "FFmpeg output:") {
						t.Fatal("readable failure log lost FFmpeg's diagnostics")
					}
					logged = true
				}
			}
			if !logged {
				t.Fatal("manual failure log does not match the toast explanation")
			}
			tt.Frame()
			if !slices.Contains(tt.Texts(), "Recording repair failed") || !tt.HasText("damaged.flv\n"+message) || !tt.HasText("View logs") {
				t.Fatal("conversion toast is missing its clear reason, filename or logs action", tt.Texts())
			}
			if len(a.processingFailures) != 0 || len(a.engine.TakeProcessingFailures()) != 0 {
				t.Fatal("displayed feedback was retained for duplicate delivery")
			}
			if err := tt.Click("View logs"); err != nil {
				t.Fatal(err)
			}
			if a.page != "logs" || a.logStreamer != row.StreamerID {
				t.Fatal("toast action did not open the recording's logs")
			}
			// The same error on a fresh retry must still produce a new toast.
			retry()
			if !a.refreshVisible() || len(a.processingFailures) != 1 {
				t.Fatal("an identical retry failure was suppressed")
			}
			tt.Frame()
			if !slices.Contains(tt.Texts(), "Recording repair failed") {
				t.Fatal("repeated retry failed silently")
			}
			tt.Move(0, 0) // Hovering the toast deliberately pauses its timeout.
			tt.Advance(11 * time.Second)
			a.refreshVisible()
			tt.Frame()
			if slices.Contains(tt.Texts(), "Recording repair failed") {
				t.Fatal("expired feedback was shown again")
			}
			if got, err := os.ReadFile(raw); err != nil || string(got) != original {
				t.Fatal("failed conversion changed the original", err)
			}
		})
	}
}

func TestConversionFailureToastLayout(t *testing.T) {
	for _, theme := range []string{"light", "dark"} {
		for _, width := range []int{1050, 1400} {
			t.Run(fmt.Sprintf("%s-%d", theme, width), func(t *testing.T) {
				a := accountFixture(t)
				a.cfg.Theme, a.page = theme, "recordings"
				a.processingFailures = []monitor.ProcessingFailure{{
					StreamerID: "demo_creator",
					Path:       "@demo_creator_2026-01-01_12-00-00_01234567.flv",
					Message:    "Normal conversion and automatic repair failed: The recording contains damaged or incomplete media data; raw FLV kept",
					Repair:     true,
				}}
				tt := ui.NewTester(a.view, width, 650)
				tt.Advance(300 * time.Millisecond)
				for _, text := range tt.Texts() {
					if !strings.Contains(text, "damaged or incomplete") && text != "Recording repair failed" && text != "View logs" {
						continue
					}
					if r, ok := tt.Find(text); !ok || r.X < 0 || r.Y < 0 || r.X+r.W > float32(width) || r.Y+r.H > 650 {
						t.Fatal("conversion feedback overflows the window", text, r)
					}
				}
				if !slices.Contains(tt.Texts(), "Recording repair failed") {
					t.Fatal("conversion toast is missing")
				}
				if dir := os.Getenv("TIKTOK_UI_SNAPSHOTS"); dir != "" {
					if err := os.MkdirAll(dir, 0755); err != nil {
						t.Fatal(err)
					}
					f, err := os.Create(filepath.Join(dir, fmt.Sprintf("conversion-error-%s-%d.png", theme, width)))
					if err != nil {
						t.Fatal(err)
					}
					err = png.Encode(f, tt.Image())
					if closeErr := f.Close(); err != nil || closeErr != nil {
						t.Fatal(err, closeErr)
					}
				}
			})
		}
	}
}

func TestRawCleanupToggleRequiresProcessing(t *testing.T) {
	a := accountFixture(t)
	a.page = "settings"
	tt := ui.NewTester(a.view, 1400, 2600)
	const cleanup = "Delete original FLV after processing"
	const processing = "Create a processed copy with FFmpeg"
	focus := func(label string) bool {
		for range 40 {
			if tt.Focused(label) {
				return true
			}
			tt.Key(0, ui.KeyTab)
		}
		return tt.Focused(label)
	}
	activate := func(label string) {
		t.Helper()
		if !focus(label) {
			t.Fatal("setting is not keyboard accessible", label)
		}
		tt.Key(0, ui.KeySpace)
	}
	if a.cfg.DeleteRawAfterProcessing {
		t.Fatal("cleanup is enabled by default")
	}
	activate(cleanup)
	if !a.cfg.DeleteRawAfterProcessing {
		t.Fatal("cleanup could not be enabled with processing")
	}
	activate(processing)
	if a.cfg.PostProcess || a.cfg.DeleteRawAfterProcessing {
		t.Fatal("disabling processing left cleanup enabled")
	}
	if focus(cleanup) || a.cfg.DeleteRawAfterProcessing {
		t.Fatal("cleanup is interactive without processing")
	}
	activate(processing)
	if !a.cfg.PostProcess || a.cfg.DeleteRawAfterProcessing {
		t.Fatal("re-enabling processing restored destructive cleanup automatically")
	}
	activate(cleanup)
	if !a.cfg.DeleteRawAfterProcessing {
		t.Fatal("cleanup did not become available after re-enabling processing")
	}
}
