package app

import (
	"math"

	"github.com/egoist/mygo/ui"
	"tiktok-live-recorder/internal/state"
)

func (a *application) splitSettings(c *ui.Context) {
	t := c.Theme()
	ui.Row(c).FillWidth().AlignItems(ui.Center).Children(func() {
		ui.Text(c, "Split recordings").Grow(1)
		ui.Select(c, &a.splitMode, []string{"Off", "By duration", "By file size"}).Width(170).Label("Split recordings")
	})
	if a.splitMode == "By duration" {
		ui.Row(c).FillWidth().AlignItems(ui.Center).Children(func() {
			ui.Text(c, "Split interval").Grow(1)
			ui.Select(c, &a.splitChoice, []string{"30 minutes", "1 hour", "Custom"}).Width(170).Label("Split recordings every")
		})
		if a.splitChoice == "Custom" {
			number(c, "Split interval (minutes)", &a.splitMinutes, 1, float64(state.MaxSplitMinutes))
		}
	}
	if a.splitMode == "By file size" {
		ui.Row(c).FillWidth().AlignItems(ui.Center).Gap(10).Children(func() {
			ui.Text(c, "File size").Grow(1)
			step := float64(state.BytesPerMB) / a.splitSizeScale()
			ui.NumberInput(c, &a.splitSize, float64(state.MinSplitBytes)/a.splitSizeScale(), float64(state.MaxSplitBytes)/a.splitSizeScale(), step).Width(160).Height(34).Label("Split file size")
			before := a.splitSizeScale()
			if ui.Select(c, &a.splitSizeUnit, []string{"MB", "GB"}).Width(80).MinWidth(80).Label("Split size unit").Changed() {
				a.splitSize = math.Round(a.splitSize*before) / a.splitSizeScale()
			}
		})
		ui.Text(c, "Cuts at keyframes. Clips may slightly exceed the selected size. MB and GB use decimal units.").FontSize(11).TextColor(t.TextMuted).MaxLines(3)
	}
	if a.splitMode != "Off" {
		toggle(c, "Group split clips in a separate folder", "Create a folder for each broadcast. Turn off to save clips directly in the recording folder, or the streamer's folder when enabled.", &a.cfg.FolderPerSplitRecording).Label("Split recording folder")
	}
	ui.Text(c, "Applies to new recordings. Completed clips are available while live. Original FLV tags and timestamps are preserved; each clip repeats the source headers.").FontSize(11).TextColor(t.TextMuted).MaxLines(3)
}
