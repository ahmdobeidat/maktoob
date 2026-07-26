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
