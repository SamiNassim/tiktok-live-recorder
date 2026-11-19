package app

import (
	"fmt"
	"image/png"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/egoist/mygo/ui"
	"tiktok-live-recorder/internal/monitor"
	"tiktok-live-recorder/internal/recording"
	"tiktok-live-recorder/internal/state"
)

func TestRepairMenuRequiresConfirmationAndExplainsTradeoffs(t *testing.T) {
	for _, theme := range []string{"light", "dark"} {
		for _, width := range []int{1050, 1400} {
			t.Run(fmt.Sprintf("%s-%d", theme, width), func(t *testing.T) {
				a := accountFixture(t)
				a.preview, a.page, a.cfg.Theme = false, "recordings", theme
				a.cfg.FFmpegArgs = "-c copy"
				// Opening and canceling the modal must never call this engine.
				a.engine = &monitor.Engine{}
				now := time.Now()
				r := state.Recording{ID: "completed", Name: "Synthetic recording", Path: "@demo_creator_2026-01-01_12-00-00_01234567.flv", ProcessedPath: "previous.mp4", StartedAt: now.Add(-time.Minute), EndedAt: now}
				a.data.Recordings = []state.Recording{r}
				tt := ui.NewTester(a.view, width, 650)
				row, ok := tt.Find(r.Name)
				if !ok {
					t.Fatal("recording row is missing")
				}
				tt.RightClickAt(row.X, row.Y+row.H/2)
				if err := tt.ChooseMenuItem("Repair recording…"); err != nil {
					t.Fatal(err)
				}
				tt.Advance(300 * time.Millisecond)
				if !a.repairOpen || a.repairTarget.ID != r.ID || a.engine.IsProcessing(r.Path) {
					t.Fatal("repair did not wait for confirmation or targeted the wrong recording")
				}
				for _, want := range []string{
					"Try to repair this recording?", r.Path, "more tolerant conversion", "tries repair automatically",
					"visual glitches", "audio sync issues", "always kept", "may still fail",
					"Custom output options are not used for repair.", "Create repaired MP4",
				} {
					found := false
					for _, text := range tt.Texts() {
						if !strings.Contains(text, want) {
							continue
						}
						found = true
						if rect, ok := tt.Find(text); !ok || rect.X < 0 || rect.Y < 0 || rect.X+rect.W > float32(width) || rect.Y+rect.H > 650 {
							t.Fatal("repair confirmation is clipped", text, rect)
						}
					}
					if !found {
						t.Fatal("repair explanation is missing", want)
					}
				}
				if dir := os.Getenv("TIKTOK_UI_SNAPSHOTS"); dir != "" {
					if err := os.MkdirAll(dir, 0755); err != nil {
						t.Fatal(err)
					}
					f, err := os.Create(filepath.Join(dir, fmt.Sprintf("repair-modal-%s-%d.png", theme, width)))
					if err != nil {
						t.Fatal(err)
					}
					err = png.Encode(f, tt.Image())
					if closeErr := f.Close(); err != nil || closeErr != nil {
						t.Fatal(err, closeErr)
					}
				}
				a.busy = true
				tt.Frame()
				if err := tt.Click("Create repaired MP4"); err != nil {
					t.Fatal(err)
				}
				if !a.repairOpen || a.engine.IsProcessing(r.Path) {
					t.Fatal("a disabled confirmation queued repair")
				}
				a.busy = false
				tt.Frame()
				if err := tt.Click("Cancel"); err != nil {
					t.Fatal(err)
				}
				if a.repairOpen || a.repairTarget.ID != "" || a.engine.IsProcessing(r.Path) {
					t.Fatal("cancel retained the repair target or started a repair")
				}
			})
		}
	}
}

func TestRepairRowsShowModeCancellationAndFailureToast(t *testing.T) {
	a := accountFixture(t)
	a.preview, a.page = false, "recordings"
	a.engine = &monitor.Engine{}
	now := time.Now()
	r := state.Recording{ID: "completed", Name: "Synthetic recording", Path: "source.flv", ProcessedPath: "previous.mp4", StartedAt: now.Add(-time.Minute), EndedAt: now}
	a.data.Recordings = []state.Recording{r}
	a.snapshot.Processing = map[string]monitor.ProcessingState{r.Path: {
		Repair: true, Cancelable: true, Overall: 25,
		Progress: recording.Progress{Phase: "Repairing MP4", MediaTime: time.Second},
	}}
	tt := ui.NewTester(a.view, 1050, 650)
	if !tt.HasText("Repairing MP4…") || !tt.HasText("Repairing 25%") {
		t.Fatal("repair progress is missing its mode or percentage")
	}
	row, _ := tt.Find(r.Name)
	tt.RightClickAt(row.X, row.Y+row.H/2)
	if !slices.Contains(tt.Menu(), "Cancel repair") {
		t.Fatal("repair has no cancellation action", tt.Menu())
	}
	if err := tt.ChooseMenuItem("Repair recording…"); err == nil {
		t.Fatal("a second repair can be started during processing")
	}
	tt.CloseMenu()
	a.snapshot.Processing = nil
	a.processingFailures = []monitor.ProcessingFailure{{StreamerID: "demo_creator", Path: r.Path, Message: "The recording contains damaged or incomplete media data; raw FLV kept", Repair: true}}
	tt.Frame()
	if !tt.HasText("Recording repair failed") || !tt.HasText("previous.mp4") || !tt.HasText("View logs") {
		t.Fatal("repair failure hid the previous copy or its feedback")
	}
	if err := tt.Click("View logs"); err != nil {
		t.Fatal(err)
	}
	if a.page != "logs" || a.logStreamer != "demo_creator" {
		t.Fatal("repair failure did not open the relevant logs")
	}
}
