package monitor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"tiktok-live-recorder/internal/fault"
	"tiktok-live-recorder/internal/recording"
	"tiktok-live-recorder/internal/state"
)

func TestProcessingFailuresSurviveFastRetriesAndFullRecordingEvents(t *testing.T) {
	s, log := fixture(t)
	raw := filepath.Join(t.TempDir(), "source.flv")
	const original = "synthetic original media"
	if err := os.WriteFile(raw, []byte(original), 0600); err != nil {
		t.Fatal(err)
	}
	row := state.Recording{ID: "done", StreamerID: "demo_creator", Path: raw, EndedAt: time.Now()}
	if err := s.Update(func(d *state.Data) error { d.Recordings = []state.Recording{row}; return nil }); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	e := &Engine{store: s, log: log, ctx: ctx, cancel: cancel, events: make(chan Event, 1), processing: make(chan struct{}, 2)}
	t.Cleanup(e.Close)
	e.events <- Event{Name: "Synthetic recording"}
	const reason = "The recording contains damaged or incomplete media data; raw FLV kept"
	e.process = func(context.Context, string, state.Config, func(recording.Progress)) (recording.ProcessedCopy, error) {
		return recording.ProcessedCopy{}, fault.Action(reason, errors.New("synthetic decoder failure"))
	}
	before := e.Snapshot()
	for range 2 {
		if err := e.RetryProcessing(row.ID); err != nil {
			t.Fatal(err)
		}
		waitFor(t, func() bool { return !e.IsProcessing(raw) })
	}
	if _, changed := e.SnapshotIfChanged(before); !changed {
		t.Fatal("completed failures did not request a UI refresh")
	}
	failures := e.TakeProcessingFailures()
	if len(failures) != 2 {
		t.Fatal("fast or repeated failure was lost", failures)
	}
	for _, failure := range failures {
		if failure.StreamerID != row.StreamerID || failure.Path != raw || failure.Message != reason {
			t.Fatal("failure lost its recording or readable reason", failure)
		}
	}
	if len(e.TakeProcessingFailures()) != 0 {
		t.Fatal("a failure was delivered twice")
	}
	if s.Data().Recordings[0].Error != reason || s.Data().Recordings[0].ProcessedPath != "" {
		t.Fatal("failed conversion was not retained in history")
	}
	logged := 0
	for _, entry := range log.Entries() {
		if entry.Message == "Processed copy failed: "+reason {
			if entry.Level != "ERROR" || entry.Streamer != row.StreamerID || !strings.Contains(entry.Details, "synthetic decoder failure") {
				t.Fatal("manual failure log lost its context or diagnostic cause", entry)
			}
			logged++
		}
	}
	if logged != 2 {
		t.Fatal("each manual failure must log the same clear reason as the toast", logged)
	}
	if data, err := os.ReadFile(raw); err != nil || string(data) != original {
		t.Fatal("failed retry changed the original", err)
	}
}

func TestRetryProcessingUsesBoundedQueueAndCancelsQueuedJobs(t *testing.T) {
	s, log := fixture(t)
	cfg := s.Config()
	cfg.PollingEnabled, cfg.PostProcess = false, false
	if err := s.SaveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	var rows []state.Recording
	for i := range 3 {
		path := filepath.Join(t.TempDir(), "source.flv")
		if err := os.WriteFile(path, []byte("synthetic original media"), 0600); err != nil {
			t.Fatal(err)
		}
		rows = append(rows, state.Recording{ID: fmt.Sprintf("recording-%d", i), StreamerID: "demo_creator", Path: path, EndedAt: time.Now()})
	}
	if err := s.Update(func(d *state.Data) error { d.Recordings = rows; return nil }); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	e := &Engine{store: s, log: log, ctx: ctx, cancel: cancel, processing: make(chan struct{}, 2)}
	t.Cleanup(e.Close)
	started := make(chan string, 3)
	e.process = func(ctx context.Context, path string, _ state.Config, _ func(recording.Progress)) (recording.ProcessedCopy, error) {
		started <- path
		<-ctx.Done()
		return recording.ProcessedCopy{}, ctx.Err()
	}
	before := e.Snapshot()
	for _, r := range rows {
		if err := e.RetryProcessing(r.ID); err != nil {
			t.Fatal(err)
		}
		if !e.IsProcessing(r.Path) || e.RetryProcessing(r.ID) == nil {
			t.Fatal("duplicate conversion was accepted")
		}
	}
	if _, changed := e.SnapshotIfChanged(before); !changed {
		t.Fatal("queued conversions did not invalidate recording status")
	}
	for range 2 {
		select {
		case <-started:
		case <-time.After(10 * time.Second):
			t.Fatal("converter did not start")
		}
	}
	select {
	case <-started:
		t.Fatal("conversion concurrency exceeded two")
	default:
	}
	before = e.Snapshot()
	e.Close()
	if _, changed := e.SnapshotIfChanged(before); !changed {
		t.Fatal("cancellation did not invalidate conversion status")
	}
	select {
	case <-started:
		t.Fatal("queued conversion started after shutdown")
	default:
	}
	for _, r := range rows {
		if e.IsProcessing(r.Path) || e.RetryProcessing(r.ID) == nil {
			t.Fatal("shutdown left a conversion active or accepted a new one")
		}
		if got, err := os.ReadFile(r.Path); err != nil || string(got) != "synthetic original media" {
			t.Fatal("cancellation changed an original FLV", err)
		}
	}
}

func TestRetryProcessingValidatesHistoryAndCanRecoverAfterFailure(t *testing.T) {
	s, log := fixture(t)
	raw := filepath.Join(t.TempDir(), "source.flv")
	if err := os.WriteFile(raw, []byte("synthetic original media"), 0600); err != nil {
		t.Fatal(err)
	}
	row := state.Recording{ID: "done", StreamerID: "demo_creator", Path: raw, EndedAt: time.Now()}
	ctx, cancel := context.WithCancel(context.Background())
	e := &Engine{store: s, log: log, ctx: ctx, cancel: cancel, processing: make(chan struct{}, 2)}
	t.Cleanup(e.Close)
	if err := e.RetryProcessing(row.ID); err == nil {
		t.Fatal("missing history entry was accepted")
	}
	for _, invalid := range []state.Recording{
		{ID: row.ID, Path: raw},
		{ID: row.ID, Path: raw, EndedAt: row.EndedAt, RawDeleted: true},
		{ID: row.ID, EndedAt: row.EndedAt},
		{ID: row.ID, Path: filepath.Join(t.TempDir(), "missing.flv"), EndedAt: row.EndedAt},
	} {
		if err := s.Update(func(d *state.Data) error { d.Recordings = []state.Recording{invalid}; return nil }); err != nil {
			t.Fatal(err)
		}
		if err := e.RetryProcessing(row.ID); err == nil {
			t.Fatal("invalid recording was accepted")
		}
	}
	if err := s.Update(func(d *state.Data) error { d.Recordings = []state.Recording{row}; return nil }); err != nil {
		t.Fatal(err)
	}
	attempt := 0
	processed := filepath.Join(filepath.Dir(raw), "source.processed.mp4")
	e.process = func(context.Context, string, state.Config, func(recording.Progress)) (recording.ProcessedCopy, error) {
		attempt++
		if attempt == 1 {
			return recording.ProcessedCopy{}, errors.New("synthetic conversion failure")
		}
		if err := os.WriteFile(processed, []byte("synthetic verified MP4"), 0600); err != nil {
			return recording.ProcessedCopy{}, err
		}
		return recording.ProcessedCopy{Path: processed, Reencoded: true}, nil
	}
	for range 2 {
		if err := e.RetryProcessing(row.ID); err != nil {
			t.Fatal(err)
		}
		waitFor(t, func() bool { return !e.IsProcessing(raw) })
	}
	if s.Data().Recordings[0].ProcessedPath != processed {
		t.Fatal("retry did not save the copy")
	}
	second := filepath.Join(filepath.Dir(raw), "source.processed-2.mp4")
	e.process = func(context.Context, string, state.Config, func(recording.Progress)) (recording.ProcessedCopy, error) {
		if err := os.WriteFile(second, []byte("another synthetic verified MP4"), 0600); err != nil {
			return recording.ProcessedCopy{}, err
		}
		return recording.ProcessedCopy{Path: second, Reencoded: true}, nil
	}
	if err := e.RetryProcessing(row.ID); err != nil {
		t.Fatal("completed copy could not be converted again", err)
	}
	waitFor(t, func() bool { return !e.IsProcessing(raw) })
	if s.Data().Recordings[0].ProcessedPath != second {
		t.Fatal("reconversion did not update the playback copy")
	}
	if got, err := os.ReadFile(processed); err != nil || string(got) != "synthetic verified MP4" {
		t.Fatal("reconversion changed the previous copy", err)
	}
	if got, err := os.ReadFile(raw); err != nil || string(got) != "synthetic original media" {
		t.Fatal("retry changed the original FLV", err)
	}
}

func TestRawCleanupRechecksFilesAndCancellation(t *testing.T) {
	for _, name := range []string{"success", "canceled", "raw replaced", "raw edited", "copy replaced", "copy edited", "copy missing", "empty copy", "same file", "raw symlink", "copy symlink", "not FLV"} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			raw, processed := filepath.Join(root, "source.flv"), filepath.Join(root, "copy.mkv")
			if name == "not FLV" {
				raw = filepath.Join(root, "source.mkv")
			}
			for _, path := range []string{raw, processed} {
				if err := os.WriteFile(path, []byte("original media"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if name == "same file" {
				if err := os.Remove(processed); err != nil {
					t.Fatal(err)
				}
				if err := os.Link(raw, processed); err != nil {
					t.Skip("hard links unavailable", err)
				}
			}
			if name == "raw symlink" || name == "copy symlink" {
				path := raw
				if name == "copy symlink" {
					path = processed
				}
				if err := os.Rename(path, path+".saved"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(path+".saved", path); err != nil {
					t.Skip("symlinks unavailable", err)
				}
			}
			original, err := snapshotFile(raw)
			if err != nil {
				t.Fatal(err)
			}
			copyInfo, err := snapshotFile(processed)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			switch name {
			case "canceled":
				cancel()
			case "raw replaced", "copy replaced":
				path := raw
				if name == "copy replaced" {
					path = processed
				}
				if err := os.Rename(path, path+".saved"); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte("original media"), 0600); err != nil {
					t.Fatal(err)
				}
				before := original
				if name == "copy replaced" {
					before = copyInfo
				}
				// Equal timestamps and sizes must not hide a different file ID.
				if err := os.Chtimes(path, before.ModTime(), before.ModTime()); err != nil {
					t.Fatal(err)
				}
			case "raw edited", "copy edited", "empty copy":
				path, data := raw, []byte("changed media payload")
				if name != "raw edited" {
					path = processed
				}
				if name == "empty copy" {
					data = nil
				}
				if err := os.WriteFile(path, data, 0600); err != nil {
					t.Fatal(err)
				}
			case "copy missing":
				if err := os.Remove(processed); err != nil {
					t.Fatal(err)
				}
			}
			err = removeProcessedRaw(ctx, raw, processed, original, copyInfo)
			if name == "success" {
				if err != nil {
					t.Fatal(err)
				}
				if _, err := os.Lstat(raw); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("original was not removed", err)
				}
				if b, err := os.ReadFile(processed); err != nil || string(b) != "original media" {
					t.Fatal("processed copy changed", err)
				}
			} else if err == nil {
				t.Fatal("unsafe cleanup was accepted")
			} else if _, err := os.Lstat(raw); err != nil {
				t.Fatal("unsafe cleanup removed the original", err)
			}
		})
	}
}

func TestConversionProgressSnapshotsAndIndividualCancellation(t *testing.T) {
	s, log := fixture(t)
	var rows []state.Recording
	for i := range 3 {
		raw := filepath.Join(t.TempDir(), "source.flv")
		if err := os.WriteFile(raw, []byte("synthetic original media"), 0600); err != nil {
			t.Fatal(err)
		}
		now := time.Now()
		rows = append(rows, state.Recording{ID: fmt.Sprintf("done-%d", i), Path: raw, StartedAt: now.Add(-100 * time.Second), EndedAt: now})
	}
	previous := filepath.Join(t.TempDir(), "previous.mp4")
	if err := os.WriteFile(previous, []byte("previous verified copy"), 0600); err != nil {
		t.Fatal(err)
	}
	rows[0].ProcessedPath = previous
	if err := s.Update(func(d *state.Data) error { d.Recordings = rows; return nil }); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	e := &Engine{store: s, log: log, ctx: ctx, cancel: cancel, processing: make(chan struct{}, 2)}
	t.Cleanup(e.Close)
	started := make(chan string, 3)
	e.process = func(ctx context.Context, source string, _ state.Config, report func(recording.Progress)) (recording.ProcessedCopy, error) {
		report(recording.Progress{Phase: "Encoding MP4", MediaTime: 42 * time.Second, Speed: 1.25})
		started <- source
		<-ctx.Done()
		return recording.ProcessedCopy{}, ctx.Err()
	}
	for _, r := range rows[:2] {
		if err := e.RetryProcessing(r.ID); err != nil {
			t.Fatal(err)
		}
		select {
		case <-started:
		case <-time.After(10 * time.Second):
			t.Fatal("conversion did not start")
		}
	}
	before := e.Snapshot()
	job := before.Processing[rows[0].Path]
	if job.Queued || job.StartedAt.IsZero() || job.Progress.MediaTime != 42*time.Second || job.Progress.Speed != 1.25 {
		t.Fatal("live progress missing from snapshot", job)
	}
	if job.Pass != 1 || job.Passes != 2 || job.Percent != 42 || job.Overall != 21 {
		t.Fatal("pass and overall percentages missing from snapshot", job)
	}
	if err := e.RetryProcessing(rows[2].ID); err != nil {
		t.Fatal(err)
	}
	if !e.Snapshot().Processing[rows[2].Path].Queued {
		t.Fatal("queued conversion was not identified")
	}
	if err := e.CancelProcessing(rows[2].ID); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return !e.IsProcessing(rows[2].Path) })
	select {
	case <-started:
		t.Fatal("canceled queued job started")
	default:
	}
	if err := e.CancelProcessing(rows[0].ID); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return !e.IsProcessing(rows[0].Path) })
	if len(e.TakeProcessingFailures()) != 0 {
		t.Fatal("an explicitly canceled conversion was reported as a failure")
	}
	if !e.IsProcessing(rows[1].Path) || e.ctx.Err() != nil {
		t.Fatal("canceling one conversion affected another job or the engine")
	}
	if before.Processing[rows[0].Path].Progress.Phase != "Encoding MP4" {
		t.Fatal("an earlier progress snapshot was mutated")
	}
	if s.Data().Recordings[0].ProcessedPath != previous {
		t.Fatal("cancellation changed the previous playback copy")
	}
	if got, err := os.ReadFile(previous); err != nil || string(got) != "previous verified copy" {
		t.Fatal("cancellation changed the previous file", err)
	}
	for _, r := range rows {
		if got, err := os.ReadFile(r.Path); err != nil || string(got) != "synthetic original media" {
			t.Fatal("cancellation changed an original FLV", err)
		}
	}
}

func TestProcessingCleanupHonorsErrorsAndSettingsChanges(t *testing.T) {
	for _, name := range []string{"success", "re-encoded", "default keeps raw", "failed", "canceled", "cleanup disabled", "processing disabled", "history cleared", "raw replaced", "empty copy"} {
		t.Run(name, func(t *testing.T) {
			s, log := fixture(t)
			cfg := s.Config()
			cfg.PollingEnabled = false
			cfg.DeleteRawAfterProcessing = name != "default keeps raw"
			if err := s.SaveConfig(cfg); err != nil {
				t.Fatal(err)
			}
			root := t.TempDir()
			raw, processed := filepath.Join(root, "source.flv"), filepath.Join(root, "copy.mkv")
			if err := os.WriteFile(raw, []byte("original media"), 0600); err != nil {
				t.Fatal(err)
			}
			if err := s.Update(func(d *state.Data) error {
				d.Recordings = []state.Recording{{ID: "done", Path: raw, EndedAt: time.Now()}}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			e := &Engine{store: s, log: log, ctx: ctx, cancel: cancel, processing: make(chan struct{}, 2)}
			t.Cleanup(e.Close)
			e.process = func(context.Context, string, state.Config, func(recording.Progress)) (recording.ProcessedCopy, error) {
				if name == "failed" {
					return recording.ProcessedCopy{}, errors.New("processing failed")
				}
				payload := []byte("verified copy")
				if name == "empty copy" {
					payload = nil
				}
				if err := os.WriteFile(processed, payload, 0600); err != nil {
					return recording.ProcessedCopy{}, err
				}
				switch name {
				case "canceled":
					cancel()
				case "cleanup disabled", "processing disabled":
					latest := s.Config()
					if name == "cleanup disabled" {
						latest.DeleteRawAfterProcessing = false
					} else {
						latest.PostProcess = false
					}
					if err := s.SaveConfig(latest); err != nil {
						return recording.ProcessedCopy{}, err
					}
				case "history cleared":
					if _, err := s.ClearRecordingHistory(); err != nil {
						return recording.ProcessedCopy{}, err
					}
				case "raw replaced":
					before, err := os.Stat(raw)
					if err != nil {
						return recording.ProcessedCopy{}, err
					}
					if err := os.Rename(raw, raw+".saved"); err != nil {
						return recording.ProcessedCopy{}, err
					}
					if err := os.WriteFile(raw, []byte("original media"), 0600); err != nil {
						return recording.ProcessedCopy{}, err
					}
					if err := os.Chtimes(raw, before.ModTime(), before.ModTime()); err != nil {
						return recording.ProcessedCopy{}, err
					}
				}
				return recording.ProcessedCopy{Path: processed, Reencoded: name == "re-encoded"}, nil
			}
			e.processCopy("demo_creator", raw, cfg)
			done := make(chan struct{})
			go func() { e.wg.Wait(); close(done) }()
			select {
			case <-done:
			case <-time.After(10 * time.Second):
				t.Fatal("processing did not finish")
			}
			_, err := os.Stat(raw)
			if name == "success" {
				if !errors.Is(err, os.ErrNotExist) || !s.Data().Recordings[0].RawDeleted {
					t.Fatal("successful cleanup was not recorded", err)
				}
				reopened, err := state.Open(s.Directory())
				if err != nil {
					t.Fatal(err)
				}
				if n, err := reopened.PruneMissingRecordings(); err != nil || n != 0 || len(reopened.Data().Recordings) != 1 {
					t.Fatal("cleanup lost the recording from history", n, err)
				}
			} else if err != nil {
				t.Fatal("original was lost after unsuccessful or disabled cleanup", err)
			}
			if name == "history cleared" && len(s.Data().Recordings) != 0 {
				t.Fatal("processing restored cleared history")
			}
			if name == "empty copy" && s.Data().Recordings[0].ProcessedPath != "" {
				t.Fatal("an empty copy replaced the raw file as the playback path")
			}
		})
	}
}
