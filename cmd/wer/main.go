// Command wer measures transcription accuracy against a reference corpus.
//
// maktoob's premise is that local whisper is good enough on Levantine Arabic
// voice notes to be worth reading instead of listening to. That is a claim about
// error rate, and until it is measured it is only a hope. This tool measures it.
//
// It deliberately runs the same path the product runs — internal/audio to
// convert, internal/asr to transcribe, the same pinned language and the same
// model — so the number describes what a user actually gets, not what a
// favourable harness could extract. It needs whisper-server running.
//
//	go run ./cmd/wer -corpus testdata/wer
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode"

	"github.com/ahmdobeidat/maktoob/internal/arabic"
	"github.com/ahmdobeidat/maktoob/internal/asr"
	"github.com/ahmdobeidat/maktoob/internal/audio"
)

func main() {
	corpus := flag.String("corpus", "testdata/wer", "directory holding the audio and reference.tsv")
	asrURL := flag.String("asr", "http://127.0.0.1:8642", "whisper-server base URL")
	lang := flag.String("lang", "ar", `language to transcribe as, or "auto" to detect`)
	jsonOut := flag.String("json", "", "also write the full per-file result to this path")
	flag.Parse()

	if err := run(*corpus, *asrURL, *lang, *jsonOut); err != nil {
		fmt.Fprintln(os.Stderr, "wer:", err)
		os.Exit(1)
	}
}

// entry is one measured file.
type entry struct {
	File       string  `json:"file"`
	Reference  string  `json:"reference"`
	Hypothesis string  `json:"hypothesis"`
	RefWords   int     `json:"ref_words"`
	Sub        int     `json:"sub"`
	Del        int     `json:"del"`
	Ins        int     `json:"ins"`
	WER        float64 `json:"wer"`
	CER        float64 `json:"cer"`
	DurationMS int64   `json:"duration_ms"`
}

func run(corpus, asrURL, lang, jsonOut string) error {
	refs, order, err := readRefs(filepath.Join(corpus, "reference.tsv"))
	if err != nil {
		return err
	}

	conv := audio.FFmpeg{}
	client := asr.NewClient(asrURL)
	if lang != "auto" {
		client.Language = lang
	}

	tmp, err := os.MkdirTemp("", "maktoob-wer-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)

	ctx := context.Background()
	var entries []entry
	var totRefWords, totSub, totDel, totIns int
	var totRefChars, totCharEdits int

	for _, name := range order {
		src := filepath.Join(corpus, name)
		wav := filepath.Join(tmp, strings.TrimSuffix(name, filepath.Ext(name))+".wav")

		durMS, err := conv.DurationMS(ctx, src)
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		if err := conv.ToWAV(ctx, src, wav); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}

		tctx, cancel := context.WithTimeout(ctx, asr.Timeout(durMS))
		res, err := client.Transcribe(tctx, wav)
		cancel()
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}

		var sb strings.Builder
		for _, seg := range res.Segments {
			sb.WriteString(seg.Text)
			sb.WriteString(" ")
		}
		hyp := strings.TrimSpace(sb.String())

		refW, hypW := tokens(refs[name]), tokens(hyp)
		sub, del, ins := align(refW, hypW)

		// Character rate is computed over the joined token stream, so it scores
		// spelling against a reference that has already had spacing normalised.
		refC := []rune(strings.Join(refW, " "))
		hypC := []rune(strings.Join(hypW, " "))
		cs, cd, ci := alignRunes(refC, hypC)

		e := entry{
			File:       name,
			Reference:  refs[name],
			Hypothesis: hyp,
			RefWords:   len(refW),
			Sub:        sub,
			Del:        del,
			Ins:        ins,
			WER:        rate(sub+del+ins, len(refW)),
			CER:        rate(cs+cd+ci, len(refC)),
			DurationMS: durMS,
		}
		entries = append(entries, e)

		totRefWords += len(refW)
		totSub, totDel, totIns = totSub+sub, totDel+del, totIns+ins
		totRefChars += len(refC)
		totCharEdits += cs + cd + ci

		fmt.Printf("%-14s  WER %6.1f%%  CER %6.1f%%  (%d words: %dS %dD %dI)\n",
			name, e.WER*100, e.CER*100, e.RefWords, sub, del, ins)
	}

	// The corpus-level rate pools every edit over every reference word. It is the
	// honest headline: averaging the per-file rates would let a six-word file
	// count as much as a forty-word one.
	corpusWER := rate(totSub+totDel+totIns, totRefWords)
	corpusCER := rate(totCharEdits, totRefChars)

	fmt.Printf("\ncorpus: %d files, %d reference words\n", len(entries), totRefWords)
	fmt.Printf("corpus WER %.1f%%  (%dS %dD %dI)\n", corpusWER*100, totSub, totDel, totIns)
	fmt.Printf("corpus CER %.1f%%\n", corpusCER*100)

	if jsonOut != "" {
		out := map[string]any{
			"corpus_wer": corpusWER,
			"corpus_cer": corpusCER,
			"ref_words":  totRefWords,
			"sub":        totSub,
			"del":        totDel,
			"ins":        totIns,
			"files":      entries,
		}
		b, err := json.MarshalIndent(out, "", "  ")
		if err != nil {
			return err
		}
		if err := os.WriteFile(jsonOut, append(b, '\n'), 0o600); err != nil {
			return err
		}
	}
	return nil
}

func rate(edits, n int) float64 {
	if n == 0 {
		return 0
	}
	return float64(edits) / float64(n)
}

// tokens normalises a transcript into the comparable word sequence.
//
// It reuses arabic.Normalize — the same folding the search index applies — so
// the score answers the question the product cares about: is the word the user
// would search for the word that got written down. That folds the hamza-carrying
// alef forms, teh marbuta and alef maksura, which speakers and keyboards use
// interchangeably and which no reader would call a transcription error.
//
// Punctuation is then dropped entirely. Whisper invents sentence-final marks
// that the reference script has no opinion about, and counting them would
// measure typography rather than words.
func tokens(s string) []string {
	var out []string
	for _, f := range strings.Fields(arabic.Normalize(s)) {
		var b strings.Builder
		for _, r := range f {
			if unicode.IsLetter(r) || unicode.IsDigit(r) {
				b.WriteRune(r)
			}
		}
		if b.Len() > 0 {
			out = append(out, b.String())
		}
	}
	return out
}

// align returns the substitution, deletion and insertion counts of the
// minimum-edit alignment between reference and hypothesis.
func align(ref, hyp []string) (sub, del, ins int) {
	return alignAny(len(ref), len(hyp), func(i, j int) bool { return ref[i] == hyp[j] })
}

func alignRunes(ref, hyp []rune) (sub, del, ins int) {
	return alignAny(len(ref), len(hyp), func(i, j int) bool { return ref[i] == hyp[j] })
}

// alignAny is Levenshtein with backtracking, kept generic over the equality test
// so words and characters share one implementation.
//
// The three operation counts are reported separately rather than as a single
// distance because they diagnose different failures: deletions mean whisper
// dropped speech, insertions mean it hallucinated, and substitutions mean it
// heard the wrong word. A 20% WER made of deletions is a different product than
// a 20% WER made of insertions.
func alignAny(n, m int, eq func(i, j int) bool) (sub, del, ins int) {
	const (
		opMatch = iota
		opSub
		opDel
		opIns
	)

	cost := make([][]int, n+1)
	op := make([][]int, n+1)
	for i := range cost {
		cost[i] = make([]int, m+1)
		op[i] = make([]int, m+1)
	}
	for i := 1; i <= n; i++ {
		cost[i][0] = i
		op[i][0] = opDel
	}
	for j := 1; j <= m; j++ {
		cost[0][j] = j
		op[0][j] = opIns
	}

	for i := 1; i <= n; i++ {
		for j := 1; j <= m; j++ {
			if eq(i-1, j-1) {
				cost[i][j], op[i][j] = cost[i-1][j-1], opMatch
				continue
			}
			s, d, in := cost[i-1][j-1]+1, cost[i-1][j]+1, cost[i][j-1]+1
			cost[i][j], op[i][j] = s, opSub
			if d < cost[i][j] {
				cost[i][j], op[i][j] = d, opDel
			}
			if in < cost[i][j] {
				cost[i][j], op[i][j] = in, opIns
			}
		}
	}

	for i, j := n, m; i > 0 || j > 0; {
		switch op[i][j] {
		case opMatch:
			i, j = i-1, j-1
		case opSub:
			sub++
			i, j = i-1, j-1
		case opDel:
			del++
			i--
		default:
			ins++
			j--
		}
	}
	return sub, del, ins
}

// readRefs loads the reference TSV, returning the transcripts and the file order.
func readRefs(path string) (map[string]string, []string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, err
	}
	refs := map[string]string{}
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name, text, ok := strings.Cut(line, "\t")
		if !ok {
			return nil, nil, fmt.Errorf("%s: line is not <file>\\t<transcript>: %q", path, line)
		}
		refs[strings.TrimSpace(name)] = strings.TrimSpace(text)
	}
	if len(refs) == 0 {
		return nil, nil, fmt.Errorf("%s: no reference lines", path)
	}
	order := make([]string, 0, len(refs))
	for k := range refs {
		order = append(order, k)
	}
	sort.Strings(order)
	return refs, order, nil
}
