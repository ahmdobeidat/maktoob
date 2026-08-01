# maktoob

**Read your WhatsApp voice notes. On your own machine. Without sending them to anyone.**

maktoob links to WhatsApp as a companion device, transcribes incoming voice notes
locally with [whisper.cpp](https://github.com/ggml-org/whisper.cpp), and shows them
as readable, searchable, correctable text in your browser. No audio and no
transcript is sent to any third party.

> **Status: in active development** during the JOSA Reclaim Hackathon
> (18 July – 1 August 2026). This README describes what is built; features still
> in progress are marked as such in [the design spec](docs/superpowers/specs/2026-07-24-maktoob-design.md).
>
> **Measured: 54% word error rate on Levantine Arabic** (20% character error
> rate, 10 notes, 95 seconds). It is the number that decides whether a
> transcript here is a document or a draft, and it says draft. The full
> measurement, including what the errors are made of, is in
> [docs/WER.md](docs/WER.md).

---

## The problem

In Jordan, WhatsApp is where family, work, and neighbourhood coordination happens,
and a large share of that traffic is voice notes. A voice note is opaque. It cannot
be read, skimmed, quoted, or searched. If you are deaf or hard of hearing, the
default medium of your own community is closed to you.

The existing options each fail for a different reason:

- **WhatsApp's own transcription** is unavailable on most devices in the region, is
  closed, and makes decisions about private audio that nobody outside Meta can inspect.
- **Third-party transcription apps** solve the accessibility problem by uploading
  private family audio to someone else's server. That is a trade, not a solution.
- **Asking people to "just type it"** moves the burden onto the deaf person's
  community, and it does not scale past one or two willing contacts.

maktoob adds the accessibility layer on the **receiving side only**. Nobody has to
migrate, install anything, or change how they message you. The people who send you
voice notes never know it is there.

How well it does that is measured rather than asserted, and the honest answer is
"partially": 54% word error rate on Levantine Arabic ([docs/WER.md](docs/WER.md)).
That is enough to skim, search and quote a short note, and not enough to trust a
long one without checking it against the audio.

### Who this is for

maktoob is built for **late-deafened and hard-of-hearing adults who read Arabic** —
a large group for whom a written transcript is a direct, immediate win.

It is deliberately *not* claiming to serve prelingually deaf signers whose first
language is Jordanian Sign Language, and for whom written Arabic is a second
language. A text transcript is a weaker tool there. Sign-language output is real
future work, not something this project quietly pretends to cover.

## How it works

```
  [WhatsApp companion]  ──┐
                          ├──> audio + metadata ──> ffmpeg ──> whisper-server
  [file upload]         ──┘        (local disk)     (16k wav)     (segments)
                                                                      │
                                     SQLite + FTS5  <────────────────┘
                                            │
                          ┌─────────────────┼──────────────────┐
                      live updates     Arabic search      export
                        (SSE)          + correction     (JSON / MD)
```

Two ways in, one pipeline. The file-upload path means maktoob works completely
without WhatsApp — which matters more than it sounds like (see *Platform risk*).

### The transcript is a view onto the audio, never a replacement for it

Whisper is known to be weaker on Levantine Arabic than on English or Modern Standard
Arabic. maktoob is designed around that rather than hiding it:

- Every segment carries a confidence score derived from the model's own token
  probabilities. Uncertain segments are visibly marked.
- Segments that look like **hallucinations** — Whisper's habit of inventing fluent
  text over silence — are marked separately, from three signals: the model's own
  no-speech probability, a list of known Whisper artifact phrases, and verbatim
  repetition of the previous line. A fabrication with high token confidence is the
  most dangerous failure mode for someone who cannot check the audio, so it gets
  its own signal rather than being folded into the confidence score.
- The audio player sits next to the text. Clicking any line seeks to that moment.
- Any line can be corrected inline. The machine's original output is kept alongside
  your correction, never overwritten.

Latency is measured and published in [docs/INSTALL.md](docs/INSTALL.md):
roughly 13–15 seconds per note on the development machine, CPU-only, and roughly
flat across the note lengths tested. That is a handful of notes on one machine,
not a benchmark.

Word error rate on Levantine Arabic **is measured: 54%**, with a 20% character
error rate, over ten voice notes and 95 seconds of speech. The full method,
per-file table and error breakdown are in [docs/WER.md](docs/WER.md).

That is a bad number and it is published rather than buried. It is also a
specific kind of bad: the gap between 54% word errors and 20% character errors
means most errors are near-misses a reader reads straight through — Modern
Standard spellings of dialect words, a clitic split off, a conjunction absorbed
into its neighbour. Sustained fast speech is where it genuinely breaks down.

So the transcript is a draft, which is what this interface already assumes. What
the number does not support is treating an uncorrected transcript as an
accessibility substitute for the audio.

## Privacy

The full document is **[docs/PRIVACY.md](docs/PRIVACY.md)** — what is stored,
where, what pairing actually grants, and what maktoob cannot protect you from.
The short version:

- **Outbound connections while running:** WhatsApp and Meta's media CDN, where the
  audio already lives. That is all. Installing additionally reaches GitHub, the Go
  module proxy and Hugging Face; PRIVACY.md lists every one. No telemetry, no
  analytics, no transcription service, ever.
- **What linking actually grants:** pairing gives maktoob a full multi-device
  session — all chats, all incoming message content, and your contact list, not just
  voice notes. maktoob discards the rest. That is a policy decision, not a technical
  boundary, and you should know the difference before you scan the QR code.
- **Other people's data:** the people who send you voice notes did not consent to
  this tool. maktoob stores a salted local alias instead of a phone number and never
  persists view-once media — but a linked device still sends delivery receipts, so
  senders see their notes marked delivered.
- **Data at rest:** everything lives under `data/`, unencrypted. That directory
  holds your account's **identity keys** — anyone who copies it can impersonate your
  WhatsApp — and whatsmeow's own contact table, which stores phone numbers in the
  clear. It is created `0700`, the files inside it `0600`, and it is gitignored.
  Backing it up copies your credentials.
- **Unlinking and deletion:** `maktoob logout` revokes the companion session and
  leaves your transcripts alone. `maktoob purge` deletes the media, the
  database, its write-ahead log, the session and the alias salt — it shows you
  the list first and makes you type `yes`. Run `logout` **before** `purge`:
  deleting the local session does not tell WhatsApp anything, so purging first
  leaves a live linked device on your account. There is no hidden state anywhere
  else on the machine.
- **Verbose logging is off by default:** `-verbose` forwards whatsmeow's own
  diagnostics to stderr, and those contain contacts' phone numbers in the clear.
  Do not use it where stderr is redirected to a file or captured by a service
  manager.

## Platform risk

maktoob talks to WhatsApp through [whatsmeow](https://github.com/tulir/whatsmeow),
an unofficial, reverse-engineered client. Using it is against WhatsApp's terms of
service, accounts using it **can be banned**, and Meta can change the protocol at
any time. Understand that before pairing an account you care about.

This is why the file-upload path exists and why `internal/wa` is one adapter behind
a stable internal boundary: if the WhatsApp path dies, everything downstream —
transcription, search, correction, export — keeps working.

### Why the demo does not pair a live WhatsApp account

We were asked for a live demo, and we can give one for everything except the leg
that matters most to us. This is the honest accounting of that gap.

**What is live:** drop a voice note into the interface and watch the real thing
happen — ffmpeg conversion, whisper transcribing on the machine in the room with
no network, per-segment confidence, the suspect-line flagging, correction,
search, export. That is the whole pipeline, running, on real audio, in front of
you.

**What is not live: receiving a voice note from WhatsApp itself.** The pairing
code is built and it works. We did not want to run it on stage, and the reason is
the platform risk above rather than anything about the code. The only accounts we
could pair are our own personal numbers — the ones our families actually use — and
a ban does not cost the project, it costs a person their messaging account in a
country where WhatsApp *is* the phone network. We were not willing to bet a real
number on a demo, and we are not willing to imply we tested something we did not.

So the demo runs on `maktoob import`, the web upload form, and a seeded database.
Those exercise the identical pipeline; `internal/wa` only supplies the file at the
front of it.

**We would rather fix this than keep working around it, and suggestions are
genuinely welcome.** If you know a way to demonstrate the WhatsApp ingest path
without staking a real person's number on it — a burner number that reliably
survives whatsmeow, a WhatsApp Business API route that fits a local-only tool, a
protocol-level fake good enough to be honest about, or simply evidence that the
ban risk is smaller than we think — please
[open an issue](https://github.com/ahmdobeidat/maktoob/issues). This is the one
part of the project we could not show you honestly, and we would like that to
stop being true.

## Install

```sh
git clone https://github.com/ahmdobeidat/maktoob
cd maktoob
./scripts/setup.sh
```

The script checks dependencies, builds `whisper-server` from whisper.cpp,
downloads a model, and builds maktoob. It never runs `sudo` and never pairs an
account — it prints the package command for your system and stops.

Requires Go 1.25+, plus `ffmpeg`, `cmake`, `make` and `git`, and about 4 GB of
disk for the model and the build tree. Full detail, including model choice, troubleshooting and
uninstall, is in **[docs/INSTALL.md](docs/INSTALL.md)**.

## Usage

```sh
maktoob serve                  # read your notes in a browser (127.0.0.1:8765)
maktoob import <file>          # transcribe an audio file directly
maktoob list                   # show stored notes
maktoob show <id>              # show one transcript
maktoob export <id>            # write one transcript to stdout as JSON or Markdown
maktoob pair                   # link a WhatsApp account (prints a QR code)
maktoob logout                 # revoke the WhatsApp companion session
maktoob purge                  # delete every stored note, the session, and the salt
```

`maktoob serve` is the main interface: the note list, Arabic search, the audio
player, and inline correction. New notes appear live over SSE.

Everything except `pair` and `logout` works with no WhatsApp account at all.

## Extending it

maktoob is meant to be a component, not a silo.

- **HTTP API** — documented in **[docs/API.md](docs/API.md)**. Read the note
  list, search, fetch a single transcript, save a correction, upload a file.
- **Live event stream** — `GET /events` (Server-Sent Events) pushes each note as
  it arrives and again as it completes.
- **Export** — JSON and Markdown, from the interface, the API, or the CLI. The
  JSON export and the read API return the same document, so a client that reads
  one reads the other. Neither contains a filesystem path.

A demo run-sheet, with the fallbacks for when a live demo fails, is in
**[docs/DEMO.md](docs/DEMO.md)**. `go run ./scripts/seed` builds a demo database
so the interface can be shown without WhatsApp and without transcribing anything.

There are deliberately **no outbound webhooks**. A fire-and-forget webhook with
no signature and no retry is a weaker extensibility story than none at all; poll
the API or read the event stream.

Bulk export (`--all`) is not built. `maktoob export <id>` handles one note, and
the API will list them for you to iterate.

## Why not on the phone?

Running Whisper locally needs compute a phone will not give you at usable latency,
and companion pairing means your phone keeps working normally and untouched. maktoob
is the receiving-side layer for someone at a desk. A mobile client is future work.

## Licence

MIT — see [LICENSE](LICENSE). Third-party dependency licences, including
whatsmeow's MPL-2.0, are listed in [NOTICE](NOTICE).
