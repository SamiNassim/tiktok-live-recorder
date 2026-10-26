package state

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestDefaultsAndRoundTrip(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !s.Config().PollingEnabled {
		t.Fatal("queued polling must be enabled by default")
	}
	c := s.Config()
	if !c.PostProcess || c.OutputExtension != "mkv" || c.FFmpegArgs != "-map 0:v? -map 0:a? -c copy" {
		t.Fatal("new installations must create lossless MKV playback copies")
	}
	if c.DeleteRawAfterProcessing {
		t.Fatal("new installations must keep original FLV files")
	}
	c.Cookie = "temporary-cookie"
	if err = s.SaveConfig(c); err != nil {
		t.Fatal(err)
	}
	if err = s.Update(func(d *Data) error { d.Streamers = append(d.Streamers, Streamer{ID: "alice"}); return nil }); err != nil {
		t.Fatal(err)
	}
	r, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if r.Config().Cookie != c.Cookie || len(r.Data().Streamers) != 1 {
		t.Fatal("snapshot lost")
	}
	d := r.Data()
	d.Streamers[0].ID = "mutated"
	if r.Data().Streamers[0].ID != "alice" {
		t.Fatal("snapshot aliases state")
	}
}

func TestDisablingProcessingClearsRawCleanup(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cfg := s.Config()
	cfg.DeleteRawAfterProcessing = true
	if err := s.SaveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(s.Directory())
	if err != nil || !reopened.Config().DeleteRawAfterProcessing {
		t.Fatal("explicit cleanup preference was not saved", err)
	}
	cfg.PostProcess = false
	if err := reopened.SaveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	reopened, err = Open(s.Directory())
	if err != nil || reopened.Config().PostProcess || reopened.Config().DeleteRawAfterProcessing {
		t.Fatal("cleanup remained enabled without processing", err)
	}
}

func TestSavedProcessingPreferencesSurviveNewDefaults(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		name := "disabled"
		if enabled {
			name = "custom format"
		}
		t.Run(name, func(t *testing.T) {
			s, err := Open(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			cfg := s.Config()
			cfg.PostProcess = enabled
			cfg.OutputExtension = "mp4"
			cfg.FFmpegArgs += " -movflags +faststart"
			if err := s.SaveConfig(cfg); err != nil {
				t.Fatal(err)
			}
			reopened, err := Open(s.Directory())
			if err != nil {
				t.Fatal(err)
			}
			got := reopened.Config()
			if got.PostProcess != enabled || got.OutputExtension != cfg.OutputExtension || got.FFmpegArgs != cfg.FFmpegArgs {
				t.Fatal("saved processing preferences were replaced by defaults")
			}
		})
	}
}

func TestMalformedFileIsPreserved(t *testing.T) {
	dir := t.TempDir()
	name := filepath.Join(dir, "config.json")
	raw := []byte("{broken")
	if err := os.WriteFile(name, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(dir); err == nil {
		t.Fatal("accepted corrupt config")
	}
	b, _ := os.ReadFile(name)
	if string(b) != string(raw) {
		t.Fatal("overwrote corrupt config")
	}
}
func TestConcurrentAtomicUpdates(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() {
			if err := s.Update(func(d *Data) error { d.Streamers = append(d.Streamers, Streamer{ID: "x"}); return nil }); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if len(s.Data().Streamers) != 20 {
		t.Fatal("lost concurrent updates")
	}
	if _, err = Open(s.Directory()); err != nil {
		t.Fatal(err)
	}
}

func TestWindowStateDoesNotOverwritePreferences(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	draft := s.Config()
	w := Window{X: 40, Y: 60, Width: 1300, Height: 850}
	if err = s.SaveWindow(w); err != nil {
		t.Fatal(err)
	}
	draft.Theme = "dark"
	if err = s.SaveConfig(draft); err != nil {
		t.Fatal(err)
	}
	r, err := Open(s.Directory())
	if err != nil {
		t.Fatal(err)
	}
	if r.Config().Theme != "dark" || r.Config().Window != w {
		t.Fatalf("lost window or preference: %+v", r.Config())
	}
}
