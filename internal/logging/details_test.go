package logging

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"tiktok-live-recorder/internal/fault"
)

func TestLegacyDiagnosticDetailsOmitRepeatedSummary(t *testing.T) {
	dir := t.TempDir()
	const summary = "Could not complete the sample operation. Original file kept"
	entries := []Entry{
		{Message: "Sample job failed: " + summary, Details: summary + ": unexpected tag at byte 1024"},
		{Message: "Sample job failed: " + summary, Details: summary},
	}
	f, err := os.Create(filepath.Join(dir, "tiktok-live-recorder.log"))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if err := json.NewEncoder(f).Encode(entry); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	l, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	got := l.Entries()
	if len(got) != 2 || got[0].Details != "unexpected tag at byte 1024" || got[1].Details != "" {
		t.Fatal("old entries should only show distinct details", got)
	}
}

func TestDiagnosticDetailsSurviveRotationAndReloadSeparately(t *testing.T) {
	dir := t.TempDir()
	l, err := OpenSize(dir, 3000)
	if err != nil {
		t.Fatal(err)
	}
	for range 50 {
		l.WriteError("WARN", "demo", "Stream setup timed out; queued to retry", fault.Wrap("waiting for the FLV header", context.DeadlineExceeded))
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenSize(dir, 3000)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	entries := reopened.Entries()
	if len(entries) == 0 || entries[len(entries)-1].Sequence != 50 {
		t.Fatal("lost latest failure on reload")
	}
	for _, entry := range entries {
		if strings.Contains(entry.Message, "context deadline") || !strings.Contains(entry.Details, "waiting for the FLV header: context deadline exceeded") {
			t.Fatal("readable and diagnostic text were mixed or lost", entry)
		}
	}
	files, _ := filepath.Glob(filepath.Join(dir, "*.log*"))
	if len(files) != 3 {
		t.Fatal("diagnostic logging stopped rotating", files)
	}
	for _, path := range files {
		info, err := os.Stat(path)
		if err != nil || info.Size() > 3000 {
			t.Fatal("diagnostic file exceeded its limit", err)
		}
	}
}
