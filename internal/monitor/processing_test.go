package monitor

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"tiktok-live-recorder/internal/state"
)

func TestRawCleanupRechecksFilesAndCancellation(t *testing.T) {
	for _, name := range []string{"success", "canceled", "raw replaced", "raw edited", "copy replaced", "copy edited", "copy missing", "empty copy", "same file", "raw symlink", "copy symlink", "not FLV"} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			raw, processed := filepath.Join(root, "source.flv"), filepath.Join(root, "copy.mkv")
			if name == "not FLV" {
				raw = filepath.Join(root, "source.mkv")
			}
			for _, path := range []string{raw, processed} {
				if err := os.WriteFile(path, []byte("original media"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if name == "same file" {
				if err := os.Remove(processed); err != nil {
					t.Fatal(err)
				}
				if err := os.Link(raw, processed); err != nil {
					t.Skip("hard links unavailable", err)
				}
			}
			if name == "raw symlink" || name == "copy symlink" {
				path := raw
				if name == "copy symlink" {
					path = processed
				}
				if err := os.Rename(path, path+".saved"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(path+".saved", path); err != nil {
					t.Skip("symlinks unavailable", err)
				}
			}
			original, err := os.Lstat(raw)
			if err != nil {
				t.Fatal(err)
			}
			copyInfo, err := os.Lstat(processed)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			switch name {
			case "canceled":
				cancel()
			case "raw replaced", "copy replaced":
				path := raw
				if name == "copy replaced" {
					path = processed
				}
				if err := os.Rename(path, path+".saved"); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte("original media"), 0600); err != nil {
					t.Fatal(err)
				}
			case "raw edited", "copy edited", "empty copy":
				path, data := raw, []byte("changed media payload")
				if name != "raw edited" {
					path = processed
				}
				if name == "empty copy" {
					data = nil
				}
				if err := os.WriteFile(path, data, 0600); err != nil {
					t.Fatal(err)
				}
			case "copy missing":
				if err := os.Remove(processed); err != nil {
					t.Fatal(err)
				}
			}
			err = removeProcessedRaw(ctx, raw, processed, original, copyInfo)
			if name == "success" {
				if err != nil {
					t.Fatal(err)
				}
				if _, err := os.Lstat(raw); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("original was not removed", err)
				}
				if b, err := os.ReadFile(processed); err != nil || string(b) != "original media" {
					t.Fatal("processed copy changed", err)
				}
			} else if err == nil {
				t.Fatal("unsafe cleanup was accepted")
			} else if _, err := os.Lstat(raw); err != nil {
				t.Fatal("unsafe cleanup removed the original", err)
			}
		})
	}
}

func TestProcessingCleanupHonorsErrorsAndSettingsChanges(t *testing.T) {
	for _, name := range []string{"success", "default keeps raw", "failed", "canceled", "cleanup disabled", "processing disabled", "history cleared", "raw replaced", "empty copy"} {
		t.Run(name, func(t *testing.T) {
			s, log := fixture(t)
			cfg := s.Config()
			cfg.PollingEnabled = false
			cfg.DeleteRawAfterProcessing = name != "default keeps raw"
			if err := s.SaveConfig(cfg); err != nil {
				t.Fatal(err)
			}
			root := t.TempDir()
			raw, processed := filepath.Join(root, "source.flv"), filepath.Join(root, "copy.mkv")
			if err := os.WriteFile(raw, []byte("original media"), 0600); err != nil {
				t.Fatal(err)
			}
			if err := s.Update(func(d *state.Data) error {
				d.Recordings = []state.Recording{{ID: "done", Path: raw, EndedAt: time.Now()}}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			e := &Engine{store: s, log: log, ctx: ctx, cancel: cancel, processing: make(chan struct{}, 2)}
			t.Cleanup(e.Close)
			e.process = func(context.Context, string, state.Config) (string, error) {
				if name == "failed" {
					return "", errors.New("processing failed")
				}
				payload := []byte("verified copy")
				if name == "empty copy" {
					payload = nil
				}
				if err := os.WriteFile(processed, payload, 0600); err != nil {
					return "", err
				}
				switch name {
				case "canceled":
					cancel()
				case "cleanup disabled", "processing disabled":
					latest := s.Config()
					if name == "cleanup disabled" {
						latest.DeleteRawAfterProcessing = false
					} else {
						latest.PostProcess = false
					}
					if err := s.SaveConfig(latest); err != nil {
						return "", err
					}
				case "history cleared":
					if _, err := s.ClearRecordingHistory(); err != nil {
						return "", err
					}
				case "raw replaced":
					if err := os.Rename(raw, raw+".saved"); err != nil {
						return "", err
					}
					if err := os.WriteFile(raw, []byte("replacement media"), 0600); err != nil {
						return "", err
					}
				}
				return processed, nil
			}
			e.processCopy("demo_creator", raw, cfg)
			done := make(chan struct{})
			go func() { e.wg.Wait(); close(done) }()
			select {
			case <-done:
			case <-time.After(10 * time.Second):
				t.Fatal("processing did not finish")
			}
			_, err := os.Stat(raw)
			if name == "success" {
				if !errors.Is(err, os.ErrNotExist) || !s.Data().Recordings[0].RawDeleted {
					t.Fatal("successful cleanup was not recorded", err)
				}
				reopened, err := state.Open(s.Directory())
				if err != nil {
					t.Fatal(err)
				}
				if n, err := reopened.PruneMissingRecordings(); err != nil || n != 0 || len(reopened.Data().Recordings) != 1 {
					t.Fatal("cleanup lost the recording from history", n, err)
				}
			} else if err != nil {
				t.Fatal("original was lost after unsuccessful or disabled cleanup", err)
			}
			if name == "history cleared" && len(s.Data().Recordings) != 0 {
				t.Fatal("processing restored cleared history")
			}
			if name == "empty copy" && s.Data().Recordings[0].ProcessedPath != "" {
				t.Fatal("an empty copy replaced the raw file as the playback path")
			}
		})
	}
}
