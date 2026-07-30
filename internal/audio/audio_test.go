package audio

import (
	"context"
	"encoding/binary"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// fixture is a three-second 48 kHz mono opus tone, committed so the conversion
// assertions below run against a real file rather than a mock. It is the shape
// a WhatsApp voice note arrives in.
const fixture = "../../testdata/fixtures/tone-3s.ogg"

// requireFFmpeg skips rather than fails when ffmpeg is absent.
//
// CI does not install it, and a test that fails there would either block every
// pull request or teach everyone to ignore a red check. What ffmpeg's absence
// means is "this assertion was not made", not "the code is broken".
func requireFFmpeg(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg is not installed; skipping conversion tests")
	}
}

// Whisper wants 16 kHz mono 16-bit PCM. Feeding it anything else does not fail
// loudly — whisper resamples internally — it just moves the work into inference
// and quietly changes the timings every segment is reported against.
func TestToWAVProducesWhatWhisperExpects(t *testing.T) {
	requireFFmpeg(t)

	dst := filepath.Join(t.TempDir(), "out.wav")
	if err := (FFmpeg{}).ToWAV(context.Background(), fixture, dst); err != nil {
		t.Fatalf("ToWAV: %v", err)
	}

	raw, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("read output: %v", err)
	}
	if len(raw) < 44 {
		t.Fatalf("output is %d bytes, too short to be a WAV", len(raw))
	}

	// Parsed from the header rather than shelled out to ffprobe, so the
	// assertion does not depend on a second tool being present and agreeing.
	if got := string(raw[0:4]); got != "RIFF" {
		t.Errorf("magic = %q, want RIFF", got)
	}
	if got := string(raw[8:12]); got != "WAVE" {
		t.Errorf("format = %q, want WAVE", got)
	}

	audioFormat := binary.LittleEndian.Uint16(raw[20:22])
	channels := binary.LittleEndian.Uint16(raw[22:24])
	sampleRate := binary.LittleEndian.Uint32(raw[24:28])
	bitsPerSample := binary.LittleEndian.Uint16(raw[34:36])

	if audioFormat != 1 {
		t.Errorf("audio format = %d, want 1 (uncompressed PCM)", audioFormat)
	}
	if channels != Channels {
		t.Errorf("channels = %d, want %d", channels, Channels)
	}
	if sampleRate != SampleRate {
		t.Errorf("sample rate = %d, want %d", sampleRate, SampleRate)
	}
	if bitsPerSample != 16 {
		t.Errorf("bits per sample = %d, want 16", bitsPerSample)
	}
}

// The converted wav is the complete audio of a private message. ffmpeg creates
// it at the process umask, typically 0644, while the database and the session
// store next to it are both 0600. The enclosing directory saves it in practice;
// relying on that leaves the guarantee one refactor from being false, and
// PRIVACY.md tells the reader the files are 0600.
func TestToWAVRestrictsPermissions(t *testing.T) {
	requireFFmpeg(t)
	if runtime.GOOS == "windows" {
		t.Skip("POSIX file modes do not apply on Windows")
	}

	dst := filepath.Join(t.TempDir(), "out.wav")
	if err := (FFmpeg{}).ToWAV(context.Background(), fixture, dst); err != nil {
		t.Fatalf("ToWAV: %v", err)
	}

	info, err := os.Stat(dst)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Errorf("converted audio is mode %04o, want 0600", got)
	}
}

func TestDurationMS(t *testing.T) {
	requireFFmpeg(t)

	got, err := (FFmpeg{}).DurationMS(context.Background(), fixture)
	if err != nil {
		t.Fatalf("DurationMS: %v", err)
	}
	// The fixture is three seconds; opus pads slightly, so this is a window
	// rather than an equality.
	if got < 2900 || got > 3200 {
		t.Errorf("duration = %dms, want roughly 3000ms", got)
	}
}

// A duration of zero is returned without error when ffprobe cannot determine
// one. Duration drives display and the transcription timeout, neither of which
// should fail an otherwise good note.
func TestDurationOfUnreadableInputIsNotFatal(t *testing.T) {
	requireFFmpeg(t)

	bad := filepath.Join(t.TempDir(), "not-audio.ogg")
	if err := os.WriteFile(bad, []byte("this is not audio"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	got, err := (FFmpeg{}).DurationMS(context.Background(), bad)
	if err == nil && got != 0 {
		t.Errorf("duration = %d on garbage input, want 0", got)
	}
}

// ffmpeg names its input in diagnostics, and that message is stored on the note
// and rendered into JSON and Markdown exports — files a user may hand to
// someone else. An export that discloses the reader's home directory layout
// contradicts the one promise this project makes.
func TestConversionErrorsDoNotLeakThePath(t *testing.T) {
	requireFFmpeg(t)

	dir := t.TempDir()
	src := filepath.Join(dir, "definitely-not-audio.ogg")
	if err := os.WriteFile(src, []byte("not audio at all"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	err := (FFmpeg{}).ToWAV(context.Background(), src, filepath.Join(dir, "out.wav"))
	if err == nil {
		t.Fatal("converting garbage succeeded")
	}

	if strings.Contains(err.Error(), dir) {
		t.Errorf("error discloses the directory it ran in: %v", err)
	}
	// The file name survives, because naming which note failed is the whole
	// point of the message.
	if !strings.Contains(err.Error(), "definitely-not-audio.ogg") {
		t.Errorf("error does not say which file failed: %v", err)
	}
}

// Pure unit, so it runs on CI where ffmpeg is absent.
func TestRedactPaths(t *testing.T) {
	for _, tc := range []struct {
		name  string
		msg   string
		paths []string
		want  string
	}{
		{
			name:  "replaces an absolute path with its file name",
			msg:   "/home/someone/data/media/abc.ogg: Invalid data found",
			paths: []string{"/home/someone/data/media/abc.ogg"},
			want:  "abc.ogg: Invalid data found",
		},
		{
			name:  "replaces every occurrence",
			msg:   "/a/b/c.ogg failed; retrying /a/b/c.ogg",
			paths: []string{"/a/b/c.ogg"},
			want:  "c.ogg failed; retrying c.ogg",
		},
		{
			name:  "handles several paths",
			msg:   "/in/a.ogg -> /out/b.wav failed",
			paths: []string{"/in/a.ogg", "/out/b.wav"},
			want:  "a.ogg -> b.wav failed",
		},
		{
			name:  "ignores empty paths",
			msg:   "something went wrong",
			paths: []string{""},
			want:  "something went wrong",
		},
		{
			name:  "leaves an unrelated message alone",
			msg:   "Invalid data found when processing input",
			paths: []string{"/some/other/file.ogg"},
			want:  "Invalid data found when processing input",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := redactPaths(tc.msg, tc.paths...); got != tc.want {
				t.Errorf("redactPaths = %q, want %q", got, tc.want)
			}
		})
	}
}

// A cancelled context must stop ffmpeg rather than leaving a subprocess behind.
func TestToWAVHonoursCancellation(t *testing.T) {
	requireFFmpeg(t)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	dst := filepath.Join(t.TempDir(), "out.wav")
	start := time.Now()
	if err := (FFmpeg{}).ToWAV(ctx, fixture, dst); err == nil {
		t.Fatal("conversion succeeded on a cancelled context")
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Errorf("cancellation took %s to take effect", elapsed)
	}
}

// The Converter interface exists so the pipeline can be tested without ffmpeg.
// This asserts the real implementation still satisfies it.
func TestFFmpegSatisfiesConverter(t *testing.T) {
	var _ Converter = FFmpeg{}
}
