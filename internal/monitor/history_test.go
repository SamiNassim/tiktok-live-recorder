package monitor

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"tiktok-live-recorder/internal/logging"
	"tiktok-live-recorder/internal/state"
	"tiktok-live-recorder/internal/tiktok"
)

func TestProcessingCompletionDoesNotRestoreClearedHistory(t *testing.T) {
	s, log := fixture(t)
	cfg := s.Config()
	cfg.PollingEnabled = false
	if err := s.SaveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	raw, processed := filepath.Join(t.TempDir(), "raw.flv"), filepath.Join(t.TempDir(), "copy.mkv")
	for _, path := range []string{raw, processed} {
		if err := os.WriteFile(path, []byte("original media"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Update(func(d *state.Data) error {
		d.Recordings = []state.Recording{{ID: "done", Path: raw, EndedAt: time.Now()}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	e := newEngine(s, log, &fakeClient{}, func(context.Context, state.Streamer, tiktok.Stream, state.Config) (activeRecording, error) {
		t.Error("recording started while monitoring was disabled")
		return nil, nil
	})
	defer e.Close()
	started, release := make(chan struct{}), make(chan struct{})
	e.mu.Lock()
	e.process = func(ctx context.Context, _ string, _ state.Config) (string, error) {
		close(started)
		select {
		case <-release:
			return processed, nil
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	e.processCopy("demo_creator", raw, cfg)
	e.mu.Unlock()
	select {
	case <-started:
	case <-time.After(10 * time.Second):
		t.Fatal("processing did not start")
	}
	if n, err := s.ClearRecordingHistory(); err != nil || n != 1 {
		t.Fatal(n, err)
	}
	close(release)
	waitFor(t, func() bool {
		return slices.ContainsFunc(log.Entries(), func(entry logging.Entry) bool {
			return entry.Message == "Processed copy ready; raw FLV kept"
		})
	})
	if len(s.Data().Recordings) != 0 {
		t.Fatal("processing restored an entry removed from history")
	}
	for _, path := range []string{raw, processed} {
		if data, err := os.ReadFile(path); err != nil || string(data) != "original media" {
			t.Fatal("clearing history affected raw or processed media", err)
		}
	}
}
