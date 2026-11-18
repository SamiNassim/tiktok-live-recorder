package app

import (
	"github.com/egoist/mygo/ui"
	"tiktok-live-recorder/internal/logging"
	"tiktok-live-recorder/internal/monitor"
)

func (a *application) viewStreamerLogs(id string) {
	a.page = "logs"
	a.logStreamer = id
	a.logQuery, a.level = "", "All levels"
	a.logList.ScrollTo(0, ui.Start)
}

func streamerLogRow(rows []monitor.Row, id string) monitor.Row {
	for _, row := range rows {
		if row.Streamer.ID == id {
			return row
		}
	}
	return monitor.Row{}
}

func (a *application) canRetryCheck(r monitor.Row) bool {
	return !a.preview && a.engine != nil && a.snapshot.Polling && !r.Streamer.Paused &&
		r.Status != "Recording" && r.Status != "Starting" && r.Status != "Checking" && r.Status != "Finalizing"
}

func (a *application) retryCheck(id string) {
	a.work(func() error { return a.engine.RetryCheck(id) })
}

func (a *application) streamerLogIssue(c *ui.Context) {
	r := streamerLogRow(a.snapshot.Rows, a.logStreamer)
	if r.Error == "" {
		return
	}
	t := c.Theme()
	ui.Row(c).Padding(14, 16).Gap(14).AlignItems(ui.Center).FillWidth().Background(t.Warning.Alpha(0.08)).Children(func() {
		ui.Column(c).Grow(1).MinWidth(0).Gap(5).Children(func() {
			ui.Text(c, "Current issue").FontSize(12).FontWeight(600).TextColor(t.Warning)
			ui.Text(c, r.Error).FontSize(12).Selectable()
			detail := "Resolve the issue above; checks retry automatically. The warning clears when the failed operation succeeds."
			if !a.snapshot.Polling {
				detail = "Monitoring is disabled. Enable it in Settings to retry."
			} else if r.Streamer.Paused {
				detail = "Monitoring is paused for this streamer. Resume monitoring to retry."
			}
			ui.Text(c, detail).FontSize(11).TextColor(t.TextMuted)
		})
		if ui.Button(c, "Retry check").Disabled(a.busy || !a.canRetryCheck(r)).Clicked() {
			a.retryCheck(r.Streamer.ID)
		}
	})
}

func logMenu(m *ui.Menu, entry logging.Entry, services ui.Services) {
	if m.Item("Copy message").Chosen() {
		services.WriteClipboard(entry.Message)
	}
	if m.Item("Copy diagnostic details").Disabled(entry.Details == "").Chosen() {
		services.WriteClipboard(entry.Details)
	}
}
