package monitor

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"tiktok-live-recorder/internal/recording"
)

type endedRecording struct {
	*fakeRecording
	result recording.Result
}

func (h *endedRecording) Result() recording.Result { return h.result }

func TestTransportEndIsInformationalButStorageFailuresRemainWarnings(t *testing.T) {
	for _, test := range []struct {
		name           string
		err            error
		transportEnded bool
		level          string
	}{
		{"early HTTP close", io.ErrUnexpectedEOF, true, "INFO"},
		{"disk failure", errors.New("disk sync failed"), false, "WARN"},
		{"mixed errors", errors.Join(io.ErrUnexpectedEOF, errors.New("disk sync failed")), false, "WARN"},
	} {
		t.Run(test.name, func(t *testing.T) {
			store, log := fixture(t)
			e := &Engine{store: store, log: log, ctx: context.Background()}
			h := &endedRecording{fakeRecording: &fakeRecording{path: "synthetic.flv"}, result: recording.Result{Path: "synthetic.flv", Bytes: 100, EndedAt: time.Now(), Err: test.err, TransportEnded: test.transportEnded}}
			it := &item{handle: h}
			it.row.Streamer.ID = "demo_creator"
			e.completed(it)
			entries := log.Entries()
			if len(entries) != 1 || entries[0].Level != test.level {
				t.Fatal("wrong end-of-recording log level", entries)
			}
			if test.transportEnded && strings.Contains(strings.ToLower(entries[0].Message), "offline") {
				t.Fatal("transport closure incorrectly asserted streamer was offline")
			}
		})
	}
}
