package app

import (
	"github.com/egoist/mygo/ui"
	"tiktok-live-recorder/internal/assets"
)

func palette(dark bool) *ui.Theme {
	t := ui.LightTheme()
	if dark {
		t = ui.DarkTheme()
		t.Background = ui.Hex("#151515")
		t.Surface = ui.Hex("#1d1d1d")
		t.SurfaceHover = ui.Hex("#292929")
		t.SurfacePressed = ui.Hex("#414141")
		t.Border = ui.Hex("#373737")
		t.Text = ui.Hex("#f5f5f5")
		t.TextMuted = ui.Hex("#a0a0a0")
		t.Accent = ui.Hex("#f4f4f4")
		t.AccentHover = ui.Hex("#dedede")
		t.AccentPressed = ui.Hex("#c8c8c8")
		t.AccentText = ui.Hex("#171717")
		t.Danger = ui.Hex("#ff626c")
		t.Success = ui.Hex("#8bd36d")
		t.Warning = ui.Hex("#f0c55a")
	} else {
		t.Background = ui.Hex("#f1f1f1")
		t.Surface = ui.Hex("#fafafa")
		t.SurfaceHover = ui.Hex("#e8e8e8")
		t.SurfacePressed = ui.Hex("#d4d4d4")
		t.Border = ui.Hex("#d2d2d2")
		t.Text = ui.Hex("#202020")
		t.TextMuted = ui.Hex("#686868")
		t.Accent = ui.Hex("#272727")
		t.AccentHover = ui.Hex("#444444")
		t.AccentPressed = ui.Hex("#111111")
		t.AccentText = ui.Hex("#ffffff")
	}
	t.Radius = 6
	t.Spacing = 4
	t.FontSize = 13
	return t
}
func icon(name string) *ui.SVG {
	b, err := assets.Icons.ReadFile("icons/" + name + ".svg")
	if err != nil {
		return nil
	}
	return ui.MustParseSVG(b)
}

var navIcons = map[string]*ui.SVG{"streamers": icon("list-check"), "recordings": icon("folder"), "logs": icon("file-text"), "settings": icon("settings")}

var dotsIcon = icon("dots")
var connectionIcon = icon("connection")
var warningIcon = icon("warning")
