package logging

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSavedAndDisplayedLogNeverRetainsSessionOrSignedMediaURL(t *testing.T) {
	dir := t.TempDir()
	l, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	l.WriteError("WARN", "demo_creator", "Read failed for https://media.example/live.flv?token=synthetic-source-secret", errors.New("request failed: sessionid=synthetic-cookie; https://media.example/?signature=synthetic-signature"))
	entry := l.Entries()[0]
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "tiktok-live-recorder.log"))
	if err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{entry.Message, entry.Details, string(raw)} {
		for _, secret := range []string{"synthetic-source-secret", "synthetic-cookie", "synthetic-signature"} {
			if strings.Contains(text, secret) {
				t.Fatal("log contains a synthetic secret")
			}
		}
	}
}
