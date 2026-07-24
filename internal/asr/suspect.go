package asr

import (
	"bufio"
	"os"
	"strings"

	"github.com/ahmdobeidat/maktoob/internal/arabic"
)

// Whisper's most dangerous failure is not a garbled transcript. It is fluent,
// confident, entirely invented text produced over silence or noise, absorbed
// from subtitle credits in its training data. Those fabrications carry high
// token probability, so a confidence score alone would mark them as
// trustworthy — the safety signal would vouch for the lie.
//
// For a reader who cannot check the transcript against the audio, that is the
// worst available outcome, so it gets its own signal rather than being folded
// into confidence.

// NoSpeechThreshold is whisper's own default for treating a segment as
// non-speech. Above it, the text is suspect no matter how confident the tokens
// are.
const NoSpeechThreshold = 0.6

// SuspectReason explains why a segment was flagged, so the interface can tell a
// reader what kind of doubt applies rather than showing an unexplained marker.
type SuspectReason string

const (
	ReasonNone     SuspectReason = ""
	ReasonNoSpeech SuspectReason = "no_speech"
	ReasonRepeat   SuspectReason = "repeat"
	ReasonKnown    SuspectReason = "known_artifact"
)

// knownArtifacts are phrases whisper emits over silence. They are subtitle
// credits learned from training data and appear verbatim, so exact matching on
// normalised text is enough.
//
// Written as Unicode escapes: bidirectional glyphs reorder source visually,
// which makes a list like this impossible to review. Extend it with
// LoadArtifacts rather than editing this slice.
var knownArtifacts = []string{
	// Arabic subtitle translation credit, the most commonly reported artifact
	// on silent Arabic audio.
	"\u062A\u0631\u062C\u0645\u0629 \u0646\u0627\u0646\u0633\u064A \u0642\u0646\u0642\u0631",
	// "subscribe to the channel", absorbed from video captions.
	"\u0627\u0634\u062A\u0631\u0643\u0648\u0627 \u0641\u064A \u0627\u0644\u0642\u0646\u0627\u0629",
	// Cross-language siblings: multilingual models leak these into Arabic output.
	"altyazı m.k.",
	"untertitelung des zdf",
	"amara.org",
	"subtitles by the amara.org community",
	"thanks for watching",
}

// Detector flags segments that look fabricated rather than merely uncertain.
type Detector struct {
	// Artifacts are normalised phrases treated as known fabrications.
	Artifacts []string
	// NoSpeechThreshold overrides the package default when non-zero.
	NoSpeechThreshold float64
}

// NewDetector returns a Detector seeded with the built-in artifact list.
func NewDetector() *Detector {
	normalised := make([]string, 0, len(knownArtifacts))
	for _, a := range knownArtifacts {
		normalised = append(normalised, arabic.Normalize(a))
	}
	return &Detector{
		Artifacts:         normalised,
		NoSpeechThreshold: NoSpeechThreshold,
	}
}

// LoadArtifacts adds phrases from a file, one per line, ignoring blanks and
// lines beginning with '#'. This is the supported extension point: new
// artifacts appear as whisper is retrained and as other languages leak in, and
// a user should not have to rebuild to suppress one.
func (d *Detector) LoadArtifacts(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		d.Artifacts = append(d.Artifacts, arabic.Normalize(line))
	}
	return sc.Err()
}

// Inspect returns a reason per segment, in the order given.
//
// Repetition is checked against the immediately preceding segment because
// whisper's degenerate-decode failure emits the same line over and over. The
// first occurrence is left unflagged: it may well be what was actually said.
func (d *Detector) Inspect(segments []Segment) []SuspectReason {
	threshold := d.NoSpeechThreshold
	if threshold == 0 {
		threshold = NoSpeechThreshold
	}

	reasons := make([]SuspectReason, len(segments))
	var prev string

	for i, seg := range segments {
		text := arabic.Normalize(seg.Text)

		switch {
		case text == "":
			reasons[i] = ReasonNone
		case seg.NoSpeechProb > threshold:
			reasons[i] = ReasonNoSpeech
		case d.isKnownArtifact(text):
			reasons[i] = ReasonKnown
		case text == prev:
			reasons[i] = ReasonRepeat
		default:
			reasons[i] = ReasonNone
		}

		if text != "" {
			prev = text
		}
	}
	return reasons
}

func (d *Detector) isKnownArtifact(normalised string) bool {
	for _, a := range d.Artifacts {
		if a != "" && strings.Contains(normalised, a) {
			return true
		}
	}
	return false
}
