package monitor

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"tiktok-live-recorder/internal/network"
	"tiktok-live-recorder/internal/recording"
	"tiktok-live-recorder/internal/state"
	"tiktok-live-recorder/internal/tiktok"
)

type splitRecording struct {
	*fakeRecording
	mu        sync.Mutex
	clips     []recording.Clip
	finalPath string
}

func (h *splitRecording) TakeClips() []recording.Clip {
	h.mu.Lock()
	defer h.mu.Unlock()
	clips := h.clips
	h.clips = nil
	return clips
}
func (h *splitRecording) Result() recording.Result {
	r := h.fakeRecording.Result()
	r.Path = h.finalPath
	return r
}

func TestCompletedClipsEnterHistoryAndProcessWithoutPollingActiveStream(t *testing.T) {
	store, log := fixture(t)
	client := &fakeClient{}
	client.live.Store(true)
	initial := filepath.Join(t.TempDir(), "capture_part001.flv")
	next := filepath.Join(filepath.Dir(initial), "capture_part002.flv")
	h := &splitRecording{fakeRecording: &fakeRecording{done: make(chan struct{}), started: time.Now(), path: initial}, finalPath: next}
	e := newEngine(store, log, client, func(context.Context, state.Streamer, tiktok.Stream, state.Config) (activeRecording, error) {
		return h, nil
	})
	defer e.Close()
	waitFor(t, func() bool { return e.Snapshot().Recording == 1 })
	processed := make(chan string, 2)
	e.mu.Lock()
	e.process = func(ctx context.Context, path string, cfg state.Config, report func(recording.Progress)) (recording.ProcessedCopy, error) {
		processed <- path
		return recording.ProcessedCopy{}, errors.New("synthetic conversion failure")
	}
	e.mu.Unlock()
	// Changes to future recordings must not change processing for this session.
	cfg := store.Config()
	cfg.PostProcess = false
	if err := e.SaveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	h.mu.Lock()
	h.clips = []recording.Clip{{Result: recording.Result{Path: initial, Bytes: 5000, EndedAt: time.Now()}, StartedAt: h.started, NextPath: next, NextStartedAt: time.Now()}}
	h.mu.Unlock()
	e.reconcile()
	select {
	case path := <-processed:
		if path != initial {
			t.Fatal("wrong completed part processed")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("completed part not processed while live")
	}
	history := store.Data().Recordings
	if len(history) != 2 || history[0].EndedAt.IsZero() || history[0].Bytes != 5000 || !history[1].EndedAt.IsZero() || history[1].Path != next {
		t.Fatal("split history incorrect", history)
	}
	if e.next() != "" || client.calls.Load() != 1 || e.Snapshot().Rows[0].Path != next {
		t.Fatal("clip rotation polled or disconnected active capture")
	}
	h.Stop()
	e.reconcile()
	history = store.Data().Recordings
	if history[1].EndedAt.IsZero() || history[1].Bytes != 1234 {
		t.Fatal("final part not finalized", history)
	}
}

func TestProxyChangeStopsActiveCaptureAndPreservesPauseSettings(t *testing.T) {
	store, log := fixture(t)
	client := &fakeClient{}
	client.live.Store(true)
	h := &fakeRecording{done: make(chan struct{}), started: time.Now(), path: filepath.Join(t.TempDir(), "raw.flv")}
	e := newEngine(store, log, client, func(context.Context, state.Streamer, tiktok.Stream, state.Config) (activeRecording, error) {
		return h, nil
	})
	defer e.Close()
	waitFor(t, func() bool { return e.Snapshot().Recording == 1 })
	cfg := store.Config()
	cfg.Proxy = network.Config{Enabled: true, URL: "http://proxy.invalid:8080"}
	cfg.PollingEnabled = false
	if err := e.SaveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	select {
	case <-h.Done():
	case <-time.After(time.Second):
		t.Fatal("proxy change did not stop old connection")
	}
	waitFor(t, func() bool {
		history := store.Data().Recordings
		return len(history) == 1 && !history[0].EndedAt.IsZero()
	})
	data := store.Data()
	if data.Streamers[0].Paused || data.Streamers[0].RecordingPaused || data.Recordings[0].EndedAt.IsZero() {
		t.Fatal("proxy change lost capture history or changed pauses")
	}
}

func TestProxyChangeRejectsDiscoveryFromPreviousRoute(t *testing.T) {
	store, log := fixture(t)
	client := &fakeClient{entered: make(chan struct{}, 1), release: make(chan struct{})}
	client.live.Store(true)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	e := &Engine{store: store, log: log, client: client, ctx: ctx, cancel: cancel, items: make(map[string]*item), wake: make(chan struct{}, 1)}
	e.start = func(context.Context, state.Streamer, tiktok.Stream, state.Config) (activeRecording, error) {
		t.Error("stale discovery started a recording")
		return nil, errors.New("unexpected recording")
	}
	e.reconcile()
	done := make(chan struct{})
	go func() { e.check("demo_creator"); close(done) }()
	<-client.entered
	cfg := store.Config()
	cfg.Proxy = network.Config{Enabled: true, URL: "socks5://proxy.invalid:1080"}
	if err := e.SaveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	close(client.release)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("old discovery did not finish")
	}
}
