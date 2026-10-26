package app

import (
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/egoist/mygo/ui"
	"tiktok-live-recorder/internal/monitor"
	"tiktok-live-recorder/internal/state"
)

func TestStreamerContextMenuCoversFullRow(t *testing.T) {
	a := accountFixture(t)
	a.seedPreview()
	tt := ui.NewTester(a.view, 1400, 900)
	left, ok := tt.Find("Streamer")
	if !ok {
		t.Fatal("streamer table missing")
	}
	for _, name := range []string{"Demo creator 01", "Demo creator 03"} {
		label, _ := tt.Find(name)
		actions, _ := tt.Find("Actions for " + name)
		for x := left.X - 5; x < actions.X+actions.W+5; x += 11 {
			tt.RightClickAt(x, label.Y+label.H/2)
			menu := tt.Menu()
			if !slices.Contains(menu, "Rename…") || !slices.Contains(menu, "Remove streamer…") {
				t.Fatalf("streamer menu missing over %s at x=%v: %v", name, x, menu)
			}
			tt.CloseMenu()
		}
		// Choose from a non-identity column and verify the target row.
		status, _ := tt.Find("Status")
		tt.RightClickAt(status.X, label.Y+label.H/2)
		if err := tt.ChooseMenuItem("Rename…"); err != nil {
			t.Fatal(err)
		}
		row := slices.IndexFunc(a.snapshot.Rows, func(r monitor.Row) bool { return r.Streamer.DisplayName() == name })
		if !a.renameOpen || a.renameID != a.snapshot.Rows[row].Streamer.ID {
			t.Fatal("row context menu targeted another streamer")
		}
		if err := tt.Click("Cancel"); err != nil {
			t.Fatal(err)
		}
	}
}

func TestRecordingMenusCoverFullRowAndCopyPath(t *testing.T) {
	a := accountFixture(t)
	a.preview = false
	a.page = "recordings"
	now := time.Now()
	path := filepath.Join(t.TempDir(), "demo.flv")
	processed := filepath.Join(t.TempDir(), "demo.mkv")
	rawOnly := filepath.Join(t.TempDir(), "raw-only.flv")
	a.data.Recordings = []state.Recording{
		{ID: "done", Name: "Completed Demo", Path: path, ProcessedPath: processed, StartedAt: now.Add(-time.Hour), EndedAt: now, Bytes: 1024},
		{ID: "live", Name: "Active Demo", Path: filepath.Join(t.TempDir(), "active.flv"), StartedAt: now},
		{ID: "raw", Name: "Raw Demo", Path: rawOnly, StartedAt: now.Add(-2 * time.Hour), EndedAt: now, Bytes: 1024},
	}
	tt := ui.NewTester(a.view, 1400, 900)
	if !tt.HasText("demo.mkv") || tt.HasText("demo.flv") {
		t.Fatal("completed playback copy is not the primary file in history")
	}
	left, _ := tt.Find("Streamer")
	file, _ := tt.Find("File")
	label, _ := tt.Find("Completed Demo")
	for x := left.X - 5; x < 1360; x += 11 {
		tt.RightClickAt(x, label.Y+label.H/2)
		menu := tt.Menu()
		for _, want := range []string{"Open recording", "Show in folder", "Copy file path", "Remove from history"} {
			if !slices.Contains(menu, want) {
				t.Fatalf("recording menu missing %q at x=%v: %v", want, x, menu)
			}
		}
		tt.CloseMenu()
	}
	tt.RightClickAt(file.X, label.Y+label.H/2)
	if err := tt.ChooseMenuItem("Copy file path"); err != nil {
		t.Fatal(err)
	}
	if tt.Clipboard() != processed {
		t.Fatal("primary action did not prefer the playback copy", tt.Clipboard())
	}
	tt.RightClickAt(file.X, label.Y+label.H/2)
	if !slices.Contains(tt.Menu(), "Open original FLV") || !slices.Contains(tt.Menu(), "Show original FLV in folder") {
		t.Fatal("original FLV menu missing", tt.Menu())
	}
	if err := tt.ChooseMenuItem("Copy original FLV path"); err != nil {
		t.Fatal(err)
	}
	if tt.Clipboard() != path {
		t.Fatal("copied the wrong original path")
	}
	label, _ = tt.Find("Raw Demo")
	tt.RightClickAt(file.X, label.Y+label.H/2)
	if slices.Contains(tt.Menu(), "Open original FLV") {
		t.Fatal("duplicate raw actions shown without a playback copy")
	}
	if err := tt.ChooseMenuItem("Copy file path"); err != nil {
		t.Fatal(err)
	}
	if tt.Clipboard() != rawOnly {
		t.Fatal("raw recording is not the fallback while a copy is unavailable")
	}
	label, _ = tt.Find("Active Demo")
	tt.RightClickAt(file.X, label.Y+label.H/2)
	if slices.Contains(tt.Menu(), "Open original FLV") {
		t.Fatal("duplicate raw actions shown for an active recording")
	}
	for _, action := range []string{"Open recording", "Remove from history"} {
		if err := tt.ChooseMenuItem(action); err == nil {
			t.Fatal("active recording action was not disabled", action)
		}
	}
	tt.CloseMenu()
}

func TestClearRecordingHistoryConfirmationAndAvailability(t *testing.T) {
	a := accountFixture(t)
	a.preview = false // No account cookie or engine; only local history controls run.
	a.page = "recordings"
	now := time.Now()
	tt := ui.NewTester(a.view, 1050, 650)
	assertUnavailable := func() {
		t.Helper()
		if err := tt.Click("Clear history…"); err != nil {
			t.Fatal(err)
		}
		if a.clearHistoryOpen {
			t.Fatal("history clear enabled without completed recordings")
		}
	}
	assertUnavailable()
	a.data.Recordings = []state.Recording{{ID: "live", Name: "Active Demo", StartedAt: now}}
	tt.Frame()
	assertUnavailable()
	a.data.Recordings = append(a.data.Recordings, state.Recording{ID: "done", Name: "Completed Demo", StartedAt: now.Add(-time.Hour), EndedAt: now})
	tt.Frame()
	for _, label := range []string{"Clear history…", "Open recordings folder"} {
		r, ok := tt.Find(label)
		if !ok || r.X < 220 || r.X+r.W > 1050 {
			t.Fatal("recording controls overflow compact window", label, r)
		}
	}
	if err := tt.Click("Clear history…"); err != nil {
		t.Fatal(err)
	}
	if !a.clearHistoryOpen || !tt.HasText("Files stay on disk. Active recordings stay in the list.") {
		t.Fatal("history confirmation or file-preservation explanation missing")
	}
	if err := tt.Click("Cancel"); err != nil {
		t.Fatal(err)
	}
	if a.clearHistoryOpen || len(a.data.Recordings) != 2 {
		t.Fatal("cancel changed history")
	}
}
