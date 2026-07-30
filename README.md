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
> **Not yet measured: word error rate on Levantine Arabic.** It is the number
> that decides whether a transcript here is a document or a draft, and it is
> stated as unmeasured rather than estimated. See *The transcript is a view onto
> the audio* below.

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

Word error rate on Levantine Arabic is **not measured yet**, and it is the
number that decides whether a transcript here is a document or a draft. It is
stated as missing rather than estimated.

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
