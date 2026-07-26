package pipeline

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ahmdobeidat/maktoob/internal/store"
)

// fakeConverter stands in for ffmpeg. Duration is what ffprobe would report.
type fakeConverter struct {
	duration int64
	err      error
}

func (f fakeConverter) DurationMS(ctx context.Context, path string) (int64, error) {
	return f.duration, f.err
}

func (f fakeConverter) ToWAV(ctx context.Context, src, dst string) error { return nil }

func newTestPipeline(t *testing.T) (*Pipeline, *store.Store) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	return &Pipeline{
		Store:     st,
		Converter: fakeConverter{duration: 7000},
		MediaDir:  filepath.Join(dir, "media"),
	}, st
}

func TestIngestReaderDuplicateLeavesNoOrphan(t *testing.T) {
	p, _ := newTestPipeline(t)
	ctx := context.Background()

	req := IngestRequest{
		Source: "whatsapp", ChatID: "chat1", ChatName: "Family",
		Sender: "alias1", SenderName: "Um Ahmad",
		WAMessageID: "dedupe1", ReceivedAt: time.Now(), Ext: ".ogg",
	}

	first, created, err := p.IngestReader(ctx, strings.NewReader("audio"), req)
	if err != nil || !created {
		t.Fatalf("first ingest: created=%v err=%v", created, err)
	}

	second, created, err := p.IngestReader(ctx, strings.NewReader("audio"), req)
	if err != nil {
		t.Fatal(err)
	}
	if created {
		t.Fatal("second ingest with the same dedupe key reported created")
	}
	if second != first {
		t.Fatalf("duplicate returned id %q, want the existing %q", second, first)
	}

	entries, err := os.ReadDir(p.MediaDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("orphan media file: got %d files, want 1", len(entries))
	}
}

func TestIngestReaderNilAudioRecordsFailedNote(t *testing.T) {
	p, st := newTestPipeline(t)
	ctx := context.Background()

	id, created, err := p.IngestReader(ctx, nil, IngestRequest{
		Source: "whatsapp", ChatID: "chat1", ChatName: "Family",
		Sender: "alias1", WAMessageID: "dedupe2", ReceivedAt: time.Now(),
		Ext: ".ogg", Status: store.StatusFailed, Error: "media unavailable",
		DurationHint: 4000,
	})
	if err != nil || !created {
		t.Fatalf("created=%v err=%v", created, err)
	}

	note, _, err := st.GetNote(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if note.Status != store.StatusFailed {
		t.Fatalf("status: got %q, want failed", note.Status)
	}
	if note.MediaPath != "" {
		t.Fatalf("media_path: got %q, want empty", note.MediaPath)
	}
	if note.DurationMS != 4000 {
		t.Fatalf("duration: got %d, want the hint 4000", note.DurationMS)
	}

	// It must be invisible to the transcription worker.
	if _, err := st.ClaimNext(ctx); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("a media-less note was claimed: %v", err)
	}
}

func TestIngestKeepsWorkingForImports(t *testing.T) {
	p, st := newTestPipeline(t)
	ctx := context.Background()

	src := filepath.Join(t.TempDir(), "note.ogg")
	if err := os.WriteFile(src, []byte("audio"), 0o600); err != nil {
		t.Fatal(err)
	}

	id, err := p.Ingest(ctx, src)
	if err != nil {
		t.Fatal(err)
	}
	note, _, err := st.GetNote(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if note.Source != "import" || note.Status != store.StatusPending {
		t.Fatalf("got source=%q status=%q", note.Source, note.Status)
	}
	if filepath.Ext(note.MediaPath) != ".ogg" {
		t.Fatalf("extension lost: %q", note.MediaPath)
	}
}

func TestWriteMediaUsesOwnerOnlyMode(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "x.ogg")
	if err := writeMedia(dst, strings.NewReader("audio")); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(dst)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode: got %o, want 600", info.Mode().Perm())
	}
}

// TestStoreFailureLeavesNoOrphanMedia covers the gap between writeMedia
// succeeding and the note row existing.
//
// Nothing references the media file until CreateNote inserts the row naming it,
// so an error in between used to leave a file no query could ever reach. On the
// import path the user re-runs the command and it is survivable. On the
// WhatsApp path it is not: whatsmeow acked the message as it handed it over and
// will not redeliver, and the reader is deaf and cannot play the audio to
// recover what was said. One transient store failure is then permanent loss
// plus a media directory that only grows.
//
// The store is closed to force the failure. Pipeline.Store is a concrete
// *store.Store rather than an interface, so there is no failing implementation
// to substitute; a closed handle fails every write the same way a disk or lock
// problem would.
func TestStoreFailureLeavesNoOrphanMedia(t *testing.T) {
	p, st := newTestPipeline(t)
	ctx := context.Background()

	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	id, created, err := p.IngestReader(ctx, strings.NewReader("audio"), IngestRequest{
		Source: "whatsapp", ChatID: "chat1", ChatName: "Family",
		Sender: "alias1", WAMessageID: "dedupe1", ReceivedAt: time.Now(), Ext: ".ogg",
	})
	if err == nil {
		t.Fatal("a closed store did not surface an error")
	}
	if created || id != "" {
		t.Fatalf("failed ingest reported id=%q created=%v", id, created)
	}

	entries, err := os.ReadDir(p.MediaDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("a store failure orphaned media: %v", names)
	}
}

// The duplicate-key path removes the file and returns the existing note's id
// with a nil error. The cleanup added for store failures must not disturb that,
// and must not double-remove a path that branch has already handled.
func TestDuplicatePathIsUnchangedByFailureCleanup(t *testing.T) {
	p, _ := newTestPipeline(t)
	ctx := context.Background()

	req := IngestRequest{
		Source: "whatsapp", ChatID: "chat1", ChatName: "Family",
		Sender: "alias1", WAMessageID: "dedupe1", ReceivedAt: time.Now(), Ext: ".ogg",
	}

	first, created, err := p.IngestReader(ctx, strings.NewReader("audio"), req)
	if err != nil || !created {
		t.Fatalf("first ingest: created=%v err=%v", created, err)
	}

	second, created, err := p.IngestReader(ctx, strings.NewReader("audio"), req)
	if err != nil {
		t.Fatalf("the duplicate path must stay a nil-error path: %v", err)
	}
	if created {
		t.Fatal("duplicate reported created")
	}
	if second != first {
		t.Fatalf("duplicate returned %q, want the existing %q", second, first)
	}

	// The first note's file must still be there: it is the one the surviving row
	// points at.
	if _, err := os.Stat(filepath.Join(p.MediaDir, first+".ogg")); err != nil {
		t.Fatalf("the surviving note lost its media: %v", err)
	}
	entries, err := os.ReadDir(p.MediaDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("got %d media files, want 1", len(entries))
	}
}
