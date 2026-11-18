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
	a.data.Recordings[0].RawDeleted = true
	tt.Frame()
	tt.RightClickAt(file.X, label.Y+label.H/2)
	for _, action := range []string{"Open original FLV", "Show original FLV in folder", "Copy original FLV path"} {
		if err := tt.ChooseMenuItem(action); err == nil {
			t.Fatal("deleted original still has an enabled action", action)
		}
	}
	if err := tt.ChooseMenuItem("Copy file path"); err != nil {
		t.Fatal(err)
	}
	if tt.Clipboard() != processed {
		t.Fatal("raw cleanup disabled the surviving playback copy")
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

func TestRecordingConversionMenuAvailability(t *testing.T) {
	a := accountFixture(t)
	a.page = "recordings"
	now := time.Now()
	a.data.Recordings = []state.Recording{
		{ID: "failed", Name: "Failed Demo", Path: "failed.flv", EndedAt: now, Error: "Synthetic processing error"},
		{ID: "raw", Name: "Raw Demo", Path: "raw.flv", EndedAt: now},
		{ID: "active", Name: "Active Demo", Path: "active.flv"},
		{ID: "deleted", Name: "Deleted Demo", Path: "deleted.flv", EndedAt: now, RawDeleted: true},
	}
	tt := ui.NewTester(a.view, 1400, 900)
	for _, test := range []struct{ name, action string }{
		{"Failed Demo", "Retry MP4 conversion"},
		{"Raw Demo", "Convert to MP4"},
		{"Active Demo", "Convert to MP4"},
		{"Deleted Demo", "Convert to MP4"},
	} {
		label, ok := tt.Find(test.name)
		if !ok {
			t.Fatal("recording row missing", test.name)
		}
		tt.RightClickAt(label.X, label.Y+label.H/2)
		if !slices.Contains(tt.Menu(), test.action) {
			t.Fatal("conversion action missing", test.name, tt.Menu())
		}
		if err := tt.ChooseMenuItem(test.action); err == nil {
			t.Fatal("conversion enabled in preview or for an unavailable original", test.name)
		}
		tt.CloseMenu()
	}
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

func TestRecordingRowsShowLiveConversionProgressAndReconversion(t *testing.T) {
	for _, theme := range []string{"dark", "light"} {
		for _, width := range []int{1050, 1400} {
			t.Run(fmt.Sprintf("%s-%d", theme, width), func(t *testing.T) {
				testRecordingConversionRow(t, theme, width)
			})
		}
	}
}

func testRecordingConversionRow(t *testing.T, theme string, width int) {
	t.Helper()
	a := accountFixture(t)
	a.cfg.Theme = theme
	a.page = "recordings"
	now := time.Now()
	r := state.Recording{ID: "completed", Name: "Synthetic conversion", Path: "source.flv", ProcessedPath: "previous.mp4", StartedAt: now.Add(-time.Hour), EndedAt: now}
	a.data.Recordings = []state.Recording{r, {ID: "neighbor", Name: "Neighboring recording", Path: "neighbor.flv", StartedAt: now.Add(-2 * time.Hour), EndedAt: now}}
	tt := ui.NewTester(a.view, width, 650)
	anchors := make(map[string]ui.Rect)
	for _, text := range []string{r.Name, "Neighboring recording", "File"} {
		anchors[text], _ = tt.Find(text)
	}
	checkLayout := func() {
		t.Helper()
		for text, before := range anchors {
			if after, ok := tt.Find(text); !ok || after != before {
				t.Fatalf("conversion moved %q: before=%+v, after=%+v", text, before, after)
			}
		}
	}
	a.snapshot.Processing = map[string]monitor.ProcessingState{r.Path: {Queued: true, Overall: -1}}
	tt.Frame()
	if !tt.HasText("Queued…") || !tt.HasText("Creating MP4…") {
		t.Fatal("queued conversion is missing its two-line status")
	}
	checkLayout()
	job := monitor.ProcessingState{StartedAt: now.Add(-2 * time.Minute), Pass: 3, Passes: 4, Percent: 50, Overall: 62, Progress: recording.Progress{Phase: "Encoding MP4", MediaTime: 12*time.Minute + 34*time.Second, Speed: 1.25}}
	a.snapshot.Processing[r.Path] = job
	tt.Frame()
	checkLayout()
	for _, text := range []string{"Creating MP4…", "Encoding 62%"} {
		rect, ok := tt.Find(text)
		if !ok || rect.X+rect.W > float32(width) || rect.Y+rect.H > 650 {
			t.Fatalf("conversion progress missing or clipped: %q, %+v", text, rect)
		}
	}
	percentages := 0
	for _, text := range tt.Texts() {
		percentages += strings.Count(text, "%")
		if strings.Contains(text, "Pass ") || strings.Contains(text, "overall") {
			t.Fatalf("extra conversion line is visible: %q", text)
		}
	}
	if percentages != 1 {
		t.Fatalf("conversion must show only its overall percentage, got %d", percentages)
	}
	for _, text := range []string{"Processed this pass: 12m 34s", "Speed: 1.2×", "Elapsed: 2m", "Recovery can add passes"} {
		if !strings.Contains(conversionDetails(job, now), text) {
			t.Fatalf("conversion details missing %q", text)
		}
	}
	if dir := os.Getenv("TIKTOK_UI_SNAPSHOTS"); dir != "" {
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatal(err)
		}
		f, err := os.Create(filepath.Join(dir, fmt.Sprintf("conversion-%s-%d.png", theme, width)))
		if err != nil {
			t.Fatal(err)
		}
		err = png.Encode(f, tt.Image())
		closeErr := f.Close()
		if err != nil || closeErr != nil {
			t.Fatal(err, closeErr)
		}
	}
	label, _ := tt.Find(r.Name)
	tt.RightClickAt(label.X, label.Y+label.H/2)
	if !slices.Contains(tt.Menu(), "Cancel conversion") {
		t.Fatal("active conversion has no cancellation action")
	}
	tt.CloseMenu()
	for _, phase := range []string{"Preparing recovery", "Verifying MP4", "Saving MP4", "Finishing conversion", "Canceling conversion"} {
		job.Progress.Phase = phase
		a.snapshot.Processing[r.Path] = job
		tt.Frame()
		checkLayout()
	}
	a.snapshot.Processing = nil
	tt.Frame()
	checkLayout()
	if !tt.HasText("previous.mp4") {
		t.Fatal("completed conversion did not restore the playback filename")
	}
	tt.RightClickAt(label.X, label.Y+label.H/2)
	if !slices.Contains(tt.Menu(), "Convert again") {
		t.Fatal("existing playback copy cannot be converted again")
	}
	tt.CloseMenu()
}
