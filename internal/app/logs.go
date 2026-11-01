package app

import (
	"github.com/egoist/mygo/ui"
	"tiktok-live-recorder/internal/logging"
)

func logMenu(m *ui.Menu, entry logging.Entry, services ui.Services) {
	if m.Item("Copy message").Chosen() {
		services.WriteClipboard(entry.Message)
	}
	if m.Item("Copy diagnostic details").Disabled(entry.Details == "").Chosen() {
		services.WriteClipboard(entry.Message + "\n\n" + entry.Details)
	}
}
