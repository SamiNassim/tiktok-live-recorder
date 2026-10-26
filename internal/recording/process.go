package recording

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
	"unicode"

	"tiktok-live-recorder/internal/ffmpeg"
	"tiktok-live-recorder/internal/state"
)

// OutputArgs deliberately accepts output options only. Input and destination
// paths are app-owned; no shell, executable, extra input, or extra output can be
// entered through this field. Filter scripts and file attachments are excluded.
func OutputArgs(raw string) ([]string, error) {
	var args []string
	var token strings.Builder
	var quote rune
	escape, started := false, false
	for _, r := range raw {
		if escape {
			token.WriteRune(r)
			escape = false
			started = true
			continue
		}
		if r == '\\' && quote != '\'' {
			escape = true
			continue
		}
		if quote != 0 {
			if r == quote {
				quote = 0
			} else {
				token.WriteRune(r)
			}
			started = true
			continue
		}
		if r == '\'' || r == '"' {
			quote = r
			started = true
			continue
		}
		if unicode.IsSpace(r) {
			if started {
				args = append(args, token.String())
				token.Reset()
				started = false
			}
			continue
		}
		token.WriteRune(r)
		started = true
	}
	if quote != 0 || escape {
		return nil, errors.New("FFmpeg output options contain an unfinished quote or escape")
	}
	if started {
		args = append(args, token.String())
	}
	flags := map[string]bool{"-an": true, "-vn": true, "-sn": true, "-dn": true, "-shortest": true, "-bitexact": true}
	values := map[string]bool{"-c": true, "-codec": true, "-c:v": true, "-c:a": true, "-c:s": true, "-codec:v": true, "-codec:a": true, "-map": true, "-map_metadata": true, "-map_chapters": true, "-b:v": true, "-b:a": true, "-crf": true, "-preset": true, "-tune": true, "-movflags": true, "-fflags": true, "-flvflags": true, "-vf": true, "-af": true, "-r": true, "-s": true, "-pix_fmt": true, "-profile:v": true, "-level:v": true, "-maxrate": true, "-bufsize": true, "-ar": true, "-ac": true, "-metadata": true, "-disposition:v": true, "-disposition:a": true, "-bsf:v": true, "-bsf:a": true, "-f": true, "-fps_mode": true, "-tag:v": true, "-tag:a": true, "-cluster_time_limit": true, "-cluster_size_limit": true, "-flush_packets": true}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if flags[arg] {
			continue
		}
		if !values[arg] {
			return nil, fmt.Errorf("%s is not a supported output option; input and output paths are managed by the app", arg)
		}
		i++
		if i >= len(args) || args[i] == "" {
			return nil, fmt.Errorf("%s needs a value", arg)
		}
		if strings.ContainsAny(args[i], "\r\n\x00") || strings.Contains(args[i], "{input}") || strings.Contains(args[i], "{output}") {
			return nil, errors.New("enter output options only, without paths or placeholders")
		}
		if arg == "-f" && args[i] != "matroska" && args[i] != "mp4" && args[i] != "mpegts" {
			return nil, errors.New("output formats are limited to matroska, mp4, or mpegts; playlists and multiple-file formats are not supported")
		}
		if arg == "-vf" || arg == "-af" { // Filters are expressions, not alternate file inputs.
			lower := strings.ToLower(args[i])
			if strings.Contains(lower, "movie=") || strings.Contains(lower, "filename=") || strings.Contains(lower, "sendcmd") || strings.Contains(lower, "file=") {
				return nil, errors.New("filters cannot read external files")
			}
		}
	}
	return args, nil
}

func Process(parent context.Context, source string, cfg state.Config) (string, error) {
	args, err := OutputArgs(cfg.FFmpegArgs)
	if err != nil {
		return "", err
	}
	path, err := ffmpeg.Find(cfg.FFmpegPath)
	if err != nil {
		return "", err
	}
	if cfg.OutputExtension != "mp4" && cfg.OutputExtension != "mkv" && cfg.OutputExtension != "ts" {
		return "", errors.New("invalid processed output format")
	}
	target := strings.TrimSuffix(source, filepath.Ext(source)) + ".processed." + cfg.OutputExtension
	if _, err := os.Lstat(target); !errors.Is(err, os.ErrNotExist) {
		return "", errors.New("the processed output already exists or cannot be checked; the raw FLV was kept")
	}
	stage, err := os.MkdirTemp(filepath.Dir(target), ".processing-")
	if err != nil {
		return "", errors.New("a processing folder could not be created; the raw FLV was kept")
	}
	defer os.RemoveAll(stage)
	staged := filepath.Join(stage, "recording."+cfg.OutputExtension)
	ctx, cancel := context.WithTimeout(parent, 30*time.Minute)
	defer cancel()
	argv := []string{"-hide_banner", "-loglevel", "error", "-nostdin", "-n"}
	if cfg.DeleteRawAfterProcessing {
		argv = append(argv, "-xerror")
	}
	argv = append(argv, "-i", source)
	argv = append(argv, args...)
	argv = append(argv, staged)
	cmd := exec.CommandContext(ctx, path, argv...)
	configureCommand(cmd)
	var stderr tailBuffer
	cmd.Stderr = &stderr
	if err = cmd.Run(); err != nil {
		return "", errors.New("FFmpeg post-processing failed; the raw FLV was kept. Check your output options and FFmpeg installation")
	}
	if info, err := os.Stat(staged); err != nil || info.Size() == 0 {
		return "", errors.New("FFmpeg produced no processed copy; the raw FLV was kept")
	}
	if cfg.DeleteRawAfterProcessing {
		// Read every output packet before allowing cleanup. Stream copying
		// validates the container without re-encoding or decoding a long live.
		verify := exec.CommandContext(ctx, path, "-hide_banner", "-loglevel", "error", "-nostdin", "-xerror", "-i", staged, "-map", "0:v?", "-map", "0:a?", "-c", "copy", "-f", "null", "-")
		configureCommand(verify)
		verify.Stderr = &stderr
		if err := verify.Run(); err != nil {
			return "", errors.New("the processed copy could not be verified; the raw FLV was kept")
		}
	}
	if err := syncProcessedCopy(staged, false); err != nil {
		return "", errors.New("the processed copy could not be safely saved; the raw FLV was kept")
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	// A hard link publishes the finished file without replacing an existing
	// destination. Filesystems without hard links use an exclusive copy instead.
	if err := os.Link(staged, target); err != nil {
		if errors.Is(err, os.ErrExist) {
			return "", errors.New("the processed output already exists; the raw FLV was kept")
		}
		if err := copyProcessedExclusive(ctx, staged, target); err != nil {
			return "", errors.New("the processed copy could not be published; the raw FLV was kept")
		}
	}
	if err := syncProcessedCopy(target, cfg.DeleteRawAfterProcessing); err != nil {
		return "", errors.New("the processed copy could not be safely saved; the raw FLV was kept")
	}
	return target, nil
}

func copyProcessedExclusive(ctx context.Context, source, target string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	defer input.Close()
	output, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	// Copy in bounded chunks so shutdown also cancels publication on filesystems
	// that cannot hard-link. A failed partial output never authorizes cleanup.
	buf := make([]byte, 128<<10)
	for err == nil {
		if err = ctx.Err(); err != nil {
			break
		}
		var n int
		n, err = input.Read(buf)
		if n > 0 {
			if _, writeErr := output.Write(buf[:n]); writeErr != nil {
				err = writeErr
				break
			}
		}
	}
	if errors.Is(err, io.EOF) {
		err = output.Sync()
	}
	return errors.Join(err, output.Close())
}

func syncProcessedCopy(path string, syncParent bool) error {
	before, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !before.Mode().IsRegular() || before.Size() == 0 {
		return errors.New("the processed copy is not a regular nonempty file")
	}
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	info, statErr := f.Stat()
	if statErr != nil || !os.SameFile(before, info) {
		f.Close()
		return errors.New("the processed copy changed before synchronization")
	}
	if err := f.Chmod(0600); err != nil {
		f.Close()
		return err
	}
	err = errors.Join(f.Sync(), f.Close())
	if err != nil || !syncParent || runtime.GOOS == "windows" {
		return err // Windows does not support directory Sync through os.File.
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	return errors.Join(dir.Sync(), dir.Close())
}

type tailBuffer struct {
	mu   sync.Mutex
	data []byte
}

func (b *tailBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.data = append(b.data, p...)
	if len(b.data) > 8192 {
		b.data = append([]byte(nil), b.data[len(b.data)-8192:]...)
	}
	return len(p), nil
}
func (b *tailBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return strings.TrimSpace(string(b.data))
}
