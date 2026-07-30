package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"

	"github.com/ahmdobeidat/maktoob/internal/pipeline"
	"github.com/ahmdobeidat/maktoob/internal/store"
	"github.com/ahmdobeidat/maktoob/internal/wa"
)

// The seam from a WhatsApp event to a claimable row runs listener -> sink ->
// pipeline -> store, and that path is the entire product. Every package on it
// is tested against mocks of its neighbours, which is exactly the arrangement
// that lets a whole-system defect hide: a nil Downloader panicking the worker,
// for instance, was invisible to every per-package test because each of them
// set the field.
//
// cmd/maktoob is the only package permitted to import both internal/wa and
// internal/pipeline, so it is the only place this can be written.

// seamPhone is the sender's number. Its digits are what must not survive into
// the database.
const seamPhone = "962790000000"

// stubDownloader stands in for *whatsmeow.Client. The real one talks to Meta's
// CDN, which is the one thing a test of this seam must not do.
type stubDownloader struct {
	data []byte
	err  error
}

func (d stubDownloader) Download(ctx context.Context, msg whatsmeow.DownloadableMessage) ([]byte, error) {
	return d.data, d.err
}

// stubConverter stands in for ffmpeg. Only DurationMS runs on the ingest path;
// ToWAV belongs to transcription, which this test does not reach.
type stubConverter struct{}

func (stubConverter) ToWAV(ctx context.Context, src, dst string) error { return nil }

func (stubConverter) DurationMS(ctx context.Context, src string) (int64, error) {
	return 7000, nil
}

// waVoiceNote builds the event whatsmeow would hand us for an incoming push-to-
// talk note. internal/wa's own test helpers are not importable across packages,
// so the shape is rebuilt here from the same fields.
func waVoiceNote(messageID string) *events.Message {
	sender := types.JID{User: seamPhone, Server: types.DefaultUserServer}
	return &events.Message{
		Info: types.MessageInfo{
			MessageSource: types.MessageSource{
				Chat:   sender,
				Sender: sender,
			},
			ID:        messageID,
			PushName:  "Um Ahmad",
			Timestamp: time.Now().Add(-time.Minute),
		},
		Message: &waE2E.Message{AudioMessage: &waE2E.AudioMessage{
			PTT:           proto.Bool(true),
			Mimetype:      proto.String("audio/ogg; codecs=opus"),
			Seconds:       proto.Uint32(7),
			DirectPath:    proto.String("/v/t62.7117-24/x"),
			MediaKey:      []byte("key"),
			FileSHA256:    []byte("sha"),
			FileEncSHA256: []byte("enc"),
		}},
	}
}

// seam wires the real store, the real pipeline and the real sink adapter behind
// a listener, and returns a channel that fires once per delivered note so the
// test never has to poll or sleep.
func seam(t *testing.T, down wa.Downloader) (*store.Store, *wa.Listener, string, <-chan error) {
	t.Helper()
	dir := t.TempDir()

	st, err := store.Open(filepath.Join(dir, "maktoob.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	mediaDir := filepath.Join(dir, "media")
	pl := &pipeline.Pipeline{
		Store:     st,
		Converter: stubConverter{},
		MediaDir:  mediaDir,
		Model:     "test",
	}

	salt, err := wa.LoadSalt(filepath.Join(dir, "salt"))
	if err != nil {
		t.Fatal(err)
	}

	// The production adapter, wrapped only to signal completion. The wrapper
	// forwards to newWASink rather than reimplementing it, so the mapping under
	// test is the one the binary uses.
	delivered := make(chan error, 4)
	adapter := newWASink(pl, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	sink := wa.SinkFunc(func(ctx context.Context, n wa.VoiceNote) error {
		err := adapter(ctx, n)
		delivered <- err
		return err
	})

	l := &wa.Listener{Down: down, Sink: sink, Salt: salt}
	ctx, cancel := context.WithCancel(context.Background())
	if err := l.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cancel(); l.Close() })

	return st, l, mediaDir, delivered
}

func awaitDelivery(t *testing.T, delivered <-chan error) {
	t.Helper()
	select {
	case err := <-delivered:
		if err != nil {
			t.Fatalf("the sink rejected the note: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("no note reached the sink")
	}
}

func TestVoiceNoteReachesTheQueueWithoutLeakingTheNumber(t *testing.T) {
	st, l, mediaDir, delivered := seam(t, stubDownloader{data: []byte("ogg-bytes")})
	ctx := context.Background()

	l.HandleEvent(waVoiceNote("3EB0SEAM01"))
	awaitDelivery(t, delivered)

	notes, err := st.ListNotes(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(notes) != 1 {
		t.Fatalf("got %d notes, want 1", len(notes))
	}
	note := notes[0]

	if note.Status != store.StatusPending {
		t.Fatalf("status is %q, want %q", note.Status, store.StatusPending)
	}
	if note.Source != "whatsapp" {
		t.Fatalf("source is %q, want whatsapp", note.Source)
	}

	// The alias is the whole privacy argument. Nothing derived from the JID may
	// survive into a column, and the check is on every identifier the row
	// carries rather than just the sender, because a leak anywhere in the row is
	// the same leak.
	assertNoNumber(t, "sender", note.Sender)
	assertNoNumber(t, "chat id", note.ChatID)
	assertNoNumber(t, "media path", note.MediaPath)
	assertNoNumber(t, "chat name", note.ChatName)
	if note.Sender == "" {
		t.Fatal("sender alias is empty, so nothing was aliased at all")
	}

	// Media must be readable only by the owner. The database is the transcript;
	// this file is the audio, and it is just as private.
	fi, err := os.Stat(note.MediaPath)
	if err != nil {
		t.Fatalf("media file: %v", err)
	}
	if got := fi.Mode().Perm(); got != 0o600 {
		t.Errorf("media file is %04o, want 0600", got)
	}
	if filepath.Dir(note.MediaPath) != mediaDir {
		t.Errorf("media landed in %s, want %s", filepath.Dir(note.MediaPath), mediaDir)
	}

	// The queue is the notes table, so a note that is not claimable is a note
	// that will never be transcribed no matter how correct the row looks.
	claimed, err := st.ClaimNext(ctx)
	if err != nil {
		t.Fatalf("ClaimNext refused a pending note with media: %v", err)
	}
	if claimed.ID != note.ID {
		t.Fatalf("ClaimNext returned %s, want %s", claimed.ID, note.ID)
	}
}

func TestFailedDownloadIsRecordedAndNeverClaimed(t *testing.T) {
	st, l, _, delivered := seam(t, stubDownloader{err: errors.New("410 gone")})
	ctx := context.Background()

	l.HandleEvent(waVoiceNote("3EB0SEAM02"))
	awaitDelivery(t, delivered)

	notes, err := st.ListNotes(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(notes) != 1 {
		t.Fatalf("got %d notes, want 1: a note whatsmeow already acked must be recorded even when its audio is gone", len(notes))
	}
	note := notes[0]

	if note.Status != store.StatusFailed {
		t.Fatalf("status is %q, want %q", note.Status, store.StatusFailed)
	}
	if note.MediaPath != "" {
		t.Fatalf("media path is %q, want empty: there is no audio on disk", note.MediaPath)
	}
	if note.Error == "" {
		t.Fatal("no error recorded, so the user is shown a blank note with no explanation")
	}
	assertNoNumber(t, "sender", note.Sender)

	// A media-less row must never be claimed: the transcriber would hand ffmpeg
	// an empty path. The row exists so the user learns the note arrived, not so
	// the worker can choke on it.
	if _, err := st.ClaimNext(ctx); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("ClaimNext returned %v, want ErrNotFound for a media-less failed note", err)
	}
}

// leakReason reports why value looks like it carries the sender's identity, or
// "" if it does not.
//
// The aliases are hex, so they contain decimal digits by construction and a
// literal "contains none of the digits" check is not expressible. A run of
// seven consecutive digits from the number is: at 16^-7 per position it will
// not occur by chance in a 64-character hex string at any rate this test would
// ever notice, and a fragment that long is what a real leak looks like.
func leakReason(value string) string {
	if value == "" {
		return ""
	}
	if strings.Contains(value, seamPhone) {
		return "contains the phone number outright"
	}
	if strings.Contains(value, "@"+string(types.DefaultUserServer)) {
		return "contains a raw JID"
	}

	const run = 7
	for i := 0; i+run <= len(seamPhone); i++ {
		if frag := seamPhone[i : i+run]; strings.Contains(value, frag) {
			return "leaks " + frag + " from the phone number"
		}
	}
	return ""
}

func assertNoNumber(t *testing.T, field, value string) {
	t.Helper()
	if reason := leakReason(value); reason != "" {
		t.Fatalf("%s %s: %q", field, reason, value)
	}
}

// The privacy assertion above is only worth as much as the detector behind it.
// A detector that silently never fires is worse than no check, because it reads
// like coverage.
func TestLeakDetectorCatchesWhatItClaimsTo(t *testing.T) {
	jid := seamPhone + "@" + string(types.DefaultUserServer)

	leaky := map[string]string{
		"the bare number":     seamPhone,
		"a raw JID":           jid,
		"a JID with a device": seamPhone + ".1:3@" + string(types.DefaultUserServer),
		"an embedded number":  "chat-" + seamPhone + "-x",
		"a seven digit run":   "prefix9627900suffix",
	}
	for name, value := range leaky {
		if leakReason(value) == "" {
			t.Errorf("%s (%q) was not detected as a leak", name, value)
		}
	}

	clean := map[string]string{
		"empty":            "",
		"a real alias":     "3f1c9a04b7e25d6188aa0c53f9e4d71b2a6c8e0f4d19b3576ce8a2401fd76b9c",
		"a short overlap":  "962790",
		"an unrelated uid": "a1b2c3d4-e5f6-4a7b-8c9d-0e1f2a3b4c5d",
	}
	for name, value := range clean {
		if reason := leakReason(value); reason != "" {
			t.Errorf("%s (%q) was wrongly flagged: %s", name, value, reason)
		}
	}
}
