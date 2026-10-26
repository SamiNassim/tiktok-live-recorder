package app

import (
	"testing"

	"github.com/egoist/mygo/ui"
)

func TestRawCleanupToggleRequiresProcessing(t *testing.T) {
	a := accountFixture(t)
	a.page = "settings"
	tt := ui.NewTester(a.view, 1400, 2600)
	const cleanup = "Delete original FLV after processing"
	const processing = "Create a processed copy with FFmpeg"
	focus := func(label string) bool {
		for range 40 {
			if tt.Focused(label) {
				return true
			}
			tt.Key(0, ui.KeyTab)
		}
		return tt.Focused(label)
	}
	activate := func(label string) {
		t.Helper()
		if !focus(label) {
			t.Fatal("setting is not keyboard accessible", label)
		}
		tt.Key(0, ui.KeySpace)
	}
	if a.cfg.DeleteRawAfterProcessing {
		t.Fatal("cleanup is enabled by default")
	}
	activate(cleanup)
	if !a.cfg.DeleteRawAfterProcessing {
		t.Fatal("cleanup could not be enabled with processing")
	}
	activate(processing)
	if a.cfg.PostProcess || a.cfg.DeleteRawAfterProcessing {
		t.Fatal("disabling processing left cleanup enabled")
	}
	if focus(cleanup) || a.cfg.DeleteRawAfterProcessing {
		t.Fatal("cleanup is interactive without processing")
	}
	activate(processing)
	if !a.cfg.PostProcess || a.cfg.DeleteRawAfterProcessing {
		t.Fatal("re-enabling processing restored destructive cleanup automatically")
	}
	activate(cleanup)
	if !a.cfg.DeleteRawAfterProcessing {
		t.Fatal("cleanup did not become available after re-enabling processing")
	}
}
