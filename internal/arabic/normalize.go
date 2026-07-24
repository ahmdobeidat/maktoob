// Package arabic normalises Arabic text for search indexing and querying.
//
// SQLite's unicode61 tokenizer classifies Arabic combining marks as separators,
// which shreds diacritised Arabic into individual letters: a word carrying
// harakat is indexed as one term per letter, and a query for the plain form
// matches nothing. The remove_diacritics option does not help, because its
// table covers precomposed Latin, Greek and Cyrillic forms while Arabic marks
// are standalone combining characters. Settings 0, 1 and 2 produce identical
// output for Arabic.
//
// Normalisation therefore happens here, in Go, and must be applied identically
// on insert and on query. Callers store the display text unchanged and index
// Normalize(text).
//
// The transformation is deliberately recall-biased: it folds orthographic
// variants together so that text written one way is found by a query written
// another way. This loses some precision — alef maksura and yeh become the same
// letter, so a small number of genuinely distinct words collide. That trade is
// correct for this application, where the alternative is a silent zero-hit miss
// on ordinary input, but callers should rank exact-form matches above folded
// ones and highlight against the original text.
//
// Every Arabic character here is written as a Unicode escape rather than a
// literal glyph. Bidirectional text reorders source code visually in most
// editors and terminals, which makes ranges and map literals difficult to
// review and easy to get wrong; escapes stay in logical order.
package arabic

import (
	"strings"
	"unicode"

	"golang.org/x/text/unicode/norm"
)

// Combining marks removed before folding. Whisper's Arabic output is usually
// undiacritised, but text pasted from other sources frequently is not, and a
// single mark is enough to break an exact match.
const (
	harakatStart = '\u064B' // ARABIC FATHATAN
	harakatEnd   = '\u065F' // ARABIC WAVY HAMZA BELOW; covers maddah and hamza marks
	quranicStart = '\u06D6' // ARABIC SMALL HIGH LIGATURE SAD WITH LAM WITH ALEF MAKSURA
	quranicEnd   = '\u06ED' // ARABIC SMALL LOW MEEM
	tatweel      = '\u0640' // ARABIC TATWEEL (kashida): typographic elongation only
	superAlef    = '\u0670' // ARABIC LETTER SUPERSCRIPT ALEF

	alef        = '\u0627' // ARABIC LETTER ALEF
	heh         = '\u0647' // ARABIC LETTER HEH
	yeh         = '\u064A' // ARABIC LETTER YEH
	waw         = '\u0648' // ARABIC LETTER WAW
	arabicZero  = '\u0660' // ARABIC-INDIC DIGIT ZERO
	arabicNine  = '\u0669' // ARABIC-INDIC DIGIT NINE
	persianZero = '\u06F0' // EXTENDED ARABIC-INDIC DIGIT ZERO
	persianNine = '\u06F9' // EXTENDED ARABIC-INDIC DIGIT NINE
)

// letterFolds collapses orthographic variants that users type interchangeably.
//
// The hamza-carrying alef forms are the highest-value entries: Whisper writes
// the hamza, phone keyboards routinely omit it, and without folding every such
// word is a zero-hit miss. Teh marbuta and heh are confused for the same reason.
var letterFolds = map[rune]rune{
	'\u0623': alef, // ALEF WITH HAMZA ABOVE
	'\u0625': alef, // ALEF WITH HAMZA BELOW
	'\u0622': alef, // ALEF WITH MADDA ABOVE
	'\u0671': alef, // ALEF WASLA
	'\u0629': heh,  // TEH MARBUTA
	'\u0649': yeh,  // ALEF MAKSURA
	'\u0624': waw,  // WAW WITH HAMZA ABOVE
	'\u0626': yeh,  // YEH WITH HAMZA ABOVE
}

// isInvisible reports zero-width and bidirectional control characters. These
// survive copy-paste from messaging apps and web pages, are not reliably treated
// as separators by the tokenizer, and are invisible to the user who cannot
// understand why their search failed.
func isInvisible(r rune) bool {
	switch {
	case r >= '\u200B' && r <= '\u200F': // ZWSP, ZWNJ, ZWJ, LRM, RLM
		return true
	case r == '\u061C': // ARABIC LETTER MARK
		return true
	case r >= '\u2066' && r <= '\u2069': // directional isolates
		return true
	case r == '\uFEFF': // ZERO WIDTH NO-BREAK SPACE / BOM
		return true
	}
	return false
}

// foldDigit maps Arabic-Indic and Extended Arabic-Indic digits to ASCII.
// Whisper emits Arabic-Indic numerals for Arabic input while users searching for
// a date, a price or a phone number type Latin digits.
func foldDigit(r rune) (rune, bool) {
	switch {
	case r >= arabicZero && r <= arabicNine:
		return '0' + (r - arabicZero), true
	case r >= persianZero && r <= persianNine:
		return '0' + (r - persianZero), true
	}
	return r, false
}

func isRemovableMark(r rune) bool {
	switch {
	case r >= harakatStart && r <= harakatEnd:
		return true
	case r >= quranicStart && r <= quranicEnd:
		return true
	case r == tatweel, r == superAlef:
		return true
	}
	return false
}

// Normalize prepares text for indexing or querying.
//
// It applies NFKC first, which decomposes Arabic Presentation Forms — the
// U+FB50..U+FDFF and U+FE70..U+FEFF blocks, including the lam-alef ligatures —
// into ordinary letters. Text pasted from PDFs and some Android keyboards
// arrives in those blocks, and no amount of letter folding would otherwise
// reach it.
//
// The result is intended for the FTS index only. It is not suitable for display:
// it is lossy, and it discards distinctions that matter when reading.
func Normalize(s string) string {
	s = norm.NFKC.String(s)

	var b strings.Builder
	b.Grow(len(s))

	for _, r := range s {
		if isInvisible(r) || isRemovableMark(r) {
			continue
		}
		if d, ok := foldDigit(r); ok {
			b.WriteRune(d)
			continue
		}
		if f, ok := letterFolds[r]; ok {
			b.WriteRune(f)
			continue
		}
		b.WriteRune(unicode.ToLower(r))
	}

	return strings.TrimSpace(b.String())
}

// BuildFTSQuery converts free-form user input into a safe FTS5 MATCH expression.
//
// User input must never reach MATCH directly. FTS5 treats a range of ordinary
// typing as query syntax, and the failures are hard errors rather than empty
// results: an apostrophe raises a syntax error, a hyphen is parsed as a column
// filter and reports "no such column", and a trailing boolean keyword or a bare
// parenthesis aborts the query. A search box wired straight to MATCH returns a
// 500 the first time someone types an English contraction.
//
// Each whitespace-separated term is normalised, stripped to letters and digits,
// and re-emitted as a double-quoted FTS5 string token. Quoted tokens carry no
// operator meaning, so nothing the user types can alter the query's structure.
// Terms are implicitly ANDed by FTS5.
//
// An empty string is returned when no usable term survives; callers must treat
// that as "no query" rather than passing it to MATCH.
func BuildFTSQuery(userInput string) string {
	fields := strings.Fields(Normalize(userInput))
	terms := make([]string, 0, len(fields))

	for _, f := range fields {
		var t strings.Builder
		for _, r := range f {
			if unicode.IsLetter(r) || unicode.IsDigit(r) {
				t.WriteRune(r)
			}
		}
		if t.Len() > 0 {
			terms = append(terms, `"`+t.String()+`"`)
		}
	}

	return strings.Join(terms, " ")
}
