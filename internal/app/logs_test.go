package app

import (
	"context"
	"testing"

	"github.com/egoist/mygo/ui"

	"tiktok-live-recorder/internal/fault"
)

func TestLogDetailsCanBeCopiedAndSearchedWithoutClutteringMessage(t *testing.T) {
	a := accountFixture(t)
	a.page = "logs"
	a.log.WriteError("WARN", "demo", "Stream setup failed: Stream setup timed out. Queued to retry.", fault.Wrap("Stream setup timed out", context.DeadlineExceeded))
	a.refresh()
	tt := ui.NewTester(a.view, 1400, 900)
	message, ok := tt.Find("Stream setup failed: Stream setup timed out. Queued to retry.")
	if !ok || tt.HasText("context deadline exceeded") {
		t.Fatal("diagnostic cause leaked into the main table or summary was hidden")
	}
	tt.RightClickAt(message.X+10, message.Y+message.H/2)
	if err := tt.ChooseMenuItem("Copy diagnostic details"); err != nil {
		t.Fatal(err)
	}
	if tt.Clipboard() != "context deadline exceeded" {
		t.Fatal("copied diagnostics should contain only the underlying cause")
	}
	a.logQuery = "deadline exceeded"
	tt.Frame()
	if len(a.logRows()) != 1 {
		t.Fatal("technical cause cannot be found by searching logs")
	}
}
