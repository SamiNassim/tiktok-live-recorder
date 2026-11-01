package monitor

import (
	"context"
	"strings"
	"testing"
	"time"

	"tiktok-live-recorder/internal/fault"
	"tiktok-live-recorder/internal/recording"
	"tiktok-live-recorder/internal/state"
	"tiktok-live-recorder/internal/tiktok"
)

func TestStartupLogsExplainOperationAndQueuedRetry(t *testing.T) {
	for _, cause := range []error{context.DeadlineExceeded, fault.Action("recording folder is not writable", nil)} {
		store, log := fixture(t)
		client := &fakeClient{}
		client.live.Store(true)
		e := newEngine(store, log, client, func(context.Context, state.Streamer, tiktok.Stream, state.Config) (activeRecording, error) {
			return nil, cause
		})
		waitFor(t, func() bool {
			for _, entry := range log.Entries() {
				if strings.HasPrefix(entry.Message, "Could not start recording:") {
					level := "WARN"
					if fault.RequiresAction(cause) {
						level = "ERROR"
					}
					if entry.Level != level || !strings.Contains(entry.Message, "Next check queued in 15s") || entry.Details == "" || strings.Contains(entry.Message, "context deadline exceeded") {
						t.Error("startup message lost severity, retry or readable context", entry)
					}
					return true
				}
			}
			return false
		})
		e.Close()
	}
}

func TestRateLimitWarningIsNotDuplicatedAndIncludesActualQueueDelay(t *testing.T) {
	e, it, client := recoveryFixture(t)
	client.err = tiktok.ErrRateLimited
	e.completed(it)
	e.check(it.row.Streamer.ID)
	count := 0
	for _, entry := range e.log.Entries() {
		if entry.Level == "WARN" {
			count++
			if !strings.Contains(entry.Message, "Next check queued in 5m0s") || entry.Details == "" {
				t.Fatal("rate-limit warning omitted its actual backoff", entry)
			}
		}
	}
	if count != 1 {
		t.Fatal("one rate limit produced duplicate warnings", count)
	}
}

func TestCompletionNeverClaimsOfflineOrSuccessfulSaveWithoutEvidence(t *testing.T) {
	for _, result := range []recording.Result{
		{Err: context.DeadlineExceeded},
		{Bytes: 100, Err: context.DeadlineExceeded},
		{Bytes: 100, TransportEnded: true},
	} {
		level, text := recordingCompletion(result, false)
		if strings.Contains(strings.ToLower(text), "offline") || strings.Contains(text, "broadcast ended") || strings.Contains(text, "context deadline exceeded") {
			t.Fatal("completion misrepresented a transport event", text)
		}
		if result.TransportEnded && level != "INFO" || result.Bytes == 0 && strings.Contains(text, "saved (") {
			t.Fatal("completion severity or save claim is misleading", level, text)
		}
	}
	cfg := state.Defaults()
	cfg.PollingEnabled = false
	if text := nextCheckMessage(cfg, &item{row: Row{NextCheck: time.Now().Add(time.Second)}}); text != "Polling is disabled." {
		t.Fatal("disabled polling advertised a scheduled retry", text)
	}
}
