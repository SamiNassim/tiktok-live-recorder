package state

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func historyFixture(t *testing.T) (*Store, []Recording) {
	t.Helper()
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	now := time.Now().UTC()
	recordings := []Recording{
		{ID: "first", Path: filepath.Join(root, "first.flv"), ProcessedPath: filepath.Join(root, "first.mkv"), StartedAt: now.Add(-time.Hour), EndedAt: now},
		{ID: "second", Path: filepath.Join(root, "second.flv"), StartedAt: now.Add(-time.Hour), EndedAt: now},
		{ID: "active", Path: filepath.Join(root, "active.flv"), StartedAt: now},
	}
	for _, r := range recordings {
		for _, path := range []string{r.Path, r.ProcessedPath} {
			if path != "" {
				if err := os.WriteFile(path, []byte("original media"), 0600); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	if err := s.Update(func(d *Data) error {
		d.Recordings = recordings
		d.Streamers = []Streamer{{ID: "demo", LastRecordedAt: now}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return s, recordings
}

func assertHistoryFilesKept(t *testing.T, recordings []Recording) {
	t.Helper()
	for _, r := range recordings {
		for _, path := range []string{r.Path, r.ProcessedPath} {
			if path != "" {
				if data, err := os.ReadFile(path); err != nil || string(data) != "original media" {
					t.Fatalf("clearing history changed media %q: %q, %v", r.ID, data, err)
				}
			}
		}
	}
}

func TestRemoveRecordingOnlyForgetsSelectedFinalizedEntry(t *testing.T) {
	s, recordings := historyFixture(t)
	before := s.Data()
	if err := s.RemoveRecording("first"); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(s.Directory())
	if err != nil {
		t.Fatal(err)
	}
	got := reopened.Data()
	if !reflect.DeepEqual(got.Recordings, recordings[1:]) || !reflect.DeepEqual(got.Streamers, before.Streamers) {
		t.Fatal("wrong history entry or streamer metadata removed", got)
	}
	assertHistoryFilesKept(t, recordings)
	if n, err := reopened.PruneMissingRecordings(); err != nil || n != 0 || len(reopened.Data().Recordings) != 2 {
		t.Fatal("reconciliation changed cleared history", n, err)
	}
}

func TestRemoveRecordingRejectsActiveAndMissingEntries(t *testing.T) {
	s, recordings := historyFixture(t)
	before := s.Data()
	for _, id := range []string{"active", "missing", ""} {
		if err := s.RemoveRecording(id); err == nil {
			t.Fatal("accepted removal", id)
		}
		if !reflect.DeepEqual(before, s.Data()) {
			t.Fatal("rejected removal changed history", id)
		}
	}
	assertHistoryFilesKept(t, recordings)
}

func TestClearRecordingHistoryKeepsActiveFilesAndStreamerMetadata(t *testing.T) {
	s, recordings := historyFixture(t)
	before := s.Data()
	if n, err := s.ClearRecordingHistory(); err != nil || n != 2 {
		t.Fatal(n, err)
	}
	reopened, err := Open(s.Directory())
	if err != nil {
		t.Fatal(err)
	}
	got := reopened.Data()
	if !reflect.DeepEqual(got.Recordings, recordings[2:]) || !reflect.DeepEqual(got.Streamers, before.Streamers) {
		t.Fatal("active recording or streamer metadata was lost", got)
	}
	if n, err := reopened.ClearRecordingHistory(); err != nil || n != 0 {
		t.Fatal("clearing active-only history was not a no-op", n, err)
	}
	assertHistoryFilesKept(t, recordings)
	// The recorder can still finalize the preserved active entry normally.
	if err := reopened.Update(func(d *Data) error { d.Recordings[0].EndedAt = time.Now(); return nil }); err != nil {
		t.Fatal(err)
	}
	if n, err := reopened.ClearRecordingHistory(); err != nil || n != 1 || len(reopened.Data().Recordings) != 0 {
		t.Fatal("finalized entry could not be cleared", n, err)
	}
	assertHistoryFilesKept(t, recordings)
}

func TestHistoryRemovalPreservesMemoryAndMediaWhenSaveFails(t *testing.T) {
	s, recordings := historyFixture(t)
	before := s.Data()
	name := filepath.Join(s.Directory(), "data.json")
	if err := os.Rename(name, filepath.Join(s.Directory(), "saved-data.json")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(name, 0700); err != nil {
		t.Fatal(err)
	}
	if err := s.RemoveRecording("first"); err == nil {
		t.Fatal("ignored save failure")
	}
	if n, err := s.ClearRecordingHistory(); err == nil || n != 0 {
		t.Fatal("reported successful clear despite save failure", n, err)
	}
	if !reflect.DeepEqual(before, s.Data()) {
		t.Fatal("failed save changed in-memory history")
	}
	assertHistoryFilesKept(t, recordings)
}
