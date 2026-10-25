package recording

import (
	"context"
	"os"
	"os/exec"
	"testing"
	"time"

	"tiktok-live-recorder/internal/state"
	"tiktok-live-recorder/internal/tiktok"
)

// Live network tests are opt-in. No real identities, captures, or cookies are
// embedded in fixtures; output is written only to a test-owned directory.
func TestLiveIntegration(t *testing.T) {
	id := os.Getenv("TIKTOK_TEST_USER")
	if id == "" {
		t.Skip("set TIKTOK_TEST_USER to exercise a live public or authorized stream")
	}
	cookie, err := tiktok.ParseCookie(os.Getenv("TIKTOK_TEST_COOKIE"))
	if err != nil {
		t.Fatal("invalid test cookie format")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	client := tiktok.NewClient()
	client.Configure(cookie, 2*time.Second)
	source, err := client.Check(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if !source.Live {
		t.Skip("the test account is not currently live")
	}
	cfg := state.Defaults()
	cfg.RecordingPath = t.TempDir()
	h, err := Start(ctx, state.Streamer{ID: id}, source, cfg)
	if err != nil {
		t.Fatal(err)
	}
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	select {
	case <-timer.C:
	case <-h.Done():
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	h.Stop()
	r := h.Result()
	if r.Err != nil || r.Bytes < 4096 {
		t.Fatal("live recording failed", r.Err, r.Bytes)
	}
	t.Logf("saved %d original FLV bytes", r.Bytes)
	if probe, err := exec.LookPath("ffprobe"); err == nil {
		cmd := exec.CommandContext(ctx, probe, "-v", "error", "-show_entries", "stream=codec_name,width,height", "-of", "json", r.Path)
		out, err := cmd.Output()
		if err != nil {
			t.Fatal("recorded FLV was not readable", err)
		}
		t.Log(string(out))
	}
}
