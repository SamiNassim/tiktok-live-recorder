package monitor

import (
	"context"
	"os"
	"testing"
	"time"

	"tiktok-live-recorder/internal/fault"
	"tiktok-live-recorder/internal/recording"
	"tiktok-live-recorder/internal/state"
	"tiktok-live-recorder/internal/tiktok"
)

func TestAttentionSurvivesRetriesAndPreferenceChangesUntilCheckSucceeds(t *testing.T) {
	e, it, client := recoveryFixture(t)
	e.completed(it)
	id := it.row.Streamer.ID
	client.err = tiktok.ErrAccess
	e.check(id)
	warning := it.row.Error
	for _, change := range []func() error{
		func() error { return e.RetryCheck(id) },
		func() error { return e.Edit(id, func(s *state.Streamer) { s.LowPriority = true }) },
		func() error { return e.SetRecordingPaused(id, true) },
		func() error { return e.SetMonitoringPaused(id, true) },
		func() error { return e.SetMonitoringPaused(id, false) },
		func() error { return e.SaveConfig(e.store.Config()) },
	} {
		if err := change(); err != nil {
			t.Fatal(err)
		}
		if it.row.Error != warning || (!it.row.Streamer.Paused && it.row.Status != "Needs attention") {
			t.Fatal("an unrelated preference change or queued retry dismissed the warning", it.row)
		}
	}
	e.check(id)
	if it.row.Error != warning || it.row.Status != "Needs attention" {
		t.Fatal("a repeated failure dismissed the warning", it.row)
	}
	client.err = nil
	e.check(id)
	if it.row.Error != "" || it.row.Status != "Offline" {
		t.Fatal("a successful check did not resolve the discovery failure", it.row)
	}
}

func TestRecordingFolderIssueRequiresSuccessfulRecordingToClear(t *testing.T) {
	e, it, client := recoveryFixture(t)
	e.completed(it)
	id := it.row.Streamer.ID
	start := e.start
	client.live.Store(true)
	e.start = func(context.Context, state.Streamer, tiktok.Stream, state.Config) (activeRecording, error) {
		return nil, fault.Action("recording folder is not writable", os.ErrPermission)
	}
	e.check(id)
	warning := it.row.Error
	if it.row.Status != "Needs attention" || warning == "" {
		t.Fatal("recording failure did not require attention", it.row)
	}
	client.live.Store(false)
	e.check(id)
	if it.row.Error != warning || it.row.Status != "Needs attention" {
		t.Fatal("an offline response hid an unresolved folder failure", it.row)
	}
	if err := e.SetRecordingPaused(id, true); err != nil {
		t.Fatal(err)
	}
	client.live.Store(true)
	e.check(id)
	if it.row.Error != warning || it.row.Status != "Needs attention" || it.handle != nil {
		t.Fatal("a live response with recording paused hid an unresolved folder failure", it.row)
	}
	if err := e.SetRecordingPaused(id, false); err != nil {
		t.Fatal(err)
	}
	e.check(id)
	if it.row.Error != warning || it.row.Status != "Needs attention" {
		t.Fatal("a failed retry hid the recording issue", it.row)
	}
	e.start = start
	e.check(id)
	if it.row.Error != "" || it.recordingIssue != "" || it.row.Status != "Recording" {
		t.Fatal("a successful recording did not resolve the folder failure", it.row)
	}
}

func TestRetryCheckUsesQueueAndPreservesRateLimitBackoff(t *testing.T) {
	e, it, client := recoveryFixture(t)
	e.completed(it)
	id := it.row.Streamer.ID
	client.err = tiktok.ErrRateLimited
	e.check(id)
	backoff, warning, calls := it.row.NextCheck, it.row.Error, client.calls.Load()
	if err := e.RetryCheck(id); err != nil {
		t.Fatal(err)
	}
	if !it.row.NextCheck.Equal(backoff) || it.row.Error != warning || client.calls.Load() != calls || e.next() != "" {
		t.Fatal("retry bypassed the queue, backoff, or dismissed the warning", it.row)
	}
	if err := e.SaveConfig(e.store.Config()); err != nil {
		t.Fatal(err)
	}
	if !it.row.NextCheck.Equal(backoff) {
		t.Fatal("saving settings bypassed rate-limit backoff")
	}
	if err := e.Edit(id, func(s *state.Streamer) { s.LowPriority = true }); err != nil {
		t.Fatal(err)
	}
	if !it.row.NextCheck.Equal(backoff) {
		t.Fatal("changing priority bypassed rate-limit backoff")
	}
	client.err = nil
	e.check(id)
	it.row.NextCheck = time.Now().Add(time.Hour)
	if err := e.RetryCheck(id); err != nil {
		t.Fatal(err)
	}
	if e.next() != id || client.calls.Load() != calls+1 {
		t.Fatal("retry did not enqueue discovery without issuing its own request")
	}
}

func TestRetryCheckRejectsActivePausedAndMissingStreamers(t *testing.T) {
	for _, mode := range []string{"recording", "starting", "checking", "streamer paused", "monitoring disabled", "removed"} {
		t.Run(mode, func(t *testing.T) {
			e, it, client := recoveryFixture(t)
			id := it.row.Streamer.ID
			if mode != "recording" {
				e.completed(it)
			}
			switch mode {
			case "starting":
				_, cancel := context.WithCancel(e.ctx)
				defer cancel()
				it.recordCancel = cancel
			case "checking":
				it.row.Status = "Checking"
			case "streamer paused":
				if err := e.SetMonitoringPaused(id, true); err != nil {
					t.Fatal(err)
				}
			case "monitoring disabled":
				cfg := e.store.Config()
				cfg.PollingEnabled = false
				if err := e.SaveConfig(cfg); err != nil {
					t.Fatal(err)
				}
			case "removed":
				delete(e.items, id)
			}
			next := it.row.NextCheck
			if err := e.RetryCheck(id); err == nil || client.calls.Load() != 0 || !it.row.NextCheck.Equal(next) {
				t.Fatal("retry changed an ineligible streamer's schedule or made a request")
			}
		})
	}
}

func TestStorageFailureDuringRecordingRemainsVisibleAfterOfflineCheck(t *testing.T) {
	e, it, _ := recoveryFixture(t)
	h := it.handle.(*fakeRecording)
	it.handle = &endedRecording{fakeRecording: h, result: recording.Result{
		Path: h.File(), Bytes: 100, EndedAt: time.Now(), TransportEnded: true,
		Err: fault.Action("could not synchronize the raw FLV to disk", os.ErrPermission),
	}}
	e.completed(it)
	if it.row.Status != "Needs attention" || it.row.Error == "" || !it.recoveryUntil.IsZero() {
		t.Fatal("storage failure was hidden or started rapid media recovery", it.row)
	}
	warning := it.row.Error
	e.check(it.row.Streamer.ID)
	if it.row.Status != "Needs attention" || it.row.Error != warning {
		t.Fatal("a successful presence check hid an unresolved storage failure", it.row)
	}
}
