package state

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func enableRawCleanup(t *testing.T, s *Store) {
	t.Helper()
	cfg := s.Config()
	cfg.PostProcess, cfg.DeleteRawAfterProcessing = true, true
	if err := s.SaveConfig(cfg); err != nil {
		t.Fatal(err)
	}
}

func TestCompleteProcessingPersistsCopyBeforeDeletingRaw(t *testing.T) {
	s, recordings := historyFixture(t)
	enableRawCleanup(t, s)
	r := recordings[0]
	if err := s.Update(func(d *Data) error { d.Recordings[0].ProcessedPath = ""; return nil }); err != nil {
		t.Fatal(err)
	}
	deleted, err := s.CompleteProcessing(r.Path, r.ProcessedPath, true, func() error {
		b, err := os.ReadFile(filepath.Join(s.Directory(), "data.json"))
		if err != nil {
			t.Fatal(err)
		}
		var saved Data
		if err := json.Unmarshal(b, &saved); err != nil {
			t.Fatal(err)
		}
		if saved.Recordings[0].ProcessedPath != r.ProcessedPath || saved.Recordings[0].RawDeleted {
			t.Fatal("cleanup started before copy metadata was saved")
		}
		return os.Remove(r.Path)
	})
	if err != nil || !deleted {
		t.Fatal("cleanup failed", deleted, err)
	}
	reopened, err := Open(s.Directory())
	if err != nil {
		t.Fatal(err)
	}
	if got := reopened.Data().Recordings[0]; !got.RawDeleted || got.Path != r.Path || got.ProcessedPath != r.ProcessedPath {
		t.Fatal("cleanup metadata was lost", got)
	}
	if n, err := reopened.PruneMissingRecordings(); err != nil || n != 0 {
		t.Fatal("cleanup removed surviving playback copy from history", n, err)
	}
	if b, err := os.ReadFile(r.ProcessedPath); err != nil || string(b) != "original media" {
		t.Fatal("cleanup changed the processed copy", err)
	}
	if err := os.Remove(r.ProcessedPath); err != nil {
		t.Fatal(err)
	}
	if n, err := reopened.PruneMissingRecordings(); err != nil || n != 1 {
		t.Fatal("history retained a recording with both files deleted", n, err)
	}
}

func TestCompleteProcessingRequiresCurrentOptInAndFinalizedHistory(t *testing.T) {
	for _, name := range []string{"default", "not requested", "disabled", "processing disabled", "cleared history", "active recording"} {
		t.Run(name, func(t *testing.T) {
			s, recordings := historyFixture(t)
			r := recordings[0]
			if name != "default" {
				enableRawCleanup(t, s)
			}
			switch name {
			case "disabled", "processing disabled":
				cfg := s.Config()
				if name == "disabled" {
					cfg.DeleteRawAfterProcessing = false
				} else {
					cfg.PostProcess = false
				}
				if err := s.SaveConfig(cfg); err != nil {
					t.Fatal(err)
				}
			case "cleared history":
				if err := s.RemoveRecording(r.ID); err != nil {
					t.Fatal(err)
				}
			case "active recording":
				r = recordings[2]
				r.ProcessedPath = filepath.Join(filepath.Dir(r.Path), "active.mkv")
			}
			called := false
			deleted, err := s.CompleteProcessing(r.Path, r.ProcessedPath, name != "not requested", func() error { called = true; return nil })
			if deleted || called || (err != nil && name != "active recording") {
				t.Fatal("cleanup bypassed opt-in or history checks", deleted, called, err)
			}
			if _, err := os.Stat(r.Path); err != nil {
				t.Fatal("original file was lost", err)
			}
		})
	}
}

func TestCompleteProcessingKeepsRawWhenMetadataCannotBeSaved(t *testing.T) {
	s, recordings := historyFixture(t)
	enableRawCleanup(t, s)
	name := filepath.Join(s.Directory(), "data.json")
	if err := os.Rename(name, name+".saved"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(name, 0700); err != nil {
		t.Fatal(err)
	}
	called := false
	r := recordings[0]
	deleted, err := s.CompleteProcessing(r.Path, r.ProcessedPath, true, func() error { called = true; return nil })
	if err == nil || deleted || called {
		t.Fatal("cleanup ran despite a history save failure", deleted, called, err)
	}
	assertHistoryFilesKept(t, recordings)
}

func TestCompleteProcessingReportsFailedRemovalWithoutLosingCopy(t *testing.T) {
	s, recordings := historyFixture(t)
	enableRawCleanup(t, s)
	r := recordings[0]
	cause := errors.New("file is busy")
	deleted, err := s.CompleteProcessing(r.Path, r.ProcessedPath, true, func() error { return cause })
	if deleted || !errors.Is(err, cause) || s.Data().Recordings[0].RawDeleted {
		t.Fatal("failed cleanup was reported as successful", deleted, err)
	}
	assertHistoryFilesKept(t, recordings)
}

func TestCompleteProcessingReportsDeletionEvenIfCleanupStatusSaveFails(t *testing.T) {
	s, recordings := historyFixture(t)
	enableRawCleanup(t, s)
	r := recordings[0]
	name := filepath.Join(s.Directory(), "data.json")
	deleted, err := s.CompleteProcessing(r.Path, r.ProcessedPath, true, func() error {
		if err := os.Remove(r.Path); err != nil {
			return err
		}
		if err := os.Rename(name, name+".saved"); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(name, 0700); err != nil {
			t.Fatal(err)
		}
		return nil
	})
	if !deleted || err == nil || !s.Data().Recordings[0].RawDeleted {
		t.Fatal("cleanup status hid an actual deletion", deleted, err)
	}
	if _, err := os.Stat(r.ProcessedPath); err != nil {
		t.Fatal("surviving copy was lost", err)
	}
}
