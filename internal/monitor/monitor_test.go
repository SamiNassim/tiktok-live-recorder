package monitor

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"tiktok-live-recorder/internal/logging"
	"tiktok-live-recorder/internal/recording"
	"tiktok-live-recorder/internal/state"
	"tiktok-live-recorder/internal/tiktok"
)

type fakeClient struct {
	calls            atomic.Int64
	live             atomic.Bool
	err              error
	entered, release chan struct{}
}

func (c *fakeClient) Configure(string, time.Duration) {}
func (c *fakeClient) Check(ctx context.Context, id string) (tiktok.Stream, error) {
	c.calls.Add(1)
	if c.entered != nil {
		select {
		case c.entered <- struct{}{}:
		default:
		}
		select {
		case <-c.release:
		case <-ctx.Done():
			return tiktok.Stream{}, ctx.Err()
		}
	}
	return tiktok.Stream{Live: c.live.Load(), Name: "Demo Creator", Sources: []tiktok.Source{{URL: "https://media.example.invalid/raw.flv"}}}, c.err
}

type fakeRecording struct {
	done    chan struct{}
	once    sync.Once
	started time.Time
	path    string
}

func (h *fakeRecording) File() string          { return h.path }
func (h *fakeRecording) Started() time.Time    { return h.started }
func (h *fakeRecording) Stop()                 { h.once.Do(func() { close(h.done) }) }
func (h *fakeRecording) Done() <-chan struct{} { return h.done }
func (h *fakeRecording) Result() recording.Result {
	<-h.done
	return recording.Result{Path: h.path, Bytes: 1234, EndedAt: time.Now()}
}
func fixture(t *testing.T) (*state.Store, *logging.Log) {
	t.Helper()
	s, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Update(func(d *state.Data) error { d.Streamers = []state.Streamer{{ID: "demo_creator"}}; return nil }); err != nil {
		t.Fatal(err)
	}
	l, err := logging.Open(s.Directory())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	return s, l
}
func waitFor(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.NewTimer(10 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for {
		if condition() {
			return
		}
		select {
		case <-deadline.C:
			t.Fatal("monitor condition was not reached")
		case <-tick.C:
		}
	}
}
func due(e *Engine) {
	e.mu.Lock()
	for _, it := range e.items {
		it.row.NextCheck = time.Now().Add(-time.Hour)
	}
	e.mu.Unlock()
	e.wakeUp()
}

func TestActiveRecordingsAreNotPolledAndCloseFinalizes(t *testing.T) {
	s, l := fixture(t)
	c := &fakeClient{}
	c.live.Store(true)
	h := &fakeRecording{done: make(chan struct{}), started: time.Now(), path: filepath.Join(t.TempDir(), "raw.flv")}
	e := newEngine(s, l, c, func(context.Context, state.Streamer, tiktok.Stream, state.Config) (activeRecording, error) {
		return h, nil
	})
	waitFor(t, func() bool { return e.Snapshot().Recording == 1 })
	for range 3 {
		due(e)
		e.reconcile()
		if e.next() != "" {
			t.Fatal("active recording entered queue")
		}
	}
	if c.calls.Load() != 1 {
		t.Fatal("active recording was checked", c.calls.Load())
	}
	e.Close()
	history := s.Data().Recordings
	if len(history) != 1 || history[0].EndedAt.IsZero() || history[0].Bytes != 1234 {
		t.Fatal(history)
	}
}

func TestRecordingPauseKeepsPresenceAndCanResume(t *testing.T) {
	s, l := fixture(t)
	if err := s.Update(func(d *state.Data) error { d.Streamers[0].RecordingPaused = true; return nil }); err != nil {
		t.Fatal(err)
	}
	c := &fakeClient{}
	c.live.Store(true)
	e := newEngine(s, l, c, func(context.Context, state.Streamer, tiktok.Stream, state.Config) (activeRecording, error) {
		return &fakeRecording{done: make(chan struct{}), started: time.Now(), path: filepath.Join(t.TempDir(), "raw.flv")}, nil
	})
	defer e.Close()
	waitFor(t, func() bool { r := e.Snapshot().Rows; return len(r) == 1 && r[0].Status == "Live" })
	if e.Snapshot().Recording != 0 {
		t.Fatal("paused recording started")
	}
	if err := e.SetRecordingPaused("demo_creator", false); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return e.Snapshot().Recording == 1 })
	if err := e.SetRecordingPaused("demo_creator", true); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return e.Snapshot().Recording == 0 })
	if s.Data().Streamers[0].Paused {
		t.Fatal("recording pause disabled monitoring")
	}
}

func TestDisabledMonitoringNeverChecksAndResumeDoesNotNeedReAdd(t *testing.T) {
	s, l := fixture(t)
	cfg := s.Config()
	cfg.PollingEnabled = false
	if err := s.SaveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	c := &fakeClient{}
	e := newEngine(s, l, c, nil)
	defer e.Close()
	e.reconcile()
	if e.next() != "" || c.calls.Load() != 0 {
		t.Fatal("disabled queue made a request")
	}
	cfg.PollingEnabled = true
	if err := e.SaveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return c.calls.Load() > 0 })
	if err := e.SetMonitoringPaused("demo_creator", true); err != nil {
		t.Fatal(err)
	}
	e.reconcile()
	if e.next() != "" {
		t.Fatal("paused streamer was queued")
	}
	if err := e.SetMonitoringPaused("demo_creator", false); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return c.calls.Load() > 1 })
}

func TestRemovedOrPausedDuringDiscoveryCannotStart(t *testing.T) {
	for _, remove := range []bool{true, false} {
		t.Run(map[bool]string{true: "remove", false: "pause"}[remove], func(t *testing.T) {
			s, l := fixture(t)
			c := &fakeClient{entered: make(chan struct{}, 1), release: make(chan struct{})}
			c.live.Store(true)
			var starts atomic.Int32
			e := newEngine(s, l, c, func(context.Context, state.Streamer, tiktok.Stream, state.Config) (activeRecording, error) {
				starts.Add(1)
				return nil, errors.New("should not start")
			})
			defer e.Close()
			<-c.entered
			var err error
			if remove {
				err = e.Remove("demo_creator")
			} else {
				err = e.SetMonitoringPaused("demo_creator", true)
			}
			if err != nil {
				t.Fatal(err)
			}
			close(c.release)
			waitFor(t, func() bool {
				if remove {
					return len(e.Snapshot().Rows) == 0
				}
				return e.Snapshot().Rows[0].Status == "Monitoring paused"
			})
			if starts.Load() != 0 {
				t.Fatal("recording started after removal/pause")
			}
		})
	}
}

func TestLiveWithoutPlaybackIsNotOffline(t *testing.T) {
	s, l := fixture(t)
	c := &fakeClient{err: tiktok.ErrAccess}
	c.live.Store(true)
	e := newEngine(s, l, c, nil)
	defer e.Close()
	waitFor(t, func() bool {
		rows := e.Snapshot().Rows
		return len(rows) == 1 && rows[0].Status == "Live · access needed"
	})
	if !e.Snapshot().Rows[0].Live {
		t.Fatal("access failure marked live streamer offline")
	}
}

func TestQueueCalculator(t *testing.T) {
	c := state.Defaults()
	normal, cold := EffectiveIntervals(c, 100)
	if normal != 200*time.Second || cold != 300*time.Second {
		t.Fatal(normal, cold)
	}
}

func TestProcessingNeverBlocksNextRecordingOrShutdown(t *testing.T) {
	s, l := fixture(t)
	cfg := s.Config()
	cfg.PostProcess = true
	if err := s.SaveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	c := &fakeClient{}
	c.live.Store(true)
	var starts atomic.Int32
	root := t.TempDir()
	e := newEngine(s, l, c, func(context.Context, state.Streamer, tiktok.Stream, state.Config) (activeRecording, error) {
		return &fakeRecording{done: make(chan struct{}), started: time.Now(), path: filepath.Join(root, fmt.Sprintf("raw-%d.flv", starts.Add(1)))}, nil
	})
	waitFor(t, func() bool { return e.Snapshot().Recording == 1 })
	processing := make(chan struct{}, 1)
	e.mu.Lock()
	e.process = func(ctx context.Context, path string, cfg state.Config) (string, error) {
		processing <- struct{}{}
		<-ctx.Done()
		return "", ctx.Err()
	}
	e.items["demo_creator"].handle.Stop()
	e.mu.Unlock()
	e.wakeUp()
	select {
	case <-processing:
	case <-time.After(10 * time.Second):
		e.Close()
		t.Fatal("processing did not start")
	}
	waitFor(t, func() bool { return e.Snapshot().Recording == 0 })
	due(e)
	waitFor(t, func() bool { return e.Snapshot().Recording == 1 })
	if starts.Load() != 2 {
		t.Fatal("new recording was blocked by processing")
	}
	e.Close()
}
