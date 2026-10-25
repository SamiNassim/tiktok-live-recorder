// Package monitor coordinates one paced polling queue. Network and disk work
// never run on the window thread, and active recordings are never polled.
package monitor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"tiktok-live-recorder/internal/logging"
	"tiktok-live-recorder/internal/recording"
	"tiktok-live-recorder/internal/state"
	"tiktok-live-recorder/internal/tiktok"
)

type Row struct {
	Streamer                        state.Streamer
	Status, Error                   string
	LastCheck, NextCheck, StartedAt time.Time
	Path                            string
	Live                            bool
	LiveCheckedAt                   time.Time
}
type Snapshot struct {
	Rows                                                       []Row
	Recording, MonitoringPaused, RecordingPaused, QueueTargets int
	Polling                                                    bool
}
type Event struct{ Name, StreamerID, Path string }
type streamClient interface {
	Configure(string, time.Duration)
	Check(context.Context, string) (tiktok.Stream, error)
}
type activeRecording interface {
	File() string
	Started() time.Time
	Stop()
	Done() <-chan struct{}
	Result() recording.Result
}
type startRecording func(context.Context, state.Streamer, tiktok.Stream, state.Config) (activeRecording, error)
type item struct {
	row          Row
	handle       activeRecording
	recordCancel context.CancelFunc
	failures     int
	config       state.Config
}
type Engine struct {
	store      *state.Store
	log        *logging.Log
	client     streamClient
	start      startRecording
	ctx        context.Context
	cancel     context.CancelFunc
	mu         sync.Mutex
	items      map[string]*item
	wake       chan struct{}
	events     chan Event
	wg         sync.WaitGroup
	processing chan struct{}
	process    func(context.Context, string, state.Config) (string, error)
}

func New(store *state.Store, log *logging.Log) *Engine {
	return newEngine(store, log, tiktok.NewClient(), func(ctx context.Context, s state.Streamer, source tiktok.Stream, cfg state.Config) (activeRecording, error) {
		return recording.Start(ctx, s, source, cfg)
	})
}
func newEngine(store *state.Store, log *logging.Log, client streamClient, start startRecording) *Engine {
	ctx, cancel := context.WithCancel(context.Background())
	e := &Engine{store: store, log: log, client: client, start: start, ctx: ctx, cancel: cancel, items: make(map[string]*item), wake: make(chan struct{}, 1), events: make(chan Event, 64), processing: make(chan struct{}, 2), process: recording.Process}
	e.recoverHistory()
	e.configure(store.Config())
	e.wg.Add(1)
	go e.run()
	return e
}
func (e *Engine) Events() <-chan Event { return e.events }
func (e *Engine) wakeUp() {
	select {
	case e.wake <- struct{}{}:
	default:
	}
}
func (e *Engine) configure(c state.Config) {
	e.client.Configure(c.Cookie, time.Duration(c.RequestDelaySeconds)*time.Second)
}
func (e *Engine) Close() { e.cancel(); e.wg.Wait() }

func (e *Engine) SaveConfig(c state.Config) error {
	var err error
	c.Cookie, err = tiktok.ParseCookie(c.Cookie)
	if err != nil {
		return err
	}
	if c.PostProcess {
		if _, err = recording.OutputArgs(c.FFmpegArgs); err != nil {
			return err
		}
	}
	if err = e.store.SaveConfig(c); err != nil {
		return err
	}
	e.configure(c)
	e.mu.Lock()
	for _, it := range e.items {
		it.row.NextCheck = time.Now()
		it.failures = 0
	}
	e.mu.Unlock()
	e.wakeUp()
	e.log.Write("INFO", "", "Settings saved")
	return nil
}
func (e *Engine) Add(raw, name string) error {
	id, err := tiktok.ParseID(raw)
	if err != nil {
		return err
	}
	err = e.store.Update(func(d *state.Data) error {
		if slices.ContainsFunc(d.Streamers, func(s state.Streamer) bool { return s.ID == id }) {
			return errors.New("this streamer is already in the list")
		}
		d.Streamers = append(d.Streamers, state.Streamer{ID: id, CustomName: strings.TrimSpace(name), AddedAt: time.Now()})
		return nil
	})
	if err == nil {
		e.log.Write("INFO", id, "Streamer added")
		e.wakeUp()
	}
	return err
}
func (e *Engine) Edit(id string, change func(*state.Streamer)) error {
	var before, after state.Streamer
	err := e.store.Update(func(d *state.Data) error {
		for i := range d.Streamers {
			if d.Streamers[i].ID == id {
				before = d.Streamers[i]
				change(&d.Streamers[i])
				after = d.Streamers[i]
				return nil
			}
		}
		return errors.New("streamer no longer exists")
	})
	if err == nil {
		e.mu.Lock()
		if it := e.items[id]; it != nil {
			it.row.Streamer = after
			if before.Paused != after.Paused || before.RecordingPaused != after.RecordingPaused || before.LowPriority != after.LowPriority {
				it.row.NextCheck = time.Now()
				it.row.Error = ""
				it.failures = 0
				if after.Paused || after.RecordingPaused {
					stop(it)
				}
				if it.handle == nil {
					it.row.Status = "Waiting"
					if after.Paused {
						it.row.Status = "Monitoring paused"
						it.row.NextCheck = time.Time{}
					}
				}
			}
		}
		e.mu.Unlock()
		e.wakeUp()
	}
	return err
}
func (e *Engine) SetRecordingPaused(id string, paused bool) error {
	err := e.Edit(id, func(s *state.Streamer) { s.RecordingPaused = paused })
	if err == nil {
		message := "Recording enabled"
		if paused {
			message = "Recording paused; monitoring remains active"
		}
		e.log.Write("INFO", id, message)
	}
	return err
}
func (e *Engine) SetMonitoringPaused(id string, paused bool) error {
	err := e.Edit(id, func(s *state.Streamer) { s.Paused = paused })
	if err == nil {
		message := "Monitoring resumed"
		if paused {
			message = "Monitoring paused"
		}
		e.log.Write("INFO", id, message)
	}
	return err
}
func stop(it *item) {
	if it.handle != nil {
		it.row.Status = "Finalizing"
		it.handle.Stop()
	} else if it.recordCancel != nil {
		it.recordCancel()
	}
}
func (e *Engine) Remove(id string) error {
	err := e.store.Update(func(d *state.Data) error {
		d.Streamers = slices.DeleteFunc(d.Streamers, func(s state.Streamer) bool { return s.ID == id })
		return nil
	})
	if err == nil {
		e.mu.Lock()
		if it := e.items[id]; it != nil {
			stop(it)
		}
		e.mu.Unlock()
		e.log.Write("INFO", id, "Streamer removed; existing files kept")
		e.wakeUp()
	}
	return err
}
func (e *Engine) Snapshot() Snapshot {
	s := Snapshot{Polling: e.store.Config().PollingEnabled}
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, it := range e.items {
		s.Rows = append(s.Rows, it.row)
		if it.handle != nil && it.row.Status == "Recording" {
			s.Recording++
		}
		if !it.row.Streamer.Paused && it.handle == nil {
			s.QueueTargets++
		}
		if it.row.Streamer.Paused {
			s.MonitoringPaused++
		}
		if it.row.Streamer.RecordingPaused {
			s.RecordingPaused++
		}
	}
	return s
}

func (e *Engine) run() {
	defer e.wg.Done()
	tick := time.NewTicker(500 * time.Millisecond)
	defer tick.Stop()
	defer func() {
		e.mu.Lock()
		var handles []activeRecording
		for _, it := range e.items {
			if it.recordCancel != nil {
				it.recordCancel()
			}
			if it.handle != nil {
				it.handle.Stop()
				handles = append(handles, it.handle)
			}
		}
		e.mu.Unlock()
		for _, h := range handles {
			<-h.Done()
		}
		e.mu.Lock()
		defer e.mu.Unlock()
		for _, it := range e.items {
			if it.handle != nil {
				e.completed(it)
			}
		}
	}()
	for {
		select {
		case <-e.ctx.Done():
			return
		case <-tick.C:
		case <-e.wake:
		}
		e.reconcile()
		for {
			if e.ctx.Err() != nil {
				return
			}
			id := e.next()
			if id == "" {
				break
			}
			e.check(id)
			e.reconcile()
		}
	}
}
func (e *Engine) reconcile() {
	data := e.store.Data()
	e.mu.Lock()
	defer e.mu.Unlock()
	seen := make(map[string]bool)
	for _, s := range data.Streamers {
		seen[s.ID] = true
		it := e.items[s.ID]
		if it == nil {
			it = &item{row: Row{Status: "Waiting", NextCheck: time.Now()}}
			e.items[s.ID] = it
		}
		it.row.Streamer = s
		if it.handle != nil {
			select {
			case <-it.handle.Done():
				e.completed(it)
			default:
			}
		}
		if s.Paused {
			stop(it)
			if it.handle == nil {
				it.row.Status = "Monitoring paused"
			}
			it.row.NextCheck = time.Time{}
			continue
		}
		if s.RecordingPaused && it.handle != nil {
			stop(it)
		}
		if it.row.Status == "Monitoring paused" {
			it.row.Status = "Waiting"
			it.row.NextCheck = time.Now()
		}
	}
	for id, it := range e.items {
		if !seen[id] {
			stop(it)
			if it.handle != nil {
				select {
				case <-it.handle.Done():
					e.completed(it)
				default:
					continue
				}
			}
			delete(e.items, id)
		}
	}
}
func (e *Engine) next() string {
	if !e.store.Config().PollingEnabled {
		return ""
	}
	now := time.Now()
	e.mu.Lock()
	defer e.mu.Unlock()
	var chosen *item
	var id string
	for key, it := range e.items {
		if it.row.Streamer.Paused || it.handle != nil || it.row.NextCheck.After(now) {
			continue
		}
		if chosen == nil || it.row.NextCheck.Before(chosen.row.NextCheck) || (it.row.NextCheck.Equal(chosen.row.NextCheck) && key < id) {
			chosen = it
			id = key
		}
	}
	return id
}
func (e *Engine) check(id string) {
	e.mu.Lock()
	it := e.items[id]
	if it == nil || it.handle != nil || it.row.Streamer.Paused {
		e.mu.Unlock()
		return
	}
	it.row.Status = "Checking"
	e.mu.Unlock()
	source, err := e.client.Check(e.ctx, id)
	if e.ctx.Err() != nil {
		return
	}
	cfg := e.store.Config()
	e.mu.Lock()
	defer e.mu.Unlock()
	data := e.store.Data()
	at := slices.IndexFunc(data.Streamers, func(s state.Streamer) bool { return s.ID == id })
	if at < 0 || data.Streamers[at].Paused || !cfg.PollingEnabled {
		it.row.Status = "Waiting"
		return
	}
	it.row.Streamer = data.Streamers[at]
	it.row.LastCheck = time.Now()
	it.row.NextCheck = time.Now().Add(interval(cfg, it.row.Streamer.LowPriority))
	if source.Name != "" || source.Avatar != "" {
		e.persist(func(d *state.Data) {
			for i := range d.Streamers {
				if d.Streamers[i].ID == id {
					if source.Name != "" {
						d.Streamers[i].Name = source.Name
						it.row.Streamer.Name = source.Name
					}
					if source.Avatar != "" {
						d.Streamers[i].Avatar = source.Avatar
						it.row.Streamer.Avatar = source.Avatar
					}
				}
			}
		})
	}
	if err == nil || source.Live {
		it.row.Live = source.Live
		it.row.LiveCheckedAt = time.Now()
	}
	if err != nil {
		it.failures++
		if it.row.Error != err.Error() {
			e.log.Write("WARN", id, err.Error())
		}
		it.row.Error = err.Error()
		it.row.Status = "Needs attention"
		if source.Live {
			it.row.Status = "Live · access needed"
		}
		if errors.Is(err, tiktok.ErrRateLimited) {
			it.row.NextCheck = time.Now().Add(max(5*time.Minute, interval(cfg, it.row.Streamer.LowPriority)))
			e.log.Write("WARN", id, "Rate limited; backing off before checking this account again")
		}
		return
	}
	it.row.Error = ""
	it.failures = 0
	if !source.Live {
		it.row.Status = "Offline"
		return
	}
	if it.row.Streamer.RecordingPaused {
		it.row.Status = "Live"
		return
	}
	it.row.Status = "Starting"
	streamer := it.row.Streamer
	recordCtx, recordCancel := context.WithCancel(e.ctx)
	it.recordCancel = recordCancel
	e.mu.Unlock()
	h, startErr := e.start(recordCtx, streamer, source, cfg)
	e.mu.Lock()
	if startErr != nil {
		canceled := recordCtx.Err() != nil
		recordCancel()
		it.recordCancel = nil
		it.row.Status = "Needs attention"
		if canceled {
			it.row.Status = "Waiting"
			it.row.NextCheck = time.Now()
			return
		}
		it.row.Error = startErr.Error()
		it.row.NextCheck = time.Now().Add(max(15*time.Second, time.Duration(cfg.RequestDelaySeconds)*time.Second))
		e.log.Write("WARN", id, startErr.Error())
		return
	}
	it.handle = h
	it.config = cfg
	it.row.Status = "Recording"
	it.row.StartedAt = h.Started()
	it.row.Path = h.File()
	it.row.NextCheck = time.Time{}
	it.row.Streamer.LastRecordedAt = h.Started()
	e.persist(func(d *state.Data) {
		for i := range d.Streamers {
			if d.Streamers[i].ID == id {
				d.Streamers[i].LastRecordedAt = h.Started()
				if cfg.AutoPromote {
					d.Streamers[i].LowPriority = false
				}
			}
		}
		d.Recordings = append(d.Recordings, state.Recording{ID: fmt.Sprintf("%d-%s", h.Started().UnixNano(), id), StreamerID: id, Name: streamer.DisplayName(), Path: h.File(), StartedAt: h.Started()})
	})
	e.log.Write("INFO", id, "Raw FLV recording started")
	data = e.store.Data()
	at = slices.IndexFunc(data.Streamers, func(s state.Streamer) bool { return s.ID == id })
	if at < 0 || data.Streamers[at].Paused || data.Streamers[at].RecordingPaused || recordCtx.Err() != nil {
		stop(it)
		return
	}
	select {
	case e.events <- Event{streamer.DisplayName(), id, h.File()}:
	default:
	}
}

func (e *Engine) completed(it *item) {
	immediate := it.row.Status == "Finalizing" || it.row.Streamer.RecordingPaused || it.row.Streamer.Paused
	r := it.handle.Result()
	initial := it.handle.File()
	e.persist(func(d *state.Data) {
		for i := range d.Recordings {
			if d.Recordings[i].Path == initial {
				d.Recordings[i].EndedAt = r.EndedAt
				d.Recordings[i].Bytes = r.Bytes
				if r.Err != nil {
					d.Recordings[i].Error = r.Err.Error()
				}
			}
		}
	})
	if r.Err != nil {
		e.log.Write("WARN", it.row.Streamer.ID, "Recording ended; raw file kept: "+r.Err.Error())
	} else {
		e.log.Write("INFO", it.row.Streamer.ID, fmt.Sprintf("Raw FLV finalized (%d bytes)", r.Bytes))
	}
	if it.config.PostProcess && r.Err == nil && r.Bytes > 13 && e.ctx.Err() == nil {
		e.processCopy(it.row.Streamer.ID, initial, it.config)
	}
	if it.recordCancel != nil {
		it.recordCancel()
		it.recordCancel = nil
	}
	it.handle = nil
	it.row.StartedAt = time.Time{}
	it.row.Path = ""
	it.row.Status = "Waiting"
	it.row.NextCheck = time.Now().Add(5 * time.Second)
	if immediate {
		it.row.NextCheck = time.Now()
	}
}
func (e *Engine) persist(change func(*state.Data)) {
	if err := e.store.Update(func(d *state.Data) error { change(d); return nil }); err != nil {
		e.log.Write("ERROR", "storage", err.Error())
	}
}
func (e *Engine) recoverHistory() {
	if !slices.ContainsFunc(e.store.Data().Recordings, func(r state.Recording) bool { return r.EndedAt.IsZero() }) {
		return
	}
	e.persist(func(d *state.Data) {
		for i := range d.Recordings {
			r := &d.Recordings[i]
			if !r.EndedAt.IsZero() {
				continue
			}
			r.EndedAt = time.Now()
			r.Error = "Application stopped before finalization; original FLV preserved"
			if info, err := os.Stat(r.Path); err == nil {
				r.Bytes = info.Size()
				r.EndedAt = info.ModTime()
			}
		}
	})
	e.log.Write("WARN", "recordings", "Recovered interrupted history; original files were preserved")
}
func interval(c state.Config, low bool) time.Duration {
	if low {
		return time.Duration(c.ColdSeconds) * time.Second
	}
	return time.Duration(c.PollSeconds) * time.Second
}
func EffectiveIntervals(c state.Config, eligible int) (normal, cold time.Duration) {
	sweep := time.Duration(eligible*c.RequestDelaySeconds) * time.Second
	return max(time.Duration(c.PollSeconds)*time.Second, sweep), max(time.Duration(c.ColdSeconds)*time.Second, sweep)
}
