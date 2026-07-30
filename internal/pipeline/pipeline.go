// Package pipeline moves a voice note from raw audio to a stored transcript.
//
// It is deliberately the only place that knows the whole sequence. The store
// owns persistence, audio owns conversion, asr owns inference; none of them
// know about each other.
package pipeline

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/google/uuid"

	"github.com/ahmdobeidat/maktoob/internal/asr"
	"github.com/ahmdobeidat/maktoob/internal/audio"
	"github.com/ahmdobeidat/maktoob/internal/store"
)

// ImportChatID is the chat notes land in when they arrive by file upload rather
// than from WhatsApp.
const ImportChatID = "import"

// Pipeline processes queued notes.
type Pipeline struct {
	Store     *store.Store
	Converter audio.Converter
	ASR       *asr.Client
	Detector  *asr.Detector

	// MediaDir holds original and converted audio.
	MediaDir string

	// Model records which model produced a transcript, so a note transcribed
	// with a different model can be identified later.
	Model string

	// OnNote, when set, is called after a note reaches a terminal state. It is
	// how the web layer learns to push an update without the pipeline knowing
	// the web layer exists.
	OnNote func(noteID string)
}

// IngestRequest is everything the pipeline needs about a note that is not the
// audio itself.
//
// It names no WhatsApp concept. The identifiers are opaque strings, which is
// what lets internal/wa alias them before they ever reach here.
type IngestRequest struct {
	Source       string // "whatsapp" | "import"
	ChatID       string // opaque; ImportChatID for uploads
	ChatName     string // may be empty; UpsertChat keeps any better name it already has
	Sender       string // opaque alias, never a JID
	SenderName   string // display name only
	WAMessageID  string // opaque dedupe key; empty for imports
	ReceivedAt   time.Time
	Ext          string // ".ogg"; defaults to ".bin"
	DurationHint int64  // used only when probing is impossible
	Status       string // empty means pending
	Error        string
}

// IngestReader writes r into the media directory and queues the note.
//
// A nil r records a media-less note. That is how a failed download is preserved:
// the user still learns that a voice note arrived, which matters most to the
// user who cannot simply play it to find out.
//
// When req.Ext is empty, it defaults to .bin. This deliberately differs from the
// pre-existing import behaviour, which left an extensionless source file
// extensionless; a file with no extension is worse to serve and worse to debug
// than one explicitly marked .bin.
//
// created reports whether a new note was inserted. False means the dedupe key
// was already present — expected traffic on reconnect, not an error — in which
// case the freshly written file is removed and id names the existing note.
func (p *Pipeline) IngestReader(ctx context.Context, r io.Reader, req IngestRequest) (id string, created bool, err error) {
	if err := os.MkdirAll(p.MediaDir, 0o700); err != nil {
		return "", false, fmt.Errorf("create media directory: %w", err)
	}

	ext := req.Ext
	if ext == "" {
		ext = ".bin"
	}

	id = uuid.NewString()
	durationMS := req.DurationHint
	var dst string

	// Nothing references dst until CreateNote inserts the row that names it, so
	// any error return after the write leaves a file no query can ever reach.
	// On the import path that was survivable: the user re-runs the command. On
	// the WhatsApp path it is not. whatsmeow acknowledged the message as it
	// handed it to us and will not redeliver, and the user is deaf and cannot
	// play the audio to recover what the note said, so a transient store failure
	// is permanent data loss plus a media directory that only grows.
	//
	// The deferred removal is keyed on the named err return so it covers every
	// exit below, including ones added later. The duplicate-key path clears dst
	// itself and returns nil, so it keeps its existing behaviour.
	defer func() {
		if err != nil && dst != "" {
			os.Remove(dst)
		}
	}()

	if r != nil {
		dst = filepath.Join(p.MediaDir, id+ext)
		if err := writeMedia(dst, r); err != nil {
			return "", false, err
		}
		// A probe failure must not reject an otherwise usable note; the hint
		// stands in, and duration only drives display and the ASR timeout.
		if d, err := p.Converter.DurationMS(ctx, dst); err == nil {
			durationMS = d
		}
	}

	if err := p.Store.UpsertChat(ctx, req.ChatID, req.ChatName); err != nil {
		return "", false, err
	}

	created, err = p.Store.CreateNote(ctx, store.Note{
		ID:          id,
		ChatID:      req.ChatID,
		Source:      req.Source,
		Sender:      req.Sender,
		SenderName:  req.SenderName,
		WAMessageID: req.WAMessageID,
		MediaPath:   dst,
		DurationMS:  durationMS,
		ReceivedAt:  req.ReceivedAt,
		Status:      req.Status,
		Error:       req.Error,
	})
	if err != nil {
		return "", false, err
	}

	if !created {
		// Nothing references this file: it is a fresh UUID path and no row was
		// inserted for it. Leaving it behind would accumulate one orphan per
		// duplicate delivery, forever.
		if dst != "" {
			os.Remove(dst)
			// Cleared so the deferred removal does not chase a path this branch
			// has already dealt with.
			dst = ""
		}
		existing, err := p.Store.NoteIDByWAMessageID(ctx, req.WAMessageID)
		if err != nil {
			return "", false, err
		}
		return existing, false, nil
	}

	return id, true, nil
}

// Ingest copies src into the media directory and queues it for transcription.
// The original file is left untouched.
func (p *Pipeline) Ingest(ctx context.Context, src string) (string, error) {
	f, err := os.Open(src)
	if err != nil {
		return "", fmt.Errorf("open source audio: %w", err)
	}
	defer f.Close()

	id, _, err := p.IngestReader(ctx, f, IngestRequest{
		Source:     "import",
		ChatID:     ImportChatID,
		ChatName:   "Imported files",
		Ext:        filepath.Ext(src),
		ReceivedAt: time.Now(),
	})
	return id, err
}

// ProcessNext takes one note from the queue and runs it to a terminal state.
// It reports whether a note was processed; false means the queue was empty.
//
// A processing failure is recorded against the note and returned as nil error:
// the queue should keep draining, and the note stays visible as playable audio
// with the error attached. Only an error that leaves the queue itself unusable
// is propagated.
func (p *Pipeline) ProcessNext(ctx context.Context) (bool, error) {
	note, err := p.Store.ClaimNext(ctx)
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}

	if err := p.process(ctx, note); err != nil {
		if setErr := p.Store.SetStatus(ctx, note.ID, store.StatusFailed, err.Error()); setErr != nil {
			return true, fmt.Errorf("recording failure for note %s: %w", note.ID, setErr)
		}
	}

	if p.OnNote != nil {
		p.OnNote(note.ID)
	}
	return true, nil
}

func (p *Pipeline) process(ctx context.Context, note store.Note) error {
	wavPath := filepath.Join(p.MediaDir, note.ID+".wav")

	if err := p.Converter.ToWAV(ctx, note.MediaPath, wavPath); err != nil {
		return fmt.Errorf("convert audio: %w", err)
	}
	if err := p.Store.SetWavPath(ctx, note.ID, wavPath); err != nil {
		return err
	}
	if err := p.Store.SetStatus(ctx, note.ID, store.StatusTranscribing, ""); err != nil {
		return err
	}

	// Bound the inference call. Whisper's repetition-loop failure on degenerate
	// audio can run far past real time, and one such note would otherwise stall
	// the single worker indefinitely.
	tctx, cancel := context.WithTimeout(ctx, asr.Timeout(note.DurationMS))
	defer cancel()

	result, err := p.ASR.Transcribe(tctx, wavPath)
	if err != nil {
		return fmt.Errorf("transcribe: %w", err)
	}

	reasons := p.Detector.Inspect(result.Segments)

	segments := make([]store.Segment, 0, len(result.Segments))
	for i, s := range result.Segments {
		segments = append(segments, store.Segment{
			Idx:          i,
			StartMS:      int64(s.Start * 1000),
			EndMS:        int64(s.End * 1000),
			ASRText:      s.Text,
			AvgLogprob:   s.AvgLogprob,
			NoSpeechProb: s.NoSpeechProb,
			MeanWordP:    s.MeanWordP(),
			MinWordP:     s.MinWordP(),
			Temperature:  s.Temperature,
			Suspect:      reasons[i] != asr.ReasonNone,
		})
	}

	if err := p.Store.ReplaceSegments(ctx, note.ID, segments); err != nil {
		return err
	}

	// Voice activity detection finding nothing is a successful outcome, not a
	// failure. Without a distinct state it either looks like a bug or like an
	// empty transcript nobody can explain.
	status := store.StatusDone
	if len(segments) == 0 {
		status = store.StatusNoSpeech
	}
	return p.Store.SetStatus(ctx, note.ID, status, "")
}

// Drain processes queued notes until the queue is empty or ctx is cancelled.
func (p *Pipeline) Drain(ctx context.Context) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		processed, err := p.ProcessNext(ctx)
		if err != nil {
			return err
		}
		if !processed {
			return nil
		}
	}
}

// writeMedia is the single place media is written to disk, which makes it the
// single enforcement point for the owner-only mode that PRIVACY.md promises.
//
// A partial file is removed rather than left behind: a truncated note that looks
// playable is worse than one that is visibly missing.
func writeMedia(dst string, r io.Reader) error {
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("create media file: %w", err)
	}

	if _, err := io.Copy(out, r); err != nil {
		out.Close()
		os.Remove(dst)
		return fmt.Errorf("write media: %w", err)
	}
	if err := out.Close(); err != nil {
		os.Remove(dst)
		return fmt.Errorf("write media: %w", err)
	}
	return nil
}
