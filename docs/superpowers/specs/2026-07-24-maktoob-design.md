# maktoob — Design Spec

> Date: 2026-07-24
> Event: JOSA Reclaim Hackathon 2026 (hacking period Jul 18 – Aug 1, 2026)
> Revision: 2 — rewritten after adversarial review
> License: MIT

## 1. Problem

Deaf and hard-of-hearing people in Jordan are locked out of the conversations their
communities run on. WhatsApp is the default channel for family, work, and
neighbourhood coordination here, and a large share of that traffic is voice notes.
A voice note is opaque: it cannot be read, skimmed, quoted, or searched.

Existing options each fail for a different reason:

- WhatsApp's own transcription is unavailable on most devices in the region, is
  closed, and makes decisions about private audio that nobody can inspect.
- Third-party transcription apps upload private family audio to someone else's
  server.
- Asking senders to "just type it" moves the burden onto the deaf user's community
  and does not scale past one or two willing contacts.

The user should not have to leave WhatsApp, and neither should anyone who messages
them. The accessibility layer is added on the receiving side only.

## 2. What we are building

A single Go program the user runs on their own machine. It links to WhatsApp as a
companion device — the same mechanism as WhatsApp Web — captures incoming voice
notes, transcribes them locally with whisper.cpp, and presents them as readable,
searchable, correctable text in a browser tab.

No audio and no transcript leaves the device, except the audio's own round trip
from WhatsApp's servers.

**Track:** 1 — UX & Accessibility. Track 3 (bridges) is a secondary framing, since
the ingest side is a per-platform adapter in the matterbridge/nchat shape.

**Name:** `maktoob` ("written"). See §15.

## 3. Core design principle

**The transcript is a view onto the audio, never a replacement for it.**

Whisper is measurably weaker on Levantine Arabic than on English or MSA. We design
around that rather than hiding it:

- Every segment carries a confidence value derived from real token probabilities.
  Low-confidence segments are visually marked, not silently presented as fact.
- The audio player is always adjacent to the text. Clicking a segment seeks the
  player to that segment's timestamp.
- Any segment is editable inline. The ASR output is preserved alongside the human
  correction; a correction annotates, it never overwrites.
- Suspected hallucinations are marked distinctly from low confidence (§7).

**This principle is conditional, and the condition is measured, not assumed.**
Published work puts Whisper around 16% WER on MSA against roughly 58% averaged
across Arabic dialects, with Levantine among its stronger dialects — but still
degrading significantly zero-shot. Below roughly 35% WER an editable transcript
with audio seek is a real accessibility gain: the user reads most of it and fixes a
few words. Above roughly 50% it inverts, and correction becomes slower than asking
the sender to retype — actively misleading for a user who cannot check the audio.

§12 D2 measures which side of that line we are on before any interface is built on
top of the assumption. The measured numbers go in the README. An accessibility jury
will trust a project that publishes its own error rate more than one that claims to
have "designed around imperfection."

## 4. Architecture

Two ingest sources feed one identical downstream pipeline. The offline path is
therefore nearly free — it is the same pipeline with whatsmeow removed.

```
  [whatsmeow live]  ──┐
                      ├──> audio blob + metadata ──> ffmpeg ──> whisper-server
  [POST /import]    ──┘         (data/media)        (16k mono wav)   (segments)
                                                                         │
                                       SQLite + FTS5  <─────────────────┘
                                              │
                            ┌─────────────────┼──────────────────┐
                        SSE push        normalized FTS         export
                       (browser)        search + edit        (JSON / MD)
```

### Package layout

```
cmd/maktoob/          CLI entry: pair | serve | import
internal/wa/          whatsmeow: pairing, event loop, PTT filter, media download
internal/audio/       ffmpeg wrapper: ogg/opus -> 16kHz mono wav
internal/asr/         whisper-server supervision, request/response, segment parsing
internal/arabic/      orthographic normalisation for indexing and querying
internal/store/       SQLite schema, queries, FTS5 index
internal/web/         HTTP handlers, SSE hub, templates, static assets
internal/export/      JSON / Markdown serialisers
```

Each package has one job and a small surface. `internal/asr` does not know what
WhatsApp is; `internal/wa` does not know what a transcript is. They meet at the
store. That boundary is also the platform-risk answer in §10.

### External dependencies and the cgo decision

`ffmpeg` and whisper.cpp's `whisper-server` are invoked as subprocesses, not linked
via cgo. Cost: the deliverable is a Go binary plus two documented system
dependencies rather than one self-contained executable. Benefit: no cgo build
chain, no platform-specific compilation for a judge who wants to run it, and
whisper.cpp upgrades independently. In an 8-day window, cgo binding failures are a
schedule risk we decline to take.

whisper.cpp is run as a **long-lived `whisper-server` process** rather than one
`whisper-cli` invocation per note. Spawning per note re-reads a ~1.6 GB model from
disk every time, costing seconds of pure overhead per transcription. A supervised
resident server keeps the model in memory, keeps cgo at zero, and gives us a
natural place to enforce per-request timeouts.

SQLite uses **`modernc.org/sqlite`** — pure Go, FTS5 compiled in, no build tags, no
cgo. The cgo driver `mattn/go-sqlite3` would require both cgo and a non-obvious
`-tags fts5`, breaking the "clone it and run one binary" story. The pure-Go driver
is slower; at this data volume that is irrelevant.

**Compute: CPU-only.** The machine has an RTX 4050 (6 GB) but no CUDA toolkit
installed, and installing it is a multi-gigabyte, plausibly half-day detour on the
one day that already does not fit. 16 cores run `large-v3-turbo` at roughly
0.3–0.5x real time. The decision and the measured latency go in the README.

## 5. Data model

```sql
CREATE TABLE chats (
  id           TEXT PRIMARY KEY,   -- WhatsApp JID, or 'import' for uploads
  display_name TEXT NOT NULL,
  created_at   INTEGER NOT NULL
);

CREATE TABLE notes (
  id            TEXT PRIMARY KEY,   -- uuid
  chat_id       TEXT NOT NULL REFERENCES chats(id),
  source        TEXT NOT NULL,      -- 'whatsapp' | 'import'
  sender        TEXT,               -- push name or JID; NULL for imports
  wa_message_id TEXT UNIQUE,        -- dedupe key for live ingest; NULL for imports
  media_path    TEXT NOT NULL,      -- original ogg/opus
  wav_path      TEXT,
  duration_ms   INTEGER,
  received_at   INTEGER NOT NULL,
  status        TEXT NOT NULL,      -- pending|converting|transcribing|done|failed
  attempts      INTEGER NOT NULL DEFAULT 0,
  error         TEXT,
  model         TEXT
);

CREATE TABLE segments (
  id           INTEGER PRIMARY KEY,
  note_id      TEXT NOT NULL REFERENCES notes(id) ON DELETE CASCADE,
  idx          INTEGER NOT NULL,
  start_ms     INTEGER NOT NULL,
  end_ms       INTEGER NOT NULL,
  asr_text     TEXT NOT NULL,       -- never mutated after insert
  edited_text  TEXT,                -- NULL = untouched by human
  confidence   REAL,                -- mean token probability, 0..1
  suspect      INTEGER NOT NULL DEFAULT 0,  -- 1 = possible hallucination
  edited_at    INTEGER,
  UNIQUE(note_id, idx)
);

CREATE VIRTUAL TABLE segments_fts USING fts5(
  text,                    -- Arabic-normalised COALESCE(edited_text, asr_text)
  note_id    UNINDEXED,
  segment_id UNINDEXED
);
```

There is no `lang_override` column and no language auto-detection. Whisper's
auto-detect runs on the first 30-second window; on a 10-second Levantine note it is
close to a coin flip, and a wrong detection produces catastrophic garbage that the
confidence marker will not flag. Language is pinned to Arabic via config (`-l ar`),
overridable by flag.

### Arabic search normalisation

SQLite's `unicode61` tokenizer classifies Arabic harakat (U+064B–U+0652) as
**separators**, which shreds diacritised Arabic into individual letters — indexing
`مَرْحَبًا كَيْفَ حَالَك` yields the terms `ا, ال, ب, ح, ر, ف, ك, م, ي` and a query for
`مرحبا` returns nothing. The `remove_diacritics` option does not help: its table
covers precomposed Latin/Greek/Cyrillic forms, and Arabic harakat are standalone
combining marks. Verified identical output at settings 0, 1, and 2.

The option is therefore omitted, and normalisation happens in Go instead — in
`internal/arabic`, applied identically on insert and on query:

- strip U+064B–U+0652 (harakat), U+0640 (tatweel), U+0670 (superscript alef)
- fold `أ إ آ ٱ → ا`, `ة → ه`, `ى → ي`, `ؤ → و`, `ئ → ي`

The second group is the one that matters most in practice. Whisper's Arabic output
is usually undiacritised anyway, so the real-world failure is orthographic variance:
Whisper writes `أحمد`, `إسلام`, `مدرسة`; a user on a phone keyboard types `احمد`,
`اسلام`, `مدرسه`. Every one of those is a zero-hit miss without folding.

`segments` stores display text; `segments_fts.text` stores the normalised form. The
FTS row is rewritten whenever a correction is saved.

### Confidence

whisper.cpp's JSON output does **not** contain `avg_logprob` or any segment-level
score — that field belongs to OpenAI's Python `whisper`, a different project. The
only probability signal available is per-token `p`, exposed by `--output-json-full`.

Confidence is the arithmetic mean of token `p` across a segment, **excluding
timestamp and special tokens** (text matching `^\[_`, or id `>= whisper_token_eot`).
Those carry near-1.0 probability and would push every segment toward 1.0.

Display bands start at `< 0.50` low / `0.50–0.75` medium / `> 0.75` unmarked. These
are provisional placeholders for a metric we have not yet observed, set properly
after the D2 bake-off, and documented as provisional in the README. The raw value
is always stored.

## 6. Processing model

A single worker goroutine drains the queue. Whisper saturates CPU; concurrency buys
nothing and complicates failure handling.

The queue is the `notes.status` column, not an in-memory channel. On startup, rows
left in `converting` or `transcribing` reset to `pending`. A crash mid-job costs one
re-transcription, never a lost note.

Every subprocess and inference request runs under `exec.CommandContext` /
`context.WithTimeout` at `max(60s, 10 × duration_ms)`. Whisper's repetition-loop
failure mode on degenerate audio can spin far past real time; without a bound, one
bad note stalls the queue and the restart logic re-queues it forever. `attempts` is
incremented per try and a note is marked permanently `failed` after 2.

Failures write `status='failed'` plus the error text, and the note still appears in
the UI as a playable audio row with a visible error. **A failed transcription must
never make the voice note disappear.**

## 7. Hallucination handling

Whisper's worst Arabic failure mode is not garbled text — it is fluent, fabricated
text on silence or noise. On silent audio the Arabic decoder reliably emits a
YouTube subtitle credit line absorbed from training data. These tokens carry *high*
probability, so §5's confidence score marks them as trustworthy. The safety
mechanism would vouch for the fabrication. For a user who cannot check against the
audio, that is the worst available failure.

Three layers, all cheap:

1. **VAD before decoding** — run with `--vad` and a Silero VAD model so non-speech
   never reaches the decoder.
2. **Token suppression** — `--suppress-nst` plus `--suppress-regex` seeded with
   known credit-line strings.
3. **Post-filter** in `internal/asr` — flag any segment repeating a previous segment
   verbatim, or matching a small committed blocklist. Sets `segments.suspect = 1`.

Suspect segments render with a distinct marker, separate from low confidence, and
the README names this failure mode explicitly.

## 8. HTTP surface

| Method | Path | Purpose |
|---|---|---|
| GET | `/` | Note list, newest first, with search box |
| GET | `/note/{id}` | Detail: player + segments |
| GET | `/events` | SSE: new note, status change, transcript ready |
| GET | `/api/notes?q=&chat=&limit=` | JSON list; `q` runs normalised FTS5 |
| GET | `/api/notes/{id}` | JSON single note with segments |
| GET | `/api/notes/{id}/audio` | Original ogg/opus, via `http.ServeContent` |
| PATCH | `/api/segments/{id}` | Save a correction |
| GET | `/api/notes/{id}/export?format=json\|md` | Export one note |
| POST | `/import` | Multipart upload of an audio file |

`/api/notes/{id}/audio` serves the **original ogg/opus**, not the 16 kHz mono wav —
the wav is a downsampled artifact that sounds worse, and both Chrome and Firefox
play ogg/opus natively. `http.ServeContent` gives Range support, without which
seeking in a long note fails in some browsers. The path is resolved by note ID
through the store, never from user input.

There is no outbound webhook. A fire-and-forget webhook with no HMAC signature and
no retry is a *weaker* extensibility story to a maintainer judge than none at all.
Criterion 6 is satisfied by the documented read API plus JSON and Markdown export,
both of which the criterion names explicitly. `docs/API.md` documents the surface.

Server binds `127.0.0.1` by default.

## 9. Web UI

Server-rendered Go templates plus vanilla JavaScript. No framework, no npm, no
build step. A judge clones the repo and runs one binary.

Two screens:

1. **List** — reverse-chronological notes: sender, chat, duration, timestamp,
   status, first line of transcript. Search filters via normalised FTS5. New notes
   arrive live over SSE.
2. **Detail** — audio player pinned, segments beneath. Each segment shows timestamp,
   text, confidence marker, and suspect marker where set. Click seeks the player.
   Click-to-edit saves via PATCH; edited state stays visibly marked as edited.

### Arabic rendering (non-negotiable)

Track 1 raises the bar on the interface specifically, and this is an Arabic
interface judged in Jordan:

- `lang="ar" dir="rtl"` on the transcript container
- every LTR run — timestamps, durations, Latin sender names — wrapped in `<bdi>` or
  `dir="ltr"`, or punctuation visibly jumps to the wrong end of lines
- CSS logical properties (`margin-inline-start`, never `margin-left`) so layout
  mirrors correctly

### Accessibility (non-negotiable)

An accessibility project whose own interface fails scrutiny is disproportionately
damaged. Judges will check.

- semantic HTML; every control reachable and operable by keyboard alone
- visible focus states throughout
- WCAG AA contrast minimum
- SSE updates announced via `aria-live="polite"`
- labelled controls; no icon-only buttons without accessible names
- confidence and suspect state never encoded by colour alone — colour plus marker
  plus text

## 10. Privacy and platform risk

`docs/PRIVACY.md` states all of this in plain language. Precision here matters more
than a strong claim: a privacy-first project caught overstating its own privacy is
worse off than one that never claimed it.

**Outbound connections, exhaustively:** WhatsApp and Meta's media CDN; a one-time,
explicit model download. Nothing else. No telemetry, no analytics, no third-party
transcription service.

**What companion pairing actually grants.** Linking gives maktoob a full multi-device
session: all chats, all incoming message content, the contact list, and a history
sync payload — not only voice notes from selected contacts. maktoob discards the
rest. That is a policy, not a capability boundary, and the document says so.

**Data at rest.** Media, database, and the whatsmeow session store all live under one
`data/` directory, unencrypted. The session store contains the account's **identity
keys**: anyone holding that directory can impersonate the WhatsApp account. It is
created `0600`, `data/` is gitignored, and PRIVACY.md warns that backing the
directory up copies account credentials.

**Platform risk.** whatsmeow is an unofficial, reverse-engineered client. Using it
is against WhatsApp's terms, accounts using it can be banned, and Meta can change
the protocol at any time. Saying nothing reads worse than disclosing it, because
maintainer judges will spot it unprompted. The honest answer is architectural: §4 is
a two-source pipeline behind a stable internal boundary, so `internal/wa` is one
replaceable adapter and everything downstream survives its loss. The README says
this, and the `/import` path proves it.

## 11. Out of scope

Deliberately excluded. The exclusions are the scope discipline the rules reward:

- sending messages, replying, or any write path back to WhatsApp
- group management
- multi-user accounts or auth
- any hosted or cloud component
- a mobile app
- speaker diarisation
- **LLM summarisation of any kind** — buzzword surface with no user need behind it,
  and it would read as slop to this jury
- outbound webhooks (§8)
- language auto-detection (§5)

**Why not on the phone?** Because local Whisper needs compute a phone will not give
at usable latency, and companion pairing means the phone keeps working untouched.
This is the receiving-side layer for someone at a desk. Named as future work in the
README rather than left to be found in Q&A.

## 12. Schedule (Jul 24 → Aug 1)

| Day | Deliverable |
|---|---|
| D1 (Jul 24, evening) | cmake, whisper.cpp build, model download, Go toolchain bump, repo scaffold, MIT LICENSE, gitignore, README problem statement |
| D2 (Jul 25) | whatsmeow pairing, event loop, PTT filter, media download, dedupe — **then the bake-off**: 10 real voice notes from paired history across `large-v3-turbo` / `medium` / `small`, record WER and wall-clock, pick the model, set confidence bands |
| D3 (Jul 26) | store, `internal/arabic` + normalised FTS, `whisper-server` runner, `-ojf` token-`p` confidence, ffmpeg pipeline — tests written alongside, not after |
| D4 (Jul 27) | web: list, detail, SSE, **audio endpoint + player + segment seek**, RTL |
| D5 (Jul 28) | FTS search UI, inline correction, hallucination filter, export, remaining tests, CI |
| D6 (Jul 29) | README with measured WER and latency, PRIVACY.md, platform-risk section, API.md, architecture diagram, screenshots, **record the demo video** |
| D7 (Jul 30) | accessibility + RTL pass, rehearsal against the run-sheet, seed data, polish |
| D8 (Jul 31) | buffer, final docs pass, submit a day ahead of the Aug 1 deadline |

Pairing on D2 rather than D3 resolves the project's highest-variance unknown while
there is still time to react, and it supplies the bake-off corpus as a side effect.

Commits land incrementally throughout each day. Judges read git history, and thin or
bulk-dumped history reads as AI slop under criterion 7.

## 13. Testing

Tests are written on the same day as the code they cover (§12 D3 and D5), not
retrofitted. Under criterion 7 they are the primary structural evidence that this
is maintainable rather than generated: an 8-package Go repo with no tests and a fast
commit history invites exactly the conclusion we are avoiding.

- `internal/arabic`: the five known-failing query pairs (`أحمد`/`احمد`,
  `إسلام`/`اسلام`, `مدرسة`/`مدرسه`, `على`/`علي`, diacritised/undiacritised) must
  match. A test that merely round-trips through FTS would pass while search is
  broken; these assert the property that actually matters.
- `internal/store`: schema, FTS insert/update on correction, `asr_text` immutability,
  restart requeue, `attempts` cap.
- `internal/audio`: conversion of a committed 3-second fixture, format assertions.
- `internal/asr`: parsing of a committed `-ojf` fixture, confidence excluding special
  tokens, malformed-output handling, hallucination post-filter.
- `internal/export`: golden-file JSON and Markdown.
- `internal/web`: handler tests over a seeded store; PATCH updates FTS; audio
  endpoint serves Range requests.
- One end-to-end test: fixture audio through convert → transcribe → store → export
  using `tiny`, skipped when the model is absent.

`ffmpeg` and `whisper-server` sit behind an interface so unit tests use a fake. CI
runs on GitHub Actions; a green badge is a cheap maintainability signal for
maintainer judges under criterion 4.

## 14. Risks

| Risk | Mitigation |
|---|---|
| Levantine WER above the usable band | Measured on D2 before the interface is built. If catastrophic, reframe as a searchable voice-note archive with playback and human transcription assist — a framing that survives 50% WER, with 6 days still in hand |
| Hallucination on silence marked as high confidence | §7: VAD, token suppression, repeat post-filter, distinct `suspect` marker |
| Arabic search silently returns nothing | §5 normalisation, with the §13 tests that would actually fail on regression |
| Real WhatsApp account banned mid-event | Accepted risk, user's decision. `/import` keeps the full pipeline demoable without WhatsApp; §10 discloses the exposure |
| Session store leaks account identity keys | `0600`, gitignored, documented in PRIVACY.md |
| whatsmeow requires a newer Go toolchain | D1 task; `GOTOOLCHAIN=auto` handles it, budget the download |
| cmake absent, whisper.cpp build fails | D1 first task; it gates everything |
| Poison-pill note stalls the queue | §6 timeout plus `attempts` cap |
| Live demo failure | Pair beforehand, never on stage. Fallback is pre-seeded DB plus `/import`. D6 produces a recorded screencast; D7 produces a written run-sheet with an explicit decision point |
| Scope creep | §11 is binding |

## 15. Open items for Ahmad

1. **Talk to one or two deaf or hard-of-hearing people before D6.** Criterion 2 asks
   for real users *named*; right now they are asserted, not evidenced. Two quoted
   sentences in the README are worth more than anything on the cut list. Highest-
   leverage non-code hour available.
2. **Confirm the name `maktoob` before D1 code lands.** Cheap now, annoying later.
