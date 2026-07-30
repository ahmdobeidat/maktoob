package main

import (
	"context"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ahmdobeidat/maktoob/internal/store"
)

// The shipped example is the file the error message tells you to copy. If it
// does not parse, the first thing anyone does with this tool fails.
func TestShippedExampleFixtureParses(t *testing.T) {
	f, err := loadFixture("fixture.example.json")
	if err != nil {
		t.Fatalf("the example fixture does not load: %v", err)
	}
	if len(f.Chats) == 0 {
		t.Fatal("the example fixture contains no chats")
	}

	// It has to exercise every state the interface can render, or a demo built
	// from it shows a happy path and nothing else.
	var suspect, edited, lowConf, failed, noSpeech bool
	for _, c := range f.Chats {
		for _, n := range c.Notes {
			switch n.Status {
			case store.StatusFailed:
				failed = true
			case store.StatusNoSpeech:
				noSpeech = true
			}
			for _, s := range n.Segments {
				if s.Suspect {
					suspect = true
				}
				if s.Edited != "" {
					edited = true
				}
				if s.Confidence > 0 && s.Confidence < 0.5 {
					lowConf = true
				}
			}
		}
	}
	for name, present := range map[string]bool{
		"a suspect line":        suspect,
		"a corrected line":      edited,
		"a low-confidence line": lowConf,
		"a failed note":         failed,
		"a no-speech note":      noSpeech,
	} {
		if !present {
			t.Errorf("the example fixture has no %s, so a demo built from it cannot show that state", name)
		}
	}
}

// A typo in a hand-written fixture should be a parse error, not a marker that
// silently fails to appear during the demo.
func TestUnknownFixtureFieldsAreRejected(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "typo.json")
	body := `{"chats":[{"id":"c","name":"C","notes":[{"sender":"A","suspct":true,"segments":[]}]}]}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := loadFixture(path); err == nil {
		t.Error("a misspelled field was accepted")
	}
}

func TestMissingFixtureExplainsWhatToDo(t *testing.T) {
	_, err := loadFixture(filepath.Join(t.TempDir(), "absent.json"))
	if err == nil {
		t.Fatal("a missing fixture was accepted")
	}
	if got := err.Error(); !strings.Contains(got, "fixture.example.json") {
		t.Errorf("the error does not say which file to copy: %v", got)
	}
}

// The store holds an average log probability; the fixture is written in the
// percentages a human thinks in. The interface recomputes the percentage from
// the stored value, so the round trip has to land back where it started or a
// seeded demo displays confidences nobody chose.
func TestConfidenceSurvivesTheRoundTrip(t *testing.T) {
	for _, want := range []float64{0.05, 0.22, 0.5, 0.93, 0.99} {
		got := math.Exp(logprob(want))
		if math.Abs(got-want) > 0.001 {
			t.Errorf("confidence %v round-tripped to %v", want, got)
		}
	}

	// The ends are clamped rather than producing infinities.
	if lp := logprob(0); math.IsInf(lp, -1) {
		t.Error("a confidence of zero produced negative infinity")
	}
	if lp := logprob(1); lp != 0 {
		t.Errorf("logprob(1) = %v, want 0", lp)
	}
}

func TestSeedWritesEveryNoteAndItsSegments(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	f, err := loadFixture("fixture.example.json")
	if err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	n, err := seed(ctx, st, f, filepath.Join(dir, "media"))
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	if n == 0 {
		t.Fatal("seed wrote no notes")
	}

	notes, err := st.ListNotes(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(notes) != n {
		t.Errorf("seed reported %d notes, the store holds %d", n, len(notes))
	}

	// Newest first, which is what the fixture's minutes_ago is for.
	for i := 1; i < len(notes); i++ {
		if notes[i].ReceivedAt.After(notes[i-1].ReceivedAt) {
			t.Errorf("note %d is newer than note %d; the list order is wrong", i, i-1)
		}
	}

	// A fixture correction has to land as a real edit, not as the machine
	// output, or the demo cannot show that corrections are kept separately.
	var sawEdit bool
	for _, note := range notes {
		_, segs, err := st.GetNote(ctx, note.ID)
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range segs {
			if s.Edited() {
				sawEdit = true
				if s.ASRText == s.Text() {
					t.Error("a seeded correction overwrote the machine output")
				}
			}
		}
	}
	if !sawEdit {
		t.Error("no seeded segment came out marked as edited")
	}
}
