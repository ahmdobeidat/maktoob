package main

import (
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
		t.Fatalf("source: %q", req.Source)
	}
	if req.ChatID != "chat-alias" || req.Sender != "sender-alias" {
		t.Fatalf("identifiers not carried through: %+v", req)
	}
	if req.WAMessageID != "dedupe" {
		t.Fatalf("dedupe key: %q", req.WAMessageID)
	}
	if req.Status != "" {
		t.Fatalf("a healthy note must default to pending, got %q", req.Status)
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
