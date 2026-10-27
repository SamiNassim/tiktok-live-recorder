package monitor

import (
	"fmt"
	"testing"
	"tiktok-live-recorder/internal/state"
	"time"
)

func TestSnapshotReusesRowsUntilObservableStateChanges(t *testing.T) {
	store, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	e := &Engine{store: store, items: map[string]*item{}}

	for i := range 50 {
		id := fmt.Sprintf("creator_%d", i)
		e.items[id] = &item{row: Row{Streamer: state.Streamer{ID: id}, Status: "Offline"}}
	}
	previous := e.Snapshot()
	for range 100 {
		next, changed := e.SnapshotIfChanged(previous)
		if changed || &next.Rows[0] != &previous.Rows[0] {
			t.Fatal("unchanged map order invalidated the snapshot")
		}
	}
	if n := testing.AllocsPerRun(100, func() { e.SnapshotIfChanged(previous) }); n != 0 {
		t.Fatal("idle snapshot allocates", n)
	}
	e.items["creator_0"].row.NextCheck = time.Now()
	next, changed := e.SnapshotIfChanged(previous)
	if !changed {
		t.Fatal("queue change was missed")
	}
	if &next.Rows[0] == &previous.Rows[0] {
		t.Fatal("changed rows reused mutable snapshot storage")
	}
	e.items["creator_0"].row.Streamer.RecordingPaused = true
	if _, changed := e.SnapshotIfChanged(next); !changed {
		t.Fatal("recording-pause change was missed")
	}
	previous = e.Snapshot()
	cfg := store.Config()
	cfg.PollingEnabled = !cfg.PollingEnabled
	if err := store.SaveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	if _, changed := e.SnapshotIfChanged(previous); !changed {
		t.Fatal("monitoring status change was missed")
	}

	previous = e.Snapshot()
	delete(e.items, "creator_1")
	if _, changed := e.SnapshotIfChanged(previous); !changed {
		t.Fatal("removed streamer was missed")
	}
}
