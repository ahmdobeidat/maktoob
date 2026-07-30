// Command seed builds a demo database for maktoob.
//
// It exists for two reasons the design spec names explicitly. The demo fallback
// when a live pairing fails on stage is "a pre-seeded database plus /import",
// and there is no such database unless something builds one. And screenshots of
// an empty interface show nothing worth judging.
//
// It writes transcripts directly into the store. No audio is transcribed and no
// model is loaded, so it runs in a second on a machine with neither whisper nor
// ffmpeg installed.
//
// The transcript text is NOT baked in here. It comes from a JSON fixture, so
// the Arabic content is authored once by someone who writes Arabic rather than
// being invented by the tooling. See scripts/seed/fixture.example.json.
//
//	go run ./scripts/seed -data demo-data -fixture scripts/seed/fixture.json
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"time"

	"github.com/ahmdobeidat/maktoob/internal/store"
)

// fixture is the on-disk shape of the demo content.
type fixture struct {
	// Readme is accepted and ignored so the shipped example can carry its own
	// instructions. Without it, DisallowUnknownFields would reject the very
	// file the error message tells you to copy.
	Readme string `json:"_readme,omitempty"`

	Chats []fixtureChat `json:"chats"`
}

type fixtureChat struct {
	// ID is the opaque chat identifier. In real use this is a salted alias, so
	// the fixture uses obviously-fake values rather than anything resembling a
	// phone number.
	ID    string        `json:"id"`
	Name  string        `json:"name"`
	Notes []fixtureNote `json:"notes"`
}

type fixtureNote struct {
	Sender string `json:"sender"`
	// MinutesAgo places the note on the timeline. Relative rather than absolute
	// so a fixture written today still looks fresh in a demo next week.
	MinutesAgo int              `json:"minutes_ago"`
	Status     string           `json:"status,omitempty"`
	Error      string           `json:"error,omitempty"`
	Segments   []fixtureSegment `json:"segments"`
}

type fixtureSegment struct {
	StartMS int64  `json:"start_ms"`
	EndMS   int64  `json:"end_ms"`
	Text    string `json:"text"`
	// Edited, when set, is a human correction and Text becomes the machine
	// output it replaced. This is how the fixture demonstrates that a
	// correction never overwrites what the model produced.
	Edited string `json:"edited,omitempty"`
	// Confidence is the displayed probability, 0..1. Converted to the log
	// probability the store actually holds, so the interface derives the same
	// number it would from a real transcript.
	Confidence float64 `json:"confidence"`
	// Suspect marks a line as likely fabrication. A demo that never shows this
	// state hides the project's most important design decision.
	Suspect bool `json:"suspect"`
	// NoSpeechProb backs the suspect marker with the signal a real run would
	// have used.
	NoSpeechProb float64 `json:"no_speech_prob,omitempty"`
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "seed:", err)
		os.Exit(1)
	}
}

func run() error {
	dataDir := flag.String("data", "demo-data", "directory to build the demo database in")
	path := flag.String("fixture", "scripts/seed/fixture.json", "JSON fixture to load")
	force := flag.Bool("force", false, "overwrite an existing database in -data")
	flag.Parse()

	f, err := loadFixture(*path)
	if err != nil {
		return err
	}

	dbPath := filepath.Join(*dataDir, "maktoob.db")
	if _, err := os.Stat(dbPath); err == nil && !*force {
		return fmt.Errorf("%s already exists; pass -force to overwrite it", dbPath)
	} else if err == nil {
		// Removed rather than added to. Seeding twice would otherwise stack
		// duplicate notes and quietly make the demo look wrong.
		for _, suffix := range []string{"", "-wal", "-shm"} {
			if err := os.Remove(dbPath + suffix); err != nil && !errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("clear existing database: %w", err)
			}
		}
	}

	// 0700 for the same reason the real data directory uses it: this holds
	// transcripts, and demo transcripts still sit in someone's home directory.
	if err := os.MkdirAll(*dataDir, 0o700); err != nil {
		return fmt.Errorf("create data directory: %w", err)
	}
	mediaDir := filepath.Join(*dataDir, "media")
	if err := os.MkdirAll(mediaDir, 0o700); err != nil {
		return fmt.Errorf("create media directory: %w", err)
	}

	st, err := store.Open(dbPath)
	if err != nil {
		return err
	}
	defer st.Close()

	notes, err := seed(context.Background(), st, f, mediaDir)
	if err != nil {
		return err
	}

	fmt.Printf("Seeded %d note(s) into %s\n\n", notes, dbPath)
	fmt.Printf("  maktoob serve -data %s\n\n", *dataDir)
	fmt.Println("No audio was written, so the player will report the audio is missing.")
	fmt.Println("Import a real file to demonstrate playback:")
	fmt.Printf("\n  maktoob import -data %s path/to/note.ogg\n", *dataDir)
	return nil
}

func loadFixture(path string) (fixture, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return fixture{}, fmt.Errorf(
			"no fixture at %s.\n"+
				"Copy scripts/seed/fixture.example.json to that path and write the "+
				"transcript text you want the demo to show", path)
	}
	if err != nil {
		return fixture{}, fmt.Errorf("read fixture: %w", err)
	}

	var f fixture
	dec := json.NewDecoder(bytes.NewReader(raw))
	// Unknown fields are an error so a typo in a hand-written fixture surfaces
	// as a parse failure rather than a silently missing marker in the demo.
	dec.DisallowUnknownFields()
	if err := dec.Decode(&f); err != nil {
		return fixture{}, fmt.Errorf("parse %s: %w", path, err)
	}
	if len(f.Chats) == 0 {
		return fixture{}, fmt.Errorf("%s contains no chats", path)
	}
	return f, nil
}

func seed(ctx context.Context, st *store.Store, f fixture, mediaDir string) (int, error) {
	now := time.Now()
	count := 0

	for ci, chat := range f.Chats {
		if chat.ID == "" {
			return 0, fmt.Errorf("chat %d has no id", ci)
		}
		if err := st.UpsertChat(ctx, chat.ID, chat.Name); err != nil {
			return 0, err
		}

		for ni, note := range chat.Notes {
			id := fmt.Sprintf("demo-%s-%02d", chat.ID, ni)

			status := note.Status
			if status == "" {
				status = store.StatusDone
			}

			// A media path is recorded even though no audio is written. The
			// interface checks the file and says the audio is missing, which is
			// the honest state for seeded data and exercises that branch.
			media := filepath.Join(mediaDir, id+".ogg")

			duration := int64(0)
			for _, s := range note.Segments {
				if s.EndMS > duration {
					duration = s.EndMS
				}
			}

			ok, err := st.CreateNote(ctx, store.Note{
				ID:         id,
				ChatID:     chat.ID,
				Source:     "whatsapp",
				Sender:     "alias-" + chat.ID,
				SenderName: note.Sender,
				MediaPath:  media,
				DurationMS: duration,
				ReceivedAt: now.Add(-time.Duration(note.MinutesAgo) * time.Minute),
				Status:     status,
				Error:      note.Error,
				Model:      "large-v3-turbo",
			})
			if err != nil {
				return 0, err
			}
			if !ok {
				return 0, fmt.Errorf("note %s already exists", id)
			}
			count++

			if len(note.Segments) == 0 {
				continue
			}

			segs := make([]store.Segment, 0, len(note.Segments))
			for i, s := range note.Segments {
				// Text is always what the machine produced. A fixture's Edited
				// field is laid over it afterwards through EditSegment, so the
				// seeded row keeps the original exactly as a real one would.
				segs = append(segs, store.Segment{
					Idx:          i,
					StartMS:      s.StartMS,
					EndMS:        s.EndMS,
					ASRText:      s.Text,
					AvgLogprob:   logprob(s.Confidence),
					NoSpeechProb: s.NoSpeechProb,
					Suspect:      s.Suspect,
				})
			}
			if err := st.ReplaceSegments(ctx, id, segs); err != nil {
				return 0, err
			}

			// Corrections are applied through the same path the interface uses,
			// so the seeded rows carry a real edited_at and land in the search
			// index exactly as a hand-typed correction would.
			if err := applyEdits(ctx, st, id, note.Segments); err != nil {
				return 0, err
			}
		}
	}
	return count, nil
}

func applyEdits(ctx context.Context, st *store.Store, noteID string, fixtures []fixtureSegment) error {
	_, stored, err := st.GetNote(ctx, noteID)
	if err != nil {
		return err
	}
	for i, s := range fixtures {
		if s.Edited == "" || i >= len(stored) {
			continue
		}
		if err := st.EditSegment(ctx, stored[i].ID, s.Edited); err != nil {
			return err
		}
	}
	return nil
}

// logprob converts a displayed confidence back into the average log
// probability the store holds, so the interface recomputes the same percentage
// it would show for a real transcript.
func logprob(confidence float64) float64 {
	switch {
	case confidence <= 0:
		return -5
	case confidence >= 1:
		return 0
	}
	return math.Log(confidence)
}
