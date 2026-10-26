package recording

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestExclusivePublicationPreservesExistingFilesAndCancellation(t *testing.T) {
	for _, name := range []string{"success", "existing", "canceled"} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			source, target := filepath.Join(dir, "staged.mkv"), filepath.Join(dir, "processed.mkv")
			payload := bytes.Repeat([]byte("synthetic media"), 20000)
			if err := os.WriteFile(source, payload, 0600); err != nil {
				t.Fatal(err)
			}
			if name == "existing" {
				if err := os.WriteFile(target, []byte("previous recording"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if name == "canceled" {
				cancel()
			}
			err := copyProcessedExclusive(ctx, source, target)
			if (err == nil) != (name == "success") {
				t.Fatal("wrong publication result", err)
			}
			if got, err := os.ReadFile(source); err != nil || !bytes.Equal(got, payload) {
				t.Fatal("publication changed its source", err)
			}
			switch name {
			case "success":
				if got, err := os.ReadFile(target); err != nil || !bytes.Equal(got, payload) {
					t.Fatal("published copy differs from its source", err)
				}
			case "existing":
				if got, err := os.ReadFile(target); err != nil || string(got) != "previous recording" {
					t.Fatal("publication overwrote an existing recording", err)
				}
			case "canceled":
				if _, err := os.Stat(target); !os.IsNotExist(err) {
					t.Fatal("publication started after cancellation", err)
				}
			}
		})
	}
}

func TestProcessedCopySyncRejectsEmptyFilesDirectoriesAndLinks(t *testing.T) {
	for _, name := range []string{"empty", "directory", "symlink"} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "copy.mkv")
			switch name {
			case "empty":
				if err := os.WriteFile(path, nil, 0600); err != nil {
					t.Fatal(err)
				}
			case "directory":
				if err := os.Mkdir(path, 0700); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				if err := os.WriteFile(path+".saved", []byte("media"), 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(path+".saved", path); err != nil {
					t.Skip("symlinks unavailable", err)
				}
			}
			if err := syncProcessedCopy(path, true); err == nil {
				t.Fatal("invalid copy was accepted for raw cleanup")
			}
		})
	}
}
