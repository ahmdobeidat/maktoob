package main

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ahmdobeidat/maktoob/internal/pipeline"
	"github.com/ahmdobeidat/maktoob/internal/store"
	"github.com/ahmdobeidat/maktoob/internal/wa"
)

func TestWAIngestRequestMapsFields(t *testing.T) {
	received := time.Unix(1700000000, 0)
	now := received.Add(time.Hour)

	req := waIngestRequest(wa.VoiceNote{
		ChatAlias: "chat-alias", ChatName: "Family",
		SenderAlias: "sender-alias", SenderName: "Um Ahmad",
		DedupeKey: "dedupe", ReceivedAt: received,
		DurationHint: 7000, Ext: ".ogg",
	}, now)

	if req.Source != "whatsapp" {
		t.Fatalf("source: got %q, want %q", req.Source, "whatsapp")
	}
	if req.ChatID != "chat-alias" {
		t.Fatalf("chat ID: got %q, want %q", req.ChatID, "chat-alias")
	}
	if req.ChatName != "Family" {
		t.Fatalf("chat name: got %q, want %q", req.ChatName, "Family")
	}
	if req.Sender != "sender-alias" {
		t.Fatalf("sender: got %q, want %q", req.Sender, "sender-alias")
	}
	if req.SenderName != "Um Ahmad" {
		t.Fatalf("sender name: got %q, want %q", req.SenderName, "Um Ahmad")
	}
	if req.WAMessageID != "dedupe" {
		t.Fatalf("dedupe key: got %q, want %q", req.WAMessageID, "dedupe")
	}
	if req.Ext != ".ogg" {
		t.Fatalf("ext: got %q, want %q", req.Ext, ".ogg")
	}
	if req.DurationHint != 7000 {
		t.Fatalf("duration hint: got %d, want %d", req.DurationHint, 7000)
	}
	if req.Status != "" {
		t.Fatalf("status: got %q, want empty for healthy note", req.Status)
	}
	if !req.ReceivedAt.Equal(received) {
		t.Fatalf("timestamp: got %v, want %v", req.ReceivedAt, received)
	}
}

func TestWAIngestRequestClampsFutureTimestamps(t *testing.T) {
	now := time.Unix(1700000000, 0)
	future := now.Add(48 * time.Hour)

	req := waIngestRequest(wa.VoiceNote{ReceivedAt: future, Ext: ".ogg"}, now)

	// received_at drives both list ordering and queue ordering, so a
	// future-dated message would otherwise pin itself to the top forever.
	if req.ReceivedAt.After(now) {
		t.Fatalf("timestamp not clamped: got %v, want at most %v", req.ReceivedAt, now)
	}
}

func TestWAIngestRequestMarksFailedDownload(t *testing.T) {
	now := time.Unix(1700000000, 0)

	req := waIngestRequest(wa.VoiceNote{
		ChatAlias: "chat", DedupeKey: "dedupe", ReceivedAt: now,
		Ext: ".ogg", DownloadErr: "410 gone",
	}, now)

	if req.Status != store.StatusFailed {
		t.Fatalf("status: got %q, want %q", req.Status, store.StatusFailed)
	}
	if req.Error != "410 gone" {
		t.Fatalf("error text: %q", req.Error)
	}
}

func TestCheckSaltDetectsASwappedSalt(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	pathA := filepath.Join(t.TempDir(), "salt-a")
	first, err := wa.LoadSalt(pathA)
	if err != nil {
		t.Fatal(err)
	}
	// First run records the fingerprint.
	if err := checkSalt(ctx, st, first, pathA); err != nil {
		t.Fatal(err)
	}
	// Same salt, same database: fine.
	if err := checkSalt(ctx, st, first, pathA); err != nil {
		t.Fatal(err)
	}

	pathB := filepath.Join(t.TempDir(), "salt-b")
	second, err := wa.LoadSalt(pathB)
	if err != nil {
		t.Fatal(err)
	}
	// A different salt against the same database silently forks every chat.
	err = checkSalt(ctx, st, second, pathB)
	if err == nil {
		t.Fatal("a swapped salt was accepted")
	}
	// The message has to name the file the user actually has. Hardcoding
	// "data/salt" sends anyone running with -data to a path that is not theirs,
	// at the moment they are being told their transcripts may be unreachable.
	if !strings.Contains(err.Error(), pathB) {
		t.Fatalf("error names no usable path, got: %v", err)
	}
}

// newWASink is the seam between two packages forbidden to know about each other,
// and until now only waIngestRequest was covered — the closure itself, including
// how it treats a duplicate delivery, was exercised only by the type system.
func TestNewWASinkRecordsAndToleratesDuplicates(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	st, err := store.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	pl := &pipeline.Pipeline{
		Store:     st,
		Converter: stubConverter{},
		MediaDir:  filepath.Join(dir, "media"),
	}
	sink := newWASink(pl, slog.New(slog.NewTextHandler(io.Discard, nil)))

	note := wa.VoiceNote{
		ChatAlias: "chat-alias", ChatName: "Family",
		SenderAlias: "sender-alias", SenderName: "Um Ahmad",
		DedupeKey: "dedupe-1", ReceivedAt: time.Now(),
		DurationHint: 7000, Ext: ".ogg",
		Audio: strings.NewReader("audio"),
	}

	if err := sink.Ingest(ctx, note); err != nil {
		t.Fatalf("first delivery: %v", err)
	}

	// A reconnect redelivers the same message. That is expected traffic, so the
	// sink must not surface it as an error, and it must not leave a second note
	// or a second media file behind.
	note.Audio = strings.NewReader("audio")
	if err := sink.Ingest(ctx, note); err != nil {
		t.Fatalf("duplicate delivery reported an error: %v", err)
	}

	notes, err := st.ListNotes(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(notes) != 1 {
		t.Fatalf("got %d notes after a duplicate delivery, want 1", len(notes))
	}

	entries, err := os.ReadDir(pl.MediaDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("got %d media files, want 1 — the duplicate orphaned one", len(entries))
	}
}
