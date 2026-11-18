package app

import (
	"fmt"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"testing"

	"github.com/egoist/mygo/ui"
)

func hasRenderedColor(frame *image.RGBA, rect ui.Rect, want ui.Color) bool {
	area := image.Rect(int(rect.X), int(rect.Y), int(rect.X+rect.W), int(rect.Y+rect.H)).Intersect(frame.Bounds())
	for y := area.Min.Y; y < area.Max.Y; y++ {
		for x := area.Min.X; x < area.Max.X; x++ {
			if frame.RGBAAt(x, y) == (color.RGBA{R: want.R, G: want.G, B: want.B, A: 255}) {
				return true
			}
		}
	}
	return false
}

func TestSelectedStreamerRowKeepsEveryElementReadable(t *testing.T) {
	for _, theme := range []string{"dark", "light"} {
		for _, width := range []int{1050, 1400} {
			t.Run(fmt.Sprintf("%s-%d", theme, width), func(t *testing.T) {
				a := streamerLogsFixture(t)
				a.cfg.Theme = theme
				tt := ui.NewTester(a.view, width, 650)
				if err := tt.Click("Demo Creator"); err != nil {
					t.Fatal(err)
				}
				for _, focus := range []string{"row", "search"} {
					if focus == "search" {
						if err := tt.Click("Search streamers"); err != nil {
							t.Fatal(err)
						}
					}
					if a.streamSelected != 0 {
						t.Fatal("the streamer row is not selected")
					}
					// A native selected row uses the accent background, which
					// turns this app's monochrome palette to the opposite theme.
					selected := palette(theme != "dark")
					frame := tt.Image()
					for _, text := range []struct {
						label string
						color ui.Color
					}{
						{"Demo Creator", selected.Text},
						{"@demo", selected.TextMuted},
						{"View logs", selected.Accent},
						{"●  Needs attention", selected.Warning},
						{"Normal", selected.TextMuted},
					} {
						r, ok := tt.Find(text.label)
						if !ok || !hasRenderedColor(frame, r, text.color) {
							t.Errorf("selected %s with %s focus did not render %q in its readable color", theme, focus, text.label)
						}
					}
					name, _ := tt.Find("Demo Creator")
					avatar := ui.Rect{X: name.X - 46, Y: name.Y - 8, W: 34, H: 40}
					if !hasRenderedColor(frame, avatar, selected.Accent) {
						t.Error("selected row hid the avatar initial")
					}
					actions, ok := tt.Find("Actions for Demo Creator")
					if !ok || !hasRenderedColor(frame, actions, selected.Text) || !hasRenderedColor(frame, actions, selected.Surface) {
						t.Error("selected row action icon or button surface used the wrong palette")
					}
					if dir := os.Getenv("TIKTOK_UI_SNAPSHOTS"); dir != "" {
						if err := os.MkdirAll(dir, 0755); err != nil {
							t.Fatal(err)
						}
						f, err := os.Create(filepath.Join(dir, fmt.Sprintf("selected-row-%s-%d-%s.png", theme, width, focus)))
						if err != nil {
							t.Fatal(err)
						}
						err = png.Encode(f, frame)
						closeErr := f.Close()
						if err != nil || closeErr != nil {
							t.Fatal(err, closeErr)
						}
					}
				}
			})
		}
	}
}
