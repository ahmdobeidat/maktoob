package asr

import (
	"math"
	"os"
	"testing"
	"time"
)

// The fixture is a real whisper-server verbose_json response, captured from the
// running server rather than written by hand. It pins the field names maktoob
// depends on: an upstream rename would otherwise surface as every segment
// silently scoring zero confidence, which is indistinguishable from a model
// that is merely unsure.
func loadFixture(t *testing.T) Result {
	t.Helper()
	raw, err := os.ReadFile("../../testdata/whisper_verbose_json.json")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	res, err := Parse(raw)
	if err != nil {
		t.Fatalf("parse fixture: %v", err)
	}
	return res
}

func TestParseExtractsConfidenceFields(t *testing.T) {
	res := loadFixture(t)

	if len(res.Segments) == 0 {
		t.Fatal("no segments parsed")
	}
	seg := res.Segments[0]

	if seg.Text == "" {
		t.Error("segment text is empty")
	}
	// A parsed-but-absent avg_logprob would be exactly 0, which exp() turns
	// into a perfect 1.0 confidence score. That is the failure this guards.
	if seg.AvgLogprob == 0 {
		t.Error("avg_logprob is zero: field missing or renamed upstream")
	}
	if seg.AvgLogprob > 0 {
		t.Errorf("avg_logprob = %v, want negative", seg.AvgLogprob)
	}
	if len(seg.Words) == 0 {
		t.Error("no word probabilities parsed")
	}
	if seg.End <= seg.Start {
		t.Errorf("segment span is not positive: %v..%v", seg.Start, seg.End)
	}
}

func TestConfidenceIsGeometricMean(t *testing.T) {
	cases := []struct {
		avgLogprob float64
		want       float64
	}{
		{0, 1},
		{-0.11931712180376053, 0.8875},
		{-1, 0.3679},
		{-3, 0.0498},
	}
	for _, c := range cases {
		got := Segment{AvgLogprob: c.avgLogprob}.Confidence()
		if math.Abs(got-c.want) > 0.001 {
			t.Errorf("Confidence(%v) = %.4f, want %.4f", c.avgLogprob, got, c.want)
		}
	}
}

func TestConfidenceClamped(t *testing.T) {
	if got := (Segment{AvgLogprob: math.Inf(1)}).Confidence(); got != 1 {
		t.Errorf("positive infinity -> %v, want 1", got)
	}
	if got := (Segment{AvgLogprob: math.Inf(-1)}).Confidence(); got != 0 {
		t.Errorf("negative infinity -> %v, want 0", got)
	}
	if got := (Segment{AvgLogprob: math.NaN()}).Confidence(); got != 0 {
		t.Errorf("NaN -> %v, want 0", got)
	}
}

func TestWordProbabilityAggregates(t *testing.T) {
	seg := Segment{Words: []Word{
		{Probability: 0.9},
		{Probability: 0.2},
		{Probability: 0.7},
	}}
	if got := seg.MinWordP(); math.Abs(got-0.2) > 1e-9 {
		t.Errorf("MinWordP = %v, want 0.2", got)
	}
	if got := seg.MeanWordP(); math.Abs(got-0.6) > 1e-9 {
		t.Errorf("MeanWordP = %v, want 0.6", got)
	}

	empty := Segment{}
	if empty.MinWordP() != 0 || empty.MeanWordP() != 0 {
		t.Error("word aggregates on an empty segment must be zero, not NaN")
	}
}

func TestParseRejectsMalformedJSON(t *testing.T) {
	if _, err := Parse([]byte("not json")); err == nil {
		t.Error("Parse accepted malformed input")
	}
}

func TestParseAcceptsEmptySegments(t *testing.T) {
	// Voice activity detection finding no speech is a valid, empty result, not
	// a parse failure.
	res, err := Parse([]byte(`{"language":"ar","duration":3.0,"segments":[]}`))
	if err != nil {
		t.Fatalf("Parse rejected an empty result: %v", err)
	}
	if len(res.Segments) != 0 {
		t.Errorf("got %d segments, want 0", len(res.Segments))
	}
}

func TestTimeoutHasFloorForShortNotes(t *testing.T) {
	// Whisper pads every input to a 30-second analysis window, so a two-second
	// note costs roughly as much as a long one. A timeout scaled purely to
	// duration would kill short notes before they could finish.
	if got := Timeout(2000); got < 90*time.Second {
		t.Errorf("Timeout(2s) = %v, want at least 90s", got)
	}
	if got := Timeout(600_000); got != 6000*time.Second {
		t.Errorf("Timeout(600s) = %v, want it to scale past the floor", got)
	}
}
