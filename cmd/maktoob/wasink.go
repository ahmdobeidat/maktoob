package main

import (
	"context"
	"log/slog"
	"time"

	"github.com/ahmdobeidat/maktoob/internal/pipeline"
	"github.com/ahmdobeidat/maktoob/internal/store"
	"github.com/ahmdobeidat/maktoob/internal/wa"
)

// newWASink adapts the WhatsApp listener to the transcription pipeline.
//
// This closure is the only place in maktoob where internal/wa and
// internal/pipeline meet, and it lives here on purpose. If the pipeline
// implemented wa.Sink instead, internal/pipeline would import internal/wa and
// the dependency would run backwards: deleting the WhatsApp adapter would then
// break file imports and the web interface, which is the opposite of what the
// architecture claims. CI enforces the direction.
// onIngest, when non-nil, is called with the stored note's id after a
// successful ingest. It is how the web layer learns which note arrived: without
// it the arrival event carried an empty id and a client could not resolve the
// note it had just been told about. It is not called for a duplicate, because
// nothing new arrived.
func newWASink(pl *pipeline.Pipeline, log *slog.Logger, onIngest func(noteID string)) wa.SinkFunc {
	return func(ctx context.Context, n wa.VoiceNote) error {
		id, created, err := pl.IngestReader(ctx, n.Audio, waIngestRequest(n, time.Now()))
		if err != nil {
			return err
		}
		if !created {
			// Expected on reconnect rather than a fault worth reporting.
			log.Debug("duplicate voice note ignored", "note", id)
			return nil
		}
		if onIngest != nil {
			onIngest(id)
		}
		return nil
	}
}

// waIngestRequest maps a voice note onto an ingest request.
//
// It is a plain function taking now as an argument so the clamping below is
// testable without waiting for a clock.
func waIngestRequest(n wa.VoiceNote, now time.Time) pipeline.IngestRequest {
	// received_at is peer-supplied and drives both the newest-first list and the
	// oldest-first queue. A future-dated message would pin itself to the top of
	// the list permanently and jump the queue behind it.
	received := n.ReceivedAt
	if received.After(now) {
		received = now
	}

	req := pipeline.IngestRequest{
		Source:       "whatsapp",
		ChatID:       n.ChatAlias,
		ChatName:     n.ChatName,
		Sender:       n.SenderAlias,
		SenderName:   n.SenderName,
		WAMessageID:  n.DedupeKey,
		ReceivedAt:   received,
		Ext:          n.Ext,
		DurationHint: n.DurationHint,
	}

	if n.DownloadErr != "" {
		// The audio is gone but the arrival is not. A media-less failed row is
		// how the user still learns a voice note came in.
		req.Status = store.StatusFailed
		req.Error = n.DownloadErr
	}

	return req
}
