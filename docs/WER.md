# Word error rate on Levantine Arabic

**Measured 2026-08-01. Corpus word error rate: 54%. Character error rate: 20%.**

This document replaces the "not measured yet" admission that stood in the README
until now. The number is bad, it is published anyway, and the rest of this page
is about what it does and does not mean.

## What was measured

Ten WhatsApp voice notes, 95.5 seconds of speech, 200 reference words. The
speaker wrote the scripts, read them aloud into WhatsApp, and supplied the
scripts as the reference. Nine notes are Opus `.ogg` as WhatsApp records them;
one is `.m4a`.

The reference transcripts live in
[`testdata/wer/reference.tsv`](../testdata/wer/reference.tsv), so the scoring is
open to inspection and argument.

**The audio itself is deliberately not committed.** A recorded voice identifies
the person who recorded it, and publishing family voice notes permanently to a
public repository in order to prove a point about voice-note privacy would be a
poor trade. That does cost exact reproducibility: the table below can be audited
but not regenerated without the recordings. The harness runs on any corpus laid
out the same way, so an independent measurement is a matter of supplying ten
notes and their transcripts.

Transcription ran through the same code path the product runs: `internal/audio`
to convert with ffmpeg, `internal/asr` to call `whisper-server`, the default
`ggml-large-v3-turbo` model, language pinned to `ar`, temperature 0. No tuning,
no prompt, no per-file retries. The number describes what a user gets.

## Results

| File | Seconds | Ref words | WER | CER | Operations |
|---|---|---|---|---|---|
| `audio-01.ogg` | 6.0 | 18 | 50% | 12% | 9S 0D 0I |
| `audio-02.ogg` | 5.5 | 14 | 21% | 5% | 2S 1D 0I |
| `audio-03.m4a` | 14.3 | 14 | 57% | 21% | 6S 2D 0I |
| `audio-04.ogg` | 13.9 | 44 | 75% | 31% | 24S 9D 0I |
| `audio-05.ogg` | 15.9 | 27 | 37% | 8% | 7S 2D 1I |
| `audio-06.ogg` | 5.9 | 18 | 50% | 20% | 7S 2D 0I |
| `audio-07.ogg` | 10.3 | 13 | 54% | 9% | 5S 2D 0I |
| `audio-08.ogg` | 7.9 | 14 | 64% | 29% | 6S 2D 1I |
| `audio-09.ogg` | 6.8 | 15 | 73% | 45% | 9S 1D 1I |
| `audio-10.ogg` | 9.0 | 23 | 39% | 10% | 6S 2D 1I |
| **corpus** | **95.5** | **200** | **54%** | **20%** | **81S 23D 4I** |

The corpus rate pools every edit over every reference word rather than averaging
the per-file rates, so a 13-word note does not count as much as a 44-word one.

## How words are compared

Scoring folds orthographic variants before comparing, using the same
`internal/arabic` normalisation the search index uses: hamza-carrying alef forms,
teh marbuta and alef maksura collapse, diacritics and tatweel are stripped, and
punctuation is dropped. Whisper writes the hamza and phone keyboards omit it;
counting that as a transcription error would measure keyboards rather than
hearing. Punctuation is dropped because whisper invents sentence-final marks that
a spoken script has no opinion about.

This folding makes the reported number **lower** than a raw string comparison
would give. It is the generous reading, and it is still 54%.

## What the 54% is made of

The gap between 54% WER and 20% CER is the whole story: these are mostly
near-misses, not nonsense. Breaking the 108 edits down:

| Class | Count | Example |
|---|---|---|
| Substitutions within 2 characters | 44 | `لسا` → `لسه`, `اكتر` → `اكثر`, `بالزبط` → `بالضبط` |
| Substitutions genuinely wrong | 37 | `عالسباحة` → `روحت`, `سهل` → `الشخص` |
| Deletions | 23 | 8 of them glued into a neighbouring word |
| Insertions | 4 | |

Two thirds of the errors are the model writing a real word that a reader
recovers without effort — Modern Standard spellings of dialect words (`بالمي` →
`بالماء`, `كتير` → `كثير`), a clitic split off (`احكيلك` → `احكي لك`), a
conjunction absorbed into the next word. Whisper is pulling Levantine toward
Modern Standard Arabic, which is its documented weakness on dialect.

The genuinely wrong third is not evenly spread. **`audio-04` alone contributes 33
of the 108 edits from 22% of the words**, including 17 of the 37 real
substitutions. It is the longest and fastest note, run-on speech with no pauses,
and the model loses an entire opening clause and invents a question that was
never said. Excluding it, the corpus rate falls to 48% — still bad, so the
outlier is not an excuse, but the failure mode is specific: **sustained fast
speech degrades much worse than short notes.**

## What this means for the product

It means a transcript here is a draft, not a record. That is what the interface
was already built to assume, and this measurement is the justification for that
design rather than a discovery that undermines it:

- every segment carries the model's own confidence
- likely fabrications are flagged separately from low confidence
- the audio sits next to the text at the segment that produced it
- any line can be corrected, with or without JavaScript

What it does **not** support is anyone relying on an uncorrected transcript as an
accessibility substitute for the audio. At 54% WER a deaf or hard-of-hearing
reader gets the gist of a short note and gets misled by a long one. That
limitation belongs in the demo and in the README, stated plainly, not softened.

## Limits of this measurement

Read honestly, this is a first measurement and not a benchmark:

- **n=10, 95 seconds.** Every per-file rate has an error bar wide enough to drive
  through. The corpus rate is the only figure worth quoting.
- **Read speech, not spontaneous.** The speaker read prepared scripts. Real voice
  notes have false starts, laughter and background noise this corpus lacks — but
  they also have natural prosody that read-aloud text lacks. Which direction this
  biases the result is not known.
- **Speaker count and recording conditions are not controlled**, so this does not
  separate speaker-specific effects from dialect effects.
- **One model.** `large-v3-turbo` is the default because it is fast on CPU; turbo
  variants are distilled for speed and are known to give up more accuracy on
  non-English audio than on English. A `large-v3` run is the obvious next
  experiment and has not been done.

## Reproducing it

Put the audio in `testdata/wer/` under the names `reference.tsv` lists, start
`whisper-server` as [INSTALL.md](INSTALL.md) describes, then:

```sh
go run ./cmd/wer -corpus testdata/wer
```

`-json out.json` writes every reference and hypothesis pair for inspection.
`-lang auto` and `-asr` change detection and the server address.

The scoring code has its own tests (`go test ./cmd/wer`), including hand-worked
alignment cases, because a published error rate is only as trustworthy as the
aligner that produced it.
