package arabic

import "testing"

// Arabic test data is written as Unicode escapes for the same reason the
// implementation is: bidirectional glyphs reorder source visually and make a
// failing assertion impossible to read.
const (
	// "ahmad", written with and without the hamza above the initial alef.
	ahmadHamza = "\u0623\u062D\u0645\u062F"
	ahmadBare  = "\u0627\u062D\u0645\u062F"

	// "islam", with and without the hamza below the initial alef.
	islamHamza = "\u0625\u0633\u0644\u0627\u0645"
	islamBare  = "\u0627\u0633\u0644\u0627\u0645"

	// "madrasa" (school), ending in teh marbuta versus plain heh.
	schoolMarbuta = "\u0645\u062F\u0631\u0633\u0629"
	schoolHeh     = "\u0645\u062F\u0631\u0633\u0647"

	// "ala"/"ali", ending in alef maksura versus yeh.
	alaMaksura = "\u0639\u0644\u0649"
	aliYeh     = "\u0639\u0644\u064A"

	// "marhaba", fully diacritised and plain.
	greetingDiacritised = "\u0645\u064E\u0631\u0652\u062D\u064E\u0628\u064B\u0627"
	greetingPlain       = "\u0645\u0631\u062D\u0628\u0627"
)

// TestOrthographicVariantsMatch covers the failure that motivates this package.
// Whisper writes the hamza-carrying and teh-marbuta forms; a user on a phone
// keyboard types the bare forms. Without folding, each pair is a silent
// zero-hit miss — the search returns nothing and gives no indication why.
//
// A test that merely round-trips text through the FTS index would pass while
// search was broken, because it would assert the tokenizer's behaviour rather
// than the property that matters.
func TestOrthographicVariantsMatch(t *testing.T) {
	pairs := []struct {
		name    string
		indexed string
		queried string
	}{
		{"alef with hamza above", ahmadHamza, ahmadBare},
		{"alef with hamza below", islamHamza, islamBare},
		{"teh marbuta and heh", schoolMarbuta, schoolHeh},
		{"alef maksura and yeh", alaMaksura, aliYeh},
		{"diacritised and plain", greetingDiacritised, greetingPlain},
	}

	for _, p := range pairs {
		t.Run(p.name, func(t *testing.T) {
			gotIndexed := Normalize(p.indexed)
			gotQueried := Normalize(p.queried)
			if gotIndexed != gotQueried {
				t.Errorf("variants must normalise identically:\n  indexed %q -> %q\n  queried %q -> %q",
					p.indexed, gotIndexed, p.queried, gotQueried)
			}
		})
	}
}

func TestNormalizeStripsTatweel(t *testing.T) {
	// Tatweel is pure typographic elongation and carries no meaning, but it is
	// a distinct codepoint that breaks exact matching.
	stretched := "\u0645\u062F\u0640\u0640\u0640\u0631\u0633\u0647"
	if got, want := Normalize(stretched), Normalize(schoolHeh); got != want {
		t.Errorf("tatweel not stripped: %q != %q", got, want)
	}
}

func TestNormalizeFoldsArabicIndicDigits(t *testing.T) {
	// Whisper emits Arabic-Indic numerals for Arabic input, while a user
	// searching for a price, date or phone number types Latin digits.
	cases := map[string]string{
		"\u0660\u0661\u0662\u0663\u0664":        "01234",
		"\u0665\u0666\u0667\u0668\u0669":        "56789",
		"\u06F0\u06F1\u06F2":                    "012", // extended (Persian) forms
		"\u0669\u0669\u0660 \u0661\u0662\u0663": "990 123",
	}
	for in, want := range cases {
		if got := Normalize(in); got != want {
			t.Errorf("Normalize(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestNormalizeDecomposesPresentationForms(t *testing.T) {
	// Text pasted from PDFs and some keyboards arrives in the Arabic
	// Presentation Forms blocks. NFKC decomposes these into ordinary letters;
	// no amount of letter folding would otherwise reach them.
	// U+FEDF is LAM INITIAL FORM, U+FE8E is ALEF FINAL FORM.
	presentation := "\uFEDF\uFE8E"
	normal := "\u0644\u0627"
	if got, want := Normalize(presentation), Normalize(normal); got != want {
		t.Errorf("presentation forms not decomposed: %q != %q", got, want)
	}
}

func TestNormalizeStripsInvisibleControls(t *testing.T) {
	// Zero-width and bidi control characters survive copy-paste from messaging
	// apps, are invisible to the user, and are not reliably treated as
	// separators by the tokenizer.
	withControls := "\u200Fmar\u200Bhaba\u061C"
	if got, want := Normalize(withControls), "marhaba"; got != want {
		t.Errorf("Normalize(%q) = %q, want %q", withControls, got, want)
	}
}

// TestBuildFTSQueryNeutralisesSyntax covers input that raises a hard FTS5 error
// rather than returning no rows. Passing raw input to MATCH makes the search box
// return a 500 the first time anyone types an English contraction or a hyphen.
func TestBuildFTSQueryNeutralisesSyntax(t *testing.T) {
	dangerous := []string{
		"it's",
		"a-b",
		"(",
		"-",
		"*",
		"term OR",
		`"unbalanced`,
		"col:value",
		"NEAR(a b)",
		"^anchor",
	}

	for _, in := range dangerous {
		got := BuildFTSQuery(in)
		for _, r := range got {
			switch r {
			case '\'', '(', ')', '-', '*', ':', '^':
				t.Errorf("BuildFTSQuery(%q) = %q: leaked syntax character %q", in, got, r)
			}
		}
	}
}

func TestBuildFTSQueryQuotesTerms(t *testing.T) {
	cases := map[string]string{
		"hello world": `"hello" "world"`,
		"it's":        `"its"`,
		"a-b":         `"ab"`,
		"  spaced  ":  `"spaced"`,
		ahmadHamza:    `"` + ahmadBare + `"`,
	}
	for in, want := range cases {
		if got := BuildFTSQuery(in); got != want {
			t.Errorf("BuildFTSQuery(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestBuildFTSQueryEmptyForUnusableInput(t *testing.T) {
	// Callers must treat an empty result as "no query" rather than passing it
	// to MATCH, which would itself be a syntax error.
	for _, in := range []string{"", "   ", "-", "()", "***"} {
		if got := BuildFTSQuery(in); got != "" {
			t.Errorf("BuildFTSQuery(%q) = %q, want empty", in, got)
		}
	}
}

func TestNormalizeIsIdempotent(t *testing.T) {
	// Normalisation runs on insert and again on query. If it were not
	// idempotent, a corrected segment would drift out of the index.
	inputs := []string{
		ahmadHamza, schoolMarbuta, greetingDiacritised,
		"mixed \u0623\u062D\u0645\u062F 123",
	}
	for _, in := range inputs {
		once := Normalize(in)
		twice := Normalize(once)
		if once != twice {
			t.Errorf("Normalize not idempotent for %q: %q -> %q", in, once, twice)
		}
	}
}
