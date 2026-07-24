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

// Ingest copies src into the media directory and queues it for transcription.
// The original file is left untouched.
func (p *Pipeline) Ingest(ctx context.Context, src string) (string, error) {
	if err := os.MkdirAll(p.MediaDir, 0o700); err != nil {
		return "", fmt.Errorf("create media directory: %w", err)
	}

	id := uuid.NewString()
	dst := filepath.Join(p.MediaDir, id+filepath.Ext(src))

	if err := copyFile(src, dst); err != nil {
		return "", err
	}

	durationMS, err := p.Converter.DurationMS(ctx, dst)
	if err != nil {
		// Duration drives display and the transcription timeout, neither of
		// which should reject an otherwise usable note.
		durationMS = 0
	}

	if err := p.Store.UpsertChat(ctx, ImportChatID, "Imported files"); err != nil {
		return "", err
	}

	if _, err := p.Store.CreateNote(ctx, store.Note{
		ID:         id,
		ChatID:     ImportChatID,
		Source:     "import",
		MediaPath:  dst,
		DurationMS: durationMS,
		ReceivedAt: time.Now(),
	}); err != nil {
		return "", err
	}

	return id, nil
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

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("open source audio: %w", err)
	}
	defer in.Close()

	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("create media file: %w", err)
	}
	defer out.Close()

	if _, err := io.Copy(out, in); err != nil {
		return fmt.Errorf("copy audio: %w", err)
	}
	return out.Close()
}
