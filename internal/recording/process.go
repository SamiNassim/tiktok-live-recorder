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
	"slices"
	"strings"
	"sync"
	"unicode"

	"tiktok-live-recorder/internal/fault"
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
	values := map[string]bool{"-c": true, "-codec": true, "-c:v": true, "-c:a": true, "-c:s": true, "-codec:v": true, "-codec:a": true, "-map": true, "-map_metadata": true, "-map_chapters": true, "-b:v": true, "-b:a": true, "-crf": true, "-preset": true, "-tune": true, "-movflags": true, "-fflags": true, "-flvflags": true, "-vf": true, "-af": true, "-r": true, "-s": true, "-pix_fmt": true, "-profile:v": true, "-level:v": true, "-maxrate": true, "-bufsize": true, "-ar": true, "-ac": true, "-metadata": true, "-disposition:v": true, "-disposition:a": true, "-bsf:v": true, "-bsf:a": true, "-f": true, "-fps_mode": true, "-tag:v": true, "-tag:a": true, "-flush_packets": true}
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
		if arg == "-f" {
			switch args[i] {
			case "mp4", "matroska", "mpegts":
				args[i] = "mp4" // Old saved options can explicitly select a retired format.
			default:
				return nil, errors.New("processed copies use MP4")
			}
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

// ProcessedCopy reports whether automatic recovery re-encoded the source.
type ProcessedCopy struct {
	Path      string
	Reencoded bool
}

var ffmpegBase = []string{"-hide_banner", "-loglevel", "error", "-nostdin", "-n"}

func Process(ctx context.Context, source string, cfg state.Config) (ProcessedCopy, error) {
	return ProcessWithProgress(ctx, source, cfg, nil)
}

// ProcessWithProgress reports phase changes and FFmpeg's actual media clock.
// It shares Process's publication, verification and raw-preservation rules.
func ProcessWithProgress(ctx context.Context, source string, cfg state.Config, report func(Progress)) (ProcessedCopy, error) {
	reportPhase(report, "Preparing conversion")
	args, err := OutputArgs(cfg.FFmpegArgs)
	if err != nil {
		return ProcessedCopy{}, fault.Action("could not create the processed copy; check the custom FFmpeg output options. Raw FLV kept", err)
	}
	path, err := ffmpeg.Find(cfg.FFmpegPath)
	if err != nil {
		return ProcessedCopy{}, fault.Action("could not create the processed copy; install or select FFmpeg in Settings. Raw FLV kept", err)
	}
	target, err := nextProcessedPath(source)
	if err != nil {
		return ProcessedCopy{}, fault.Action("could not choose a separate processed output; raw FLV kept", err)
	}
	stage, err := os.MkdirTemp(filepath.Dir(target), ".processing-")
	if err != nil {
		return ProcessedCopy{}, fault.Action("could not create a processing folder; check disk space and folder permissions. Raw FLV kept", err)
	}
	defer os.RemoveAll(stage)
	staged := filepath.Join(stage, "recording.mp4")

	outputArgs := []string{"-map", "0:v?", "-map", "0:a?", "-c", "copy", "-movflags", "+faststart"}
	if len(args) > 0 {
		outputArgs = args
	}
	var codecChange *videoCodecChange
	var rewind *flvTimestampRewind
	var truncated *flvTruncatedTail
	err = copyFLV(ctx, path, source, staged, outputArgs, report)
	reencoded := false
	if err == nil {
		err = verifyCopyProgress(ctx, path, staged, report)
	}
	// The original remains available even when recovery needs to change the
	// encoded picture or repair the timeline.
	if ctx.Err() == nil && err != nil && (errors.As(err, &codecChange) || errors.As(err, &rewind) || errors.As(err, &truncated) || canRecoverMP4(err)) {
		if removeErr := os.Remove(staged); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			return ProcessedCopy{}, fault.Action("Could not clear the incomplete MP4. Raw FLV kept", errors.Join(removeErr, err))
		}
		recovered := filepath.Join(stage, "recovered.mp4")
		if recoverErr := recoverMP4Progress(ctx, path, source, recovered, report); recoverErr != nil {
			if ctx.Err() != nil {
				return ProcessedCopy{}, processingFailure(ctx.Err(), recoverErr, "", "recover the processed copy")
			}
			return ProcessedCopy{}, fault.Action("Could not recover the recording as MP4. Raw FLV kept", errors.Join(recoverErr, err))
		}
		reencoded = true
		if len(args) > 0 {
			custom := filepath.Join(stage, "custom.mp4")
			if err := runFFmpegProgress(ctx, path, nil, "apply the custom MP4 options", report, append(append([]string{"-xerror", "-i", recovered}, args...), custom)...); err != nil {
				return ProcessedCopy{}, err
			}
			recovered = custom
		}
		staged = recovered
		err = verifyCopyProgress(ctx, path, staged, report)
	}
	if err != nil {
		return ProcessedCopy{}, err
	}
	reportPhase(report, "Saving MP4")
	if err := syncProcessedCopy(staged, false); err != nil {
		return ProcessedCopy{}, fault.Action("the processed copy could not be safely synchronized to disk; raw FLV kept", err)
	}
	if err := ctx.Err(); err != nil {
		return ProcessedCopy{}, processingFailure(err, err, "", "publish the processed copy")
	}
	// A hard link publishes the finished file without replacing an existing
	// destination. Filesystems without hard links use an exclusive copy instead.
	if err := os.Link(staged, target); err != nil {
		if errors.Is(err, os.ErrExist) {
			return ProcessedCopy{}, fault.Action("the processed output already exists; raw FLV kept", err)
		}
		if err := copyProcessedExclusive(ctx, staged, target); err != nil {
			if ctx.Err() != nil {
				return ProcessedCopy{}, processingFailure(ctx.Err(), err, "", "publish the processed copy")
			}
			return ProcessedCopy{}, fault.Action("could not publish the processed copy; check disk space and folder permissions. Raw FLV kept", err)
		}
	}
	if err := syncProcessedCopy(target, cfg.DeleteRawAfterProcessing && !reencoded); err != nil {
		return ProcessedCopy{}, fault.Action("the published processed copy could not be safely synchronized to disk; raw FLV kept", err)
	}
	return ProcessedCopy{Path: target, Reencoded: reencoded}, nil
}

func nextProcessedPath(source string) (string, error) {
	base := strings.TrimSuffix(source, filepath.Ext(source))
	for n := 1; n <= 9999; n++ {
		suffix := ".processed.mp4"
		if n > 1 {
			suffix = fmt.Sprintf(".processed-%d.mp4", n)
		}
		path := base + suffix
		if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
			return path, nil
		} else if err != nil {
			return "", err
		}
	}
	return "", errors.New("too many processed copies already exist")
}

func copyFLV(ctx context.Context, path, source, staged string, args []string, report func(Progress)) error {
	input, err := processingInput(ctx, source)
	if err != nil {
		return fault.Action("could not read the raw FLV for processing; raw FLV kept", err)
	}
	defer input.Close()
	argv := []string{"-xerror", "-i", "pipe:0"}
	argv = append(argv, args...)
	argv = append(argv, staged)
	stdin := &processingReader{Reader: input}
	return runFFmpegProgress(ctx, path, stdin, "create the processed copy", report, argv...)
}

func runFFmpeg(ctx context.Context, path string, input *processingReader, operation string, args ...string) error {
	return runFFmpegProgress(ctx, path, input, operation, nil, args...)
}

func runFFmpegProgress(ctx context.Context, path string, input *processingReader, operation string, report func(Progress), args ...string) error {
	base := slices.Clone(ffmpegBase)
	phase := processingPhase(operation)
	reportPhase(report, phase)
	if report != nil {
		base = append(base, "-progress", "pipe:1", "-nostats")
	}
	cmd := exec.CommandContext(ctx, path, append(base, args...)...)
	if report != nil {
		cmd.Stdout = &progressWriter{report: report, value: Progress{Phase: phase}}
	}
	configureCommand(cmd)
	if input != nil {
		cmd.Stdin = input
	}
	var stderr tailBuffer
	cmd.Stderr = &stderr
	err := cmd.Run()
	details := stderr.String()
	if input != nil && input.err != nil && ctx.Err() == nil {
		// Cmd.Run can return the exit error in place of a read error.
		if errors.Is(err, input.err) {
			err = nil
		}
		return fault.Action("Could not prepare the raw FLV for FFmpeg. Raw FLV kept",
			errors.Join(input.err, ffmpegCause(err, details)))
	}
	if err == nil && details != "" {
		err = errors.New("FFmpeg reported errors")
	}
	if err != nil {
		return processingFailure(ctx.Err(), err, details, operation)
	}
	return nil
}

func verifyCopy(ctx context.Context, path, staged string) error {
	return verifyCopyProgress(ctx, path, staged, nil)
}

func verifyCopyProgress(ctx context.Context, path, staged string, report func(Progress)) error {
	if info, err := os.Stat(staged); err != nil || info.Size() == 0 {
		return fault.Action("FFmpeg produced no processed copy; raw FLV kept", err)
	}
	return runFFmpegProgress(ctx, path, nil, "verify the processed copy", report, "-xerror", "-err_detect", "explode", "-abort_on", "empty_output+empty_output_stream", "-i", staged, "-map", "0:v?", "-map", "0:a?", "-fps_mode", "passthrough", "-enc_time_base:v", "demux", "-f", "null", "-")
}

type processingReader struct {
	io.Reader
	err error
}

func (r *processingReader) Read(p []byte) (int, error) {
	n, err := r.Reader.Read(p)
	if err != nil && !errors.Is(err, io.EOF) {
		r.err = err
	}
	return n, err
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
	if err = errors.Join(err, output.Close()); err != nil {
		return errors.Join(err, os.Remove(target))
	}
	return nil
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
