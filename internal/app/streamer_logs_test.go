package app

import (
	"fmt"
	"image/png"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/egoist/mygo/ui"
	"tiktok-live-recorder/internal/monitor"
	"tiktok-live-recorder/internal/state"
)

func streamerLogsFixture(t *testing.T) *application {
	t.Helper()
	a := accountFixture(t)
	a.snapshot.Polling = true
	a.snapshot.Rows = []monitor.Row{{
		Streamer: state.Streamer{ID: "demo", Name: "Demo Creator"},
		Status:   "Needs attention", Error: "Check the recording folder permissions",
	}}
	a.log.Write("WARN", "demo", "Could not start recording: check the recording folder permissions.")
	a.log.Write("WARN", "demo_other", "Another streamer's warning")
	a.log.Write("INFO", "", "Unrelated event mentioning demo")
	a.refresh()
	return a
}

func TestStreamerLogShortcutsShowExactStreamerAndResetOldFilters(t *testing.T) {
	for _, shortcut := range []string{"name double click", "status double click", "keyboard", "view logs", "actions menu", "row menu"} {
		t.Run(shortcut, func(t *testing.T) {
			a := streamerLogsFixture(t)
			a.logQuery, a.level = "old search", "ERROR"
			tt := ui.NewTester(a.view, 1050, 650)
			switch shortcut {
			case "name double click", "status double click":
				label := "Demo Creator"
				if shortcut == "status double click" {
					label = "●  Needs attention"
				}
				for range 2 {
					if err := tt.Click(label); err != nil {
						t.Fatal(err)
					}
				}
			case "keyboard":
				if err := tt.Click("Demo Creator"); err != nil {
					t.Fatal(err)
				}
				tt.Key(0, ui.KeyEnter)
			case "view logs":
				if err := tt.Click("View related logs for @demo"); err != nil {
					t.Fatal(err)
				}
			case "actions menu", "row menu":
				if shortcut == "actions menu" {
					if err := tt.Click("Actions for Demo Creator"); err != nil {
						t.Fatal(err)
					}
				} else if err := tt.RightClick("Demo Creator"); err != nil {
					t.Fatal(err)
				}
				if err := tt.ChooseMenuItem("View related logs"); err != nil {
					t.Fatal(err)
				}
			}
			tt.Frame()
			if a.page != "logs" || a.logStreamer != "demo" || a.logQuery != "" || a.level != "All levels" {
				t.Fatal("shortcut did not open unfiltered logs for the selected streamer")
			}
			if rows := a.logRows(); len(rows) != 1 || rows[0].Streamer != "demo" {
				t.Fatal("related logs included a different streamer or message substring", rows)
			}
			if !tt.HasText("Logs for @demo") || !tt.HasText(a.snapshot.Rows[0].Error) || !tt.HasText("Current issue") {
				t.Fatal("streamer scope or current failure was not visible", tt.Texts())
			}
			if err := tt.Click("Show all streamers"); err != nil {
				t.Fatal(err)
			}
			if a.logStreamer != "" || len(a.logRows()) != 3 {
				t.Fatal("removing the streamer filter retained the scoped cache")
			}
		})
	}
}

func TestCurrentStreamerIssueRefreshesWithoutNewLogEntry(t *testing.T) {
	a := streamerLogsFixture(t)
	a.viewStreamerLogs("demo")
	tt := ui.NewTester(a.view, 1050, 650)
	// The engine's current snapshot no longer has the stale issue shown by
	// the UI. No new log message is needed to update the current-issue panel.
	a.engine = monitor.New(a.store, a.log)
	t.Cleanup(a.engine.Close)
	if !a.refreshVisible() {
		t.Fatal("a resolved or removed issue did not repaint the scoped logs")
	}
	tt.Frame()
	if tt.HasText("Current issue") {
		t.Fatal("scoped logs retained a stale issue after the engine snapshot changed")
	}
}

func TestStreamerIssueFitsNativeLayouts(t *testing.T) {
	for _, theme := range []string{"dark", "light"} {
		for _, size := range [][2]int{{1050, 650}, {1400, 900}} {
			a := streamerLogsFixture(t)
			a.cfg.Theme = theme
			a.snapshot.Rows[0].Error = "Could not start recording: could not create the recording folder; check disk space and folder permissions"
			tt := ui.NewTester(a.view, size[0], size[1])
			for _, page := range []string{"streamers", "logs"} {
				if page == "logs" {
					a.viewStreamerLogs("demo")
					tt.Frame()
				}
				labels := []string{"View related logs for @demo"}
				if page == "logs" {
					labels = []string{a.snapshot.Rows[0].Error, "Retry check", "Search logs", "Show all streamers"}
				}
				for _, label := range labels {
					r, ok := tt.Find(label)
					if !ok || r.X < 0 || r.Y < 0 || r.X+r.W > float32(size[0]) || r.Y+r.H > float32(size[1]) {
						t.Fatalf("issue or shortcut clipped in %s %v: %s %+v", theme, size, label, r)
					}
				}
				if dir := os.Getenv("TIKTOK_UI_SNAPSHOTS"); dir != "" {
					if err := os.MkdirAll(dir, 0755); err != nil {
						t.Fatal(err)
					}
					f, err := os.Create(filepath.Join(dir, fmt.Sprintf("attention-%s-%s-%d.png", page, theme, size[0])))
					if err != nil {
						t.Fatal(err)
					}
					err = png.Encode(f, tt.Image())
					closeErr := f.Close()
					if err != nil || closeErr != nil {
						t.Fatal(err, closeErr)
					}
				}
			}
		}
	}
}

func TestCurrentStreamerIssueSurvivesClearingLogViewAndNavigation(t *testing.T) {
	a := streamerLogsFixture(t)
	a.viewStreamerLogs("demo")
	tt := ui.NewTester(a.view, 1050, 650)
	if err := tt.Click("Clear view"); err != nil {
		t.Fatal(err)
	}
	a.refresh()
	tt.Frame()
	tt.Advance(time.Minute)
	if !tt.HasText(a.snapshot.Rows[0].Error) || !tt.HasText("No matching log entries") {
		t.Fatal("clearing logs or elapsed time hid the unresolved issue", tt.Texts())
	}
	if err := tt.Click("Streamers"); err != nil {
		t.Fatal(err)
	}
	if !tt.HasText("●  Needs attention") {
		t.Fatal("viewing the logs dismissed the warning")
	}
	if err := tt.Click("View related logs for @demo"); err != nil {
		t.Fatal(err)
	}
	if !tt.HasText(a.snapshot.Rows[0].Error) {
		t.Fatal("current failure disappeared when its log entry was no longer retained")
	}
}

func TestRelatedLogsStartAtNewestEntryAndKeepSearchWithinStreamer(t *testing.T) {
	a := streamerLogsFixture(t)
	for range 50 {
		a.log.Write("INFO", "demo", "Older synthetic event")
	}
	a.refresh()
	a.page = "logs"
	tt := ui.NewTester(a.view, 1050, 650)
	a.logList.ScrollTo(45, ui.Start)
	tt.Frame()
	a.log.Write("ERROR", "demo", "Newest failure")
	a.refresh()
	a.viewStreamerLogs("demo")
	tt.Frame()
	if !tt.HasText("Newest failure") {
		t.Fatal("related logs retained an old scroll position")
	}
	a.logQuery, a.level = "permissions", "WARN"
	tt.Frame()
	if rows := a.logRows(); len(rows) != 1 || rows[0].Streamer != "demo" {
		t.Fatal("search or severity filter did not compose with the streamer filter")
	}
}
