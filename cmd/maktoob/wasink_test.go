package main

import (
	"context"
	"path/filepath"
	"testing"
	"time"

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

	first, err := wa.LoadSalt(filepath.Join(t.TempDir(), "salt-a"))
	if err != nil {
		t.Fatal(err)
	}
	// First run records the fingerprint.
	if err := checkSalt(ctx, st, first); err != nil {
		t.Fatal(err)
	}
	// Same salt, same database: fine.
	if err := checkSalt(ctx, st, first); err != nil {
		t.Fatal(err)
	}

	second, err := wa.LoadSalt(filepath.Join(t.TempDir(), "salt-b"))
	if err != nil {
		t.Fatal(err)
	}
	// A different salt against the same database silently forks every chat.
	if err := checkSalt(ctx, st, second); err == nil {
		t.Fatal("a swapped salt was accepted")
	}
}
