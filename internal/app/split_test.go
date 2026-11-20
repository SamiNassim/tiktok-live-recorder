package app

import (
	"image/png"
	"os"
	"path/filepath"
	"testing"

	"github.com/egoist/mygo/ui"
	"tiktok-live-recorder/internal/state"
)

func TestNativeSplitControlsAndPersistence(t *testing.T) {
	a := accountFixture(t)
	for _, theme := range []string{"dark", "light"} {
		a.cfg.Theme = theme
		for _, width := range []int{774, 1124} {
			tt := ui.NewTester(a.splitSettings, width, 450)
			if _, ok := tt.Find("Split recording folder"); ok {
				t.Fatal("folder switch visible while splitting off")
			}
			if err := tt.Click("Off"); err != nil {
				t.Fatal(err)
			}
			if err := tt.Click("By duration"); err != nil {
				t.Fatal(err)
			}
			if err := tt.Click("30 minutes"); err != nil {
				t.Fatal(err)
			}
			if err := tt.Click("Custom"); err != nil {
				t.Fatal(err)
			}
			a.splitMinutes = 45
			tt.Frame()
			if got := a.settingsConfig(); got.SplitMinutes != 45 || got.SplitBytes != 0 {
				t.Fatal("custom duration was not applied")
			}
			if err := tt.Click("Split recording folder"); err != nil {
				t.Fatal(err)
			}
			if a.cfg.FolderPerSplitRecording {
				t.Fatal("folder switch did not update")
			}
			if err := tt.Click("By duration"); err != nil {
				t.Fatal(err)
			}
			if err := tt.Click("By file size"); err != nil {
				t.Fatal(err)
			}
			if _, ok := tt.Find("Split recordings every"); ok {
				t.Fatal("duration control remained in file size mode")
			}
			if err := tt.Click("GB"); err != nil {
				t.Fatal(err)
			}
			if err := tt.Click("MB"); err != nil {
				t.Fatal(err)
			}
			if a.splitSize != 2000 || a.settingsConfig().SplitBytes != 2*state.BytesPerGB {
				t.Fatal("unit change altered byte limit")
			}
			a.splitSize = 500
			tt.Frame()
			for _, label := range []string{"Split file size", "Split size unit", "Split recording folder"} {
				r, ok := tt.Find(label)
				if !ok || r.X < 0 || r.X+r.W > float32(width) || r.Y+r.H > 450 {
					t.Fatal("split control clipped", label, r)
				}
			}
			if err := a.store.SaveConfig(a.settingsConfig()); err != nil {
				t.Fatal(err)
			}
			reopened, err := state.Open(a.store.Directory())
			if err != nil {
				t.Fatal(err)
			}
			a.cfg = reopened.Config()
			a.loadNumbers()
			tt.Frame()
			if a.splitMode != "By file size" || a.splitSize != 500 || a.splitSizeUnit != "MB" || a.cfg.FolderPerSplitRecording || a.unsavedSettings() {
				t.Fatal("saved split settings were not restored")
			}
			if err := tt.Click("MB"); err != nil {
				t.Fatal(err)
			}
			if err := tt.Click("GB"); err != nil {
				t.Fatal(err)
			}
			if a.splitSize != 0.5 || a.settingsConfig().SplitBytes != 500*state.BytesPerMB {
				t.Fatal("fractional GB conversion lost bytes")
			}
			if dir := os.Getenv("TIKTOK_UI_SNAPSHOTS"); dir != "" {
				if err := os.MkdirAll(dir, 0755); err != nil {
					t.Fatal(err)
				}
				f, err := os.Create(filepath.Join(dir, "split-size-"+theme+".png"))
				if err != nil {
					t.Fatal(err)
				}
				err = png.Encode(f, tt.Image())
				closeErr := f.Close()
				if err != nil || closeErr != nil {
					t.Fatal(err, closeErr)
				}
			}
			a.cfg = state.Defaults()
			a.loadNumbers()
		}
	}
}

func TestNativeProxyControlsValidationAndUnsavedChanges(t *testing.T) {
	a := accountFixture(t)
	tt := ui.NewTester(a.proxySettings, 774, 450)
	if _, ok := tt.Find("Proxy URL"); ok {
		t.Fatal("disabled proxy inputs visible")
	}
	if err := tt.Click("Enable proxy connection"); err != nil {
		t.Fatal(err)
	}
	for _, label := range []string{"Proxy URL", "Proxy username", "Proxy password"} {
		r, ok := tt.Find(label)
		if !ok || r.X < 0 || r.X+r.W > 774 {
			t.Fatal("proxy control missing or clipped", label, r)
		}
	}
	a.cfg.Proxy.URL = "http://proxy.invalid:8080"
	a.cfg.Proxy.Username = "synthetic-user"
	a.cfg.Proxy.Password = "synthetic-password"
	tt.Frame()
	if !a.unsavedSettings() || a.cfg.Proxy.Validate() != nil {
		t.Fatal("proxy edits not tracked")
	}
	if err := a.store.SaveConfig(a.settingsConfig()); err != nil {
		t.Fatal(err)
	}
	if a.unsavedSettings() {
		t.Fatal("saved proxy still marked unsaved")
	}
	if dir := os.Getenv("TIKTOK_UI_SNAPSHOTS"); dir != "" {
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatal(err)
		}
		f, err := os.Create(filepath.Join(dir, "proxy.png"))
		if err != nil {
			t.Fatal(err)
		}
		err = png.Encode(f, tt.Image())
		closeErr := f.Close()
		if err != nil || closeErr != nil {
			t.Fatal(err, closeErr)
		}
	}
	a.cfg.Proxy.URL = "invalid"
	tt.Frame()
	if a.cfg.Proxy.Validate() == nil || !a.unsavedSettings() {
		t.Fatal("invalid proxy not tracked")
	}
}

func TestNativeSettingsSplitAndProxyLayouts(t *testing.T) {
	for _, theme := range []string{"dark", "light"} {
		a := accountFixture(t)
		a.cfg.Theme = theme
		a.cfg.RecordingPath = filepath.Join("recordings", "TikTok Live Recorder")
		a.page = "settings"
		a.cfg.Proxy.Enabled = true
		a.cfg.Proxy.URL = "http://proxy.invalid:8080"
		a.splitMode = "By file size"
		tt := ui.NewTester(a.view, 1050, 800)
		for _, target := range []struct{ label, name string }{{"Enable proxy connection", "proxy"}, {"Split recordings", "split-size"}, {"Split interval (minutes)", "split-duration"}} {
			if target.name == "split-duration" {
				a.splitMode = "By duration"
				a.splitChoice = "Custom"
				a.splitMinutes = 45
				tt.Frame()
			}
			control, ok := tt.Find(target.label)
			if !ok {
				t.Fatal("settings control missing", target.label)
			}
			viewport, _ := tt.Find("Settings sections")
			a.settingsScroll.Y += control.Y - viewport.Y - 110
			tt.Frame()
			control, ok = tt.Find(target.label)
			if !ok || control.X < 220 || control.X+control.W > 1050 || control.Y < viewport.Y || control.Y+control.H > 800 {
				t.Fatal("settings control clipped", target.label, control)
			}
			if dir := os.Getenv("TIKTOK_UI_SNAPSHOTS"); dir != "" {
				if err := os.MkdirAll(dir, 0755); err != nil {
					t.Fatal(err)
				}
				f, err := os.Create(filepath.Join(dir, "settings-"+target.name+"-"+theme+".png"))
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
