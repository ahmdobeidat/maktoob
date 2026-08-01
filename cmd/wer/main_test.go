package main

import (
	"os"
	"reflect"
	"strings"
	"testing"
)

// The published WER is only as trustworthy as this alignment, so the operation
// counts are pinned against hand-worked cases rather than assumed correct.
func TestAlignCountsOperations(t *testing.T) {
	tests := []struct {
		name          string
		ref, hyp      string
		sub, del, ins int
	}{
		{"identical", "a b c", "a b c", 0, 0, 0},
		{"one substitution", "a b c", "a x c", 1, 0, 0},
		{"one deletion", "a b c", "a c", 0, 1, 0},
		{"one insertion", "a b c", "a b x c", 0, 0, 1},
		{"substitution and deletion", "a b c d", "a x c", 1, 1, 0},
		{"empty hypothesis is all deletions", "a b c", "", 0, 3, 0},
		{"empty reference is all insertions", "", "a b c", 0, 0, 3},
		{"nothing in common", "a b", "x y", 2, 0, 0},
		{"trailing words dropped", "a b c d e", "a b", 0, 3, 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sub, del, ins := align(strings.Fields(tt.ref), strings.Fields(tt.hyp))
			if sub != tt.sub || del != tt.del || ins != tt.ins {
				t.Errorf("align(%q, %q) = %dS %dD %dI, want %dS %dD %dI",
					tt.ref, tt.hyp, sub, del, ins, tt.sub, tt.del, tt.ins)
			}
		})
	}
}

// A minimum-distance alignment is not unique, but its total cost is. Whichever
// backtrace path is taken, the operations must sum to the edit distance —
// otherwise the reported rate is wrong even when the counts look plausible.
func TestAlignTotalIsEditDistance(t *testing.T) {
	ref := strings.Fields("the quick brown fox jumps over the lazy dog")
	hyp := strings.Fields("the quick fox jumped over a very lazy dog today")

	sub, del, ins := align(ref, hyp)
	// Independently: 9 reference words, 10 hypothesis words, distance 5.
	if got, want := sub+del+ins, 5; got != want {
		t.Errorf("total operations = %d (%dS %dD %dI), want %d", got, sub, del, ins, want)
	}
	// Insertions minus deletions must account for the length difference.
	if got, want := ins-del, len(hyp)-len(ref); got != want {
		t.Errorf("ins-del = %d, want %d", got, want)
	}
}

func TestRate(t *testing.T) {
	if got := rate(3, 12); got != 0.25 {
		t.Errorf("rate(3, 12) = %v, want 0.25", got)
	}
	// An empty reference must not divide by zero.
	if got := rate(0, 0); got != 0 {
		t.Errorf("rate(0, 0) = %v, want 0", got)
	}
}

// tokens decides what counts as an error, so its folding is part of the
// measurement contract and not an implementation detail.
func TestTokensFoldsVariantsAndDropsPunctuation(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want []string
	}{
		{
			// Hamza-carrying alef: whisper writes it, phone keyboards omit it.
			// Scoring these as errors would measure keyboards, not hearing.
			name: "alef forms fold together",
			in:   "أنا إلى آخر",
			want: []string{"انا", "الي", "اخر"},
		},
		{
			// Teh marbuta and alef maksura are written interchangeably.
			name: "teh marbuta folds to heh",
			in:   "محشية",
			want: []string{"محشيه"},
		},
		{
			// Whisper invents sentence-final marks the script has no opinion on.
			name: "punctuation is dropped",
			in:   "طب، شو؟",
			want: []string{"طب", "شو"},
		},
		{
			name: "diacritics are removed",
			in:   "حاليّاً",
			want: []string{"حاليا"},
		},
		{
			name: "whitespace only yields no tokens",
			in:   "   \n\t ",
			want: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tokens(tt.in); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("tokens(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// A word split into two by the model must not silently score as a match.
func TestTokensKeepsCliticSplitVisible(t *testing.T) {
	joined := tokens("احكيلك") // احكيلك
	split := tokens("احكي لك") // احكي لك
	if len(joined) != 1 || len(split) != 2 {
		t.Fatalf("token counts = %d and %d, want 1 and 2", len(joined), len(split))
	}
	sub, del, ins := align(joined, split)
	if sub+del+ins == 0 {
		t.Error("clitic split scored as a perfect match; it must count as an error")
	}
}

func TestReadRefsRejectsMalformedLine(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/reference.tsv"
	if err := os.WriteFile(path, []byte("# comment\naudio-01.ogg no tab here\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := readRefs(path); err == nil {
		t.Error("readRefs accepted a line with no tab separator")
	}
}

func TestReadRefsSkipsCommentsAndBlanks(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/reference.tsv"
	if err := os.WriteFile(path, []byte("# header\n\naudio-01.ogg\thello\naudio-02.ogg\tworld\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	refs, order, err := readRefs(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 2 || refs["audio-01.ogg"] != "hello" {
		t.Errorf("refs = %v, want 2 entries with audio-01.ogg=hello", refs)
	}
	if !reflect.DeepEqual(order, []string{"audio-01.ogg", "audio-02.ogg"}) {
		t.Errorf("order = %v, want sorted file names", order)
	}
}
