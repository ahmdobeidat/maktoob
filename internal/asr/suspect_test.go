package asr

import (
	"os"
	"path/filepath"
	"testing"
)

// TestNoSpeechOverridesHighConfidence is the reason no_speech_prob is stored at
// all. Whisper's worst failure is fluent invented text over silence, and those
// fabrications carry high token probability — so confidence alone would mark
// the fabrication as trustworthy. For a reader who cannot check the audio, that
// is worse than a visibly broken transcript.
func TestNoSpeechOverridesHighConfidence(t *testing.T) {
	d := NewDetector()

	segments := []Segment{
		{Text: "genuine speech", AvgLogprob: -0.1, NoSpeechProb: 0.01},
		{Text: "invented over silence", AvgLogprob: -0.05, NoSpeechProb: 0.95},
	}

	reasons := d.Inspect(segments)

	if reasons[0] != ReasonNone {
		t.Errorf("genuine segment flagged as %q", reasons[0])
	}
	if reasons[1] != ReasonNoSpeech {
		t.Errorf("fabrication over silence flagged as %q, want %q", reasons[1], ReasonNoSpeech)
	}
	// The fabricated segment is the more confident of the two, which is
	// precisely why confidence cannot be the only signal.
	if segments[1].Confidence() <= segments[0].Confidence() {
		t.Fatal("fixture no longer models the failure: the fabrication must score higher")
	}
}

func TestRepeatedSegmentsFlagged(t *testing.T) {
	d := NewDetector()
	reasons := d.Inspect([]Segment{
		{Text: "hello there"},
		{Text: "hello there"},
		{Text: "something else"},
	})

	if reasons[0] != ReasonNone {
		t.Errorf("first occurrence flagged as %q; it may be what was said", reasons[0])
	}
	if reasons[1] != ReasonRepeat {
		t.Errorf("repeat flagged as %q, want %q", reasons[1], ReasonRepeat)
	}
	if reasons[2] != ReasonNone {
		t.Errorf("distinct segment flagged as %q", reasons[2])
	}
}

func TestKnownArtifactFlagged(t *testing.T) {
	d := NewDetector()
	// Matching happens on normalised text, so an artifact still matches when it
	// arrives with different orthography than the stored form.
	reasons := d.Inspect([]Segment{
		{Text: "Subtitles by the Amara.org community"},
		{Text: "real content"},
	})
	if reasons[0] != ReasonKnown {
		t.Errorf("known artifact flagged as %q, want %q", reasons[0], ReasonKnown)
	}
	if reasons[1] != ReasonNone {
		t.Errorf("real content flagged as %q", reasons[1])
	}
}

func TestEmptySegmentsNotFlagged(t *testing.T) {
	d := NewDetector()
	reasons := d.Inspect([]Segment{{Text: ""}, {Text: "   "}})
	for i, r := range reasons {
		if r != ReasonNone {
			t.Errorf("empty segment %d flagged as %q", i, r)
		}
	}
}

// TestLoadArtifacts covers the supported extension point: new artifacts appear
// as models are retrained, and suppressing one must not require a rebuild.
func TestLoadArtifacts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "artifacts.txt")
	content := "# a comment\n\nplease like and subscribe\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write artifacts: %v", err)
	}

	d := NewDetector()
	before := len(d.Artifacts)
	if err := d.LoadArtifacts(path); err != nil {
		t.Fatalf("load artifacts: %v", err)
	}
	if len(d.Artifacts) != before+1 {
		t.Errorf("loaded %d artifacts, want 1 (comments and blanks skipped)",
			len(d.Artifacts)-before)
	}

	reasons := d.Inspect([]Segment{{Text: "Please like and subscribe"}})
	if reasons[0] != ReasonKnown {
		t.Errorf("user-supplied artifact flagged as %q, want %q", reasons[0], ReasonKnown)
	}
}

func TestLoadArtifactsMissingFile(t *testing.T) {
	d := NewDetector()
	if err := d.LoadArtifacts(filepath.Join(t.TempDir(), "absent.txt")); err == nil {
		t.Error("loading a missing artifacts file should report an error")
	}
}
