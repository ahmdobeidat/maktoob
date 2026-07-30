// Package audio converts source media into the format whisper expects.
//
// ffmpeg is invoked as a subprocess rather than linked, so maktoob stays a
// pure-Go binary with a documented system dependency instead of a cgo build.
package audio

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Whisper resamples internally to 16 kHz mono, so feeding it anything else just
// moves the same work into the inference process.
const (
	SampleRate = 16000
	Channels   = 1
)

// ConvertTimeout bounds the ffmpeg call. Conversion of a voice note is a
// sub-second operation; anything approaching this bound means ffmpeg is stuck
// on malformed input, and an unbounded subprocess would stall the queue.
const ConvertTimeout = 60 * time.Second

// Converter runs ffmpeg. The indirection exists so tests can substitute a fake
// rather than requiring ffmpeg on the machine running them.
type Converter interface {
	// ToWAV transcodes src into a 16 kHz mono 16-bit WAV at dst.
	ToWAV(ctx context.Context, src, dst string) error
	// DurationMS reports the media duration in milliseconds.
	DurationMS(ctx context.Context, src string) (int64, error)
}

// FFmpeg is the real Converter.
type FFmpeg struct {
	// Bin is the ffmpeg executable. Empty means "ffmpeg" from PATH.
	Bin string
	// ProbeBin is the ffprobe executable. Empty means "ffprobe" from PATH.
	ProbeBin string
}

func (f FFmpeg) bin() string {
	if f.Bin != "" {
		return f.Bin
	}
	return "ffmpeg"
}

func (f FFmpeg) probeBin() string {
	if f.ProbeBin != "" {
		return f.ProbeBin
	}
	return "ffprobe"
}

// ToWAV transcodes src into a 16 kHz mono 16-bit WAV at dst, overwriting it.
func (f FFmpeg) ToWAV(ctx context.Context, src, dst string) error {
	ctx, cancel := context.WithTimeout(ctx, ConvertTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, f.bin(),
		"-hide_banner", "-loglevel", "error",
		"-y",
		"-i", src,
		"-ar", strconv.Itoa(SampleRate),
		"-ac", strconv.Itoa(Channels),
		"-c:a", "pcm_s16le",
		dst,
	)

	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return fmt.Errorf("ffmpeg timed out after %s converting %s",
				ConvertTimeout, filepath.Base(src))
		}
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return fmt.Errorf("ffmpeg: %s", redactPaths(msg, src, dst))
	}

	// ffmpeg creates the output at the process umask, typically 0644. This file
	// is the complete audio of a private message, and the rest of the project
	// is careful to keep that at 0600 — the database says so in a comment
	// several lines long. The enclosing directory is 0700, which saves this in
	// practice, but relying on that leaves the guarantee one refactor away from
	// being false.
	if err := os.Chmod(dst, 0o600); err != nil {
		return fmt.Errorf("restrict converted audio: %w", err)
	}
	return nil
}

// redactPaths replaces absolute paths with their file names.
//
// ffmpeg names its input in diagnostics, and that message is stored on the note
// and rendered into JSON and Markdown exports — files a user may hand to
// someone else. An export that discloses the reader's home directory layout
// contradicts the one promise this project makes. The paths are known here, so
// they are replaced by name rather than guessed at with a pattern.
func redactPaths(msg string, paths ...string) string {
	for _, p := range paths {
		if p == "" {
			continue
		}
		msg = strings.ReplaceAll(msg, p, filepath.Base(p))
	}
	return msg
}

// DurationMS reports the media duration in milliseconds.
//
// A duration of zero is returned without error when ffprobe cannot determine
// one. Duration is used for display and for scaling the transcription timeout,
// neither of which should fail an otherwise good note.
func (f FFmpeg) DurationMS(ctx context.Context, src string) (int64, error) {
	ctx, cancel := context.WithTimeout(ctx, ConvertTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, f.probeBin(),
		"-v", "error",
		"-show_entries", "format=duration",
		"-of", "default=noprint_wrappers=1:nokey=1",
		src,
	)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return 0, fmt.Errorf("ffprobe: %s", redactPaths(msg, src))
	}

	text := strings.TrimSpace(stdout.String())
	if text == "" || text == "N/A" {
		return 0, nil
	}

	seconds, err := strconv.ParseFloat(text, 64)
	if err != nil {
		return 0, nil
	}
	return int64(seconds * 1000), nil
}
