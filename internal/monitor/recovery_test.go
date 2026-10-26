package monitor

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"tiktok-live-recorder/internal/recording"
	"tiktok-live-recorder/internal/state"
	"tiktok-live-recorder/internal/tiktok"
)

func recoveryFixture(t *testing.T) (*Engine, *item, *fakeClient) {
	t.Helper()
	s, log := fixture(t)
	cfg := s.Config()
	cfg.PostProcess = false
	if err := s.SaveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	h := &fakeRecording{done: make(chan struct{}), started: time.Now().Add(-time.Minute), path: filepath.Join(root, "first.flv")}
	h.Stop() // Simulate the media body ending, rather than a user pause.
	it := &item{row: Row{Streamer: s.Data().Streamers[0], Status: "Recording", Live: true}, handle: h, config: cfg}
	if err := s.Update(func(d *state.Data) error {
		d.Recordings = []state.Recording{{ID: "first", StreamerID: it.row.Streamer.ID, Path: h.File(), StartedAt: h.Started()}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	c := &fakeClient{}
	ctx, cancel := context.WithCancel(context.Background())
	e := &Engine{store: s, log: log, client: c, ctx: ctx, cancel: cancel, items: map[string]*item{it.row.Streamer.ID: it}, wake: make(chan struct{}, 1), events: make(chan Event, 1)}
	e.start = func(context.Context, state.Streamer, tiktok.Stream, state.Config) (activeRecording, error) {
		return &fakeRecording{done: make(chan struct{}), started: time.Now(), path: filepath.Join(root, "resumed.flv")}, nil
	}
	t.Cleanup(func() {
		cancel()
		if it.handle != nil {
			it.handle.Stop()
		}
	})
	return e, it, c
}

func TestMediaInterruptionRechecksBrieflyAndResumesWithoutPollingRecording(t *testing.T) {
	e, it, c := recoveryFixture(t)
	e.completed(it)
	if remaining := time.Until(it.recoveryUntil); remaining < recoveryWindow-time.Second || remaining > recoveryWindow {
		t.Fatal("media closure did not start the bounded recovery window", remaining)
	}
	id := it.row.Streamer.ID
	e.check(id)
	if it.row.Status != "Reconnecting" || it.row.Error != "" || it.row.Live || it.row.NextCheck.Sub(it.row.LastCheck) != recoveryInterval {
		t.Fatal("temporary unavailability was treated as an error or a normal offline poll", it.row)
	}
	entries := len(e.log.Entries())
	e.check(id)
	if len(e.log.Entries()) != entries {
		t.Fatal("each recovery check repeated the same informational log")
	}
	for _, entry := range e.log.Entries() {
		if entry.Level == "WARN" || entry.Level == "ERROR" {
			t.Fatal("an ordinary media interruption produced an access warning", entry.Message)
		}
	}
	c.live.Store(true)
	e.check(id)
	if it.row.Status != "Recording" || it.handle == nil || !it.recoveryUntil.IsZero() || !it.row.NextCheck.IsZero() {
		t.Fatal("resumed stream did not leave recovery and start recording", it.row)
	}
	if history := e.store.Data().Recordings; len(history) != 2 || history[0].EndedAt.IsZero() || history[1].Path == history[0].Path {
		t.Fatal("recovery did not keep separate raw files", history)
	}
	calls := c.calls.Load()
	it.row.NextCheck = time.Now().Add(-time.Hour)
	if e.next() != "" {
		t.Fatal("resumed recording reentered the polling queue")
	}
	e.check(id)
	if c.calls.Load() != calls {
		t.Fatal("active recording continued making recovery requests")
	}
}

func TestRecoveryDeadlineReturnsToConfiguredPolling(t *testing.T) {
	e, it, _ := recoveryFixture(t)
	e.completed(it)
	deadline := time.Now().Add(time.Second)
	it.recoveryUntil = deadline
	e.check(it.row.Streamer.ID)
	if !it.row.NextCheck.Equal(deadline) {
		t.Fatal("recovery scheduled a check beyond its deadline", it.row.NextCheck, deadline)
	}
	it.recoveryUntil = time.Now().Add(-time.Second)
	e.check(it.row.Streamer.ID)
	if it.row.Status != "Offline" || !it.recoveryUntil.IsZero() || it.row.NextCheck.Sub(it.row.LastCheck) != interval(e.store.Config(), false) {
		t.Fatal("recovery continued beyond its bounded window", it.row)
	}
}

func TestRecoveryRespectsPausesAndDisabledPolling(t *testing.T) {
	for _, action := range []string{"pause recording", "pause monitoring", "disable polling"} {
		t.Run(action, func(t *testing.T) {
			e, it, c := recoveryFixture(t)
			e.completed(it)
			e.check(it.row.Streamer.ID)
			var err error
			switch action {
			case "pause recording":
				err = e.SetRecordingPaused(it.row.Streamer.ID, true)
			case "pause monitoring":
				err = e.SetMonitoringPaused(it.row.Streamer.ID, true)
			case "disable polling":
				cfg := e.store.Config()
				cfg.PollingEnabled = false
				err = e.SaveConfig(cfg)
			}
			if err != nil || !it.recoveryUntil.IsZero() {
				t.Fatal("control did not cancel fast recovery", err)
			}
			if action == "pause recording" {
				e.check(it.row.Streamer.ID)
				if it.row.Status != "Offline" || it.row.NextCheck.Sub(it.row.LastCheck) != interval(e.store.Config(), false) {
					t.Fatal("recording pause still used rapid recovery checks")
				}
			} else if e.next() != "" || c.calls.Load() != 1 {
				t.Fatal("disabled monitoring continued recovery requests")
			}
		})
	}
}

func TestRecoveryPreservesAccessErrorsAndRateLimitBackoff(t *testing.T) {
	for _, cause := range []error{tiktok.ErrAccess, tiktok.ErrRateLimited, tiktok.ErrBlocked} {
		t.Run(cause.Error(), func(t *testing.T) {
			e, it, c := recoveryFixture(t)
			e.completed(it)
			c.err = cause
			e.check(it.row.Streamer.ID)
			if it.row.Status != "Needs attention" || it.row.Error != cause.Error() || !it.recoveryUntil.IsZero() {
				t.Fatal("recovery hid a real API failure", it.row)
			}
			minimum := interval(e.store.Config(), false)
			if errors.Is(cause, tiktok.ErrRateLimited) {
				minimum = max(minimum, 5*time.Minute)
			}
			if it.row.NextCheck.Sub(it.row.LastCheck) < minimum {
				t.Fatal("fast recovery bypassed error backoff", it.row.NextCheck)
			}
		})
	}
}

func TestManualStopsAndStorageFailuresDoNotStartFastRecovery(t *testing.T) {
	for _, name := range []string{"manual stop", "empty file", "storage failure", "shutdown"} {
		t.Run(name, func(t *testing.T) {
			e, it, _ := recoveryFixture(t)
			switch name {
			case "manual stop":
				it.row.Status = "Finalizing"
			case "empty file", "storage failure":
				h := it.handle.(*fakeRecording)
				result := recording.Result{Path: h.File(), EndedAt: time.Now()}
				if name == "storage failure" {
					result.Bytes = 100
					result.Err = errors.New("disk sync failed")
				}
				it.handle = &endedRecording{fakeRecording: h, result: result}
			case "shutdown":
				e.cancel()
			}
			e.completed(it)
			if !it.recoveryUntil.IsZero() {
				t.Fatal("a non-media failure or manual stop started rapid checks")
			}
		})
	}
}
