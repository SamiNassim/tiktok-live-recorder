package state

import (
	"os"
	"path/filepath"
	"testing"

	"tiktok-live-recorder/internal/network"
)

func TestSplitAndProxySettingsRoundTripAndValidation(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cfg := store.Config()
	cfg.SplitBytes = 500 * BytesPerMB
	cfg.SplitSizeUnit = "MB"
	cfg.FolderPerSplitRecording = false
	cfg.Proxy = network.Config{Enabled: true, URL: "socks5://proxy.invalid:1080", Username: "synthetic-user", Password: "synthetic-password"}
	if err := store.SaveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(store.Directory())
	if err != nil {
		t.Fatal(err)
	}
	if reopened.Config() != cfg {
		t.Fatal("split or proxy preferences lost on reopen")
	}
	for _, edit := range []func(*Config){
		func(c *Config) { c.SplitMinutes = -1 }, func(c *Config) { c.SplitMinutes = MaxSplitMinutes + 1 },
		func(c *Config) { c.SplitBytes = MinSplitBytes - 1 }, func(c *Config) { c.SplitBytes = MaxSplitBytes + 1 },
		func(c *Config) { c.SplitMinutes = 30 }, func(c *Config) { c.SplitSizeUnit = "TB" },
		func(c *Config) { c.Proxy.URL = "invalid" },
	} {
		draft := cfg
		edit(&draft)
		if err := store.SaveConfig(draft); err == nil {
			t.Fatal("invalid split/proxy preference accepted")
		}
		if store.Config() != cfg {
			t.Fatal("failed save changed settings")
		}
	}
	for _, limit := range []int64{0, MinSplitBytes, MaxSplitBytes} {
		draft := cfg
		draft.SplitBytes = limit
		if err := draft.Validate(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestOlderSettingsDefaultToUnsplitRecordings(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(`{"recording_path":"recordings","folder_per_streamer":false}`), 0600); err != nil {
		t.Fatal(err)
	}
	store, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	cfg := store.Config()
	if cfg.SplitEnabled() || cfg.Proxy.Enabled || !cfg.FolderPerSplitRecording || cfg.FolderPerStreamer || cfg.SplitSizeUnit != "GB" {
		t.Fatal("existing configuration changed behavior")
	}
}
