package recording

import (
	"path/filepath"
	"testing"
	"time"

	"tiktok-live-recorder/internal/state"
)

func TestRecordingPathsStartWithStableIdentity(t *testing.T) {
	cfg := state.Defaults()
	cfg.RecordingPath = t.TempDir()
	streamer := state.Streamer{ID: "demo_creator", Name: "Display Name", CustomName: "Custom Name"}
	stamp := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	for _, folders := range []bool{false, true} {
		cfg.FolderPerStreamer = folders
		path := recordingPath(streamer, cfg, stamp, "synthetic")
		want := "@demo_creator_2026-01-02_03-04-05_synthetic.flv"
		if filepath.Base(path) != want {
			t.Fatal("filename does not use stable identity", filepath.Base(path))
		}
		directory := cfg.RecordingPath
		if folders {
			directory = filepath.Join(directory, "@demo_creator_Custom Name")
		}
		if filepath.Dir(path) != directory {
			t.Fatal("folder does not start with stable identity", filepath.Base(filepath.Dir(path)))
		}
	}
}
