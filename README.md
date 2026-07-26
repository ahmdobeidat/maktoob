# maktoob

**Read your WhatsApp voice notes. On your own machine. Without sending them to anyone.**

maktoob links to WhatsApp as a companion device, transcribes incoming voice notes
locally with [whisper.cpp](https://github.com/ggml-org/whisper.cpp), and shows them
as readable, searchable, correctable text in your browser. No audio and no
transcript is sent to any third party.

> **Status: in active development** during the JOSA Reclaim Hackathon
> (18 July – 1 August 2026). This README describes what is built; features still
> in progress are marked as such in [the design spec](docs/superpowers/specs/2026-07-24-maktoob-design.md).

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

Whisper is measurably weaker on Levantine Arabic than on English or Modern Standard
Arabic. maktoob is designed around that rather than hiding it:

- Every segment carries a confidence score derived from the model's own token
  probabilities. Uncertain segments are visibly marked.
- Segments that look like **hallucinations** — Whisper's habit of inventing fluent
  text over silence — are marked separately, using the model's own no-speech
  probability. A fabrication with high token confidence is the most dangerous
  failure mode for someone who cannot check the audio, so it gets its own signal.
- The audio player sits next to the text. Clicking any line seeks to that moment.
- Any line can be corrected inline. The machine's original output is kept alongside
  your correction, never overwritten.

Measured accuracy and latency figures for this hardware will be published in this
README rather than described in adjectives.

## Privacy

- **Outbound connections, exhaustively:** WhatsApp and Meta's media CDN (where the
  audio already lives), and a one-time model download at install. Nothing else. No
  telemetry, no analytics, no transcription service.
- **What linking actually grants:** pairing gives maktoob a full multi-device
  session — all chats, all incoming message content, and your contact list, not just
  voice notes. maktoob discards the rest. That is a policy decision, not a technical
  boundary, and you should know the difference before you scan the QR code.
- **Other people's data:** the people who send you voice notes did not consent to
  this tool. maktoob can store a salted local alias instead of a phone number, and
  never persists view-once media.
- **Data at rest:** everything lives under `data/`. That directory contains your
  account's **identity keys** — anyone who copies it can impersonate your WhatsApp.
  It is created `0700`, the files inside it `0600`, and it is gitignored. Backing
  it up copies your credentials.
- **Unlinking is available today:** `maktoob logout` revokes the companion session
  and leaves your transcripts alone. Deleting stored data is `rm -rf data/`, which
  is the whole of it — there is no hidden state anywhere else on the machine. A
  `maktoob purge` command that does this for you is on the roadmap below and is
  **not built yet**; until it is, this bullet describes a directory, not a
  feature.
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

Requires Go 1.25+, `ffmpeg`, and a built `whisper-server` from whisper.cpp.

```sh
# system dependencies (Debian/Ubuntu)
sudo apt-get install -y cmake ffmpeg

# build maktoob
go build ./cmd/maktoob
```

Then build `whisper-server` and fetch a model per whisper.cpp's own
instructions, start it, and point maktoob at it with `-asr`.

A `scripts/setup.sh` that does the whisper.cpp build and model download in one
step, and a `docs/INSTALL.md` covering the CPU/GPU decision and expected
transcription latency, are still to be written. Neither exists yet, so nothing
above tells you to run them.

## Usage

Built and working today:

```sh
maktoob pair                   # link a WhatsApp account (prints a QR code)
maktoob import <file>          # transcribe an audio file directly
maktoob list                   # show stored notes
maktoob show <id>              # show one transcript
maktoob logout                 # revoke the WhatsApp companion session
```

Roadmap, not yet built — listed so the gap between the design and the code is
visible rather than implied:

```sh
maktoob serve                  # local web UI on 127.0.0.1:8080
maktoob export --all           # export everything as JSON or Markdown
maktoob purge                  # delete all stored media and transcripts
```

## Extending it

maktoob is meant to be a component, not a silo. This section describes the
intended shape of the web layer, which is **not built yet** — it ships with
`maktoob serve`:

- **HTTP API** — to be documented in `docs/API.md`.
- **Live event stream** — `GET /events` (Server-Sent Events) pushes each transcript
  as it completes.
- **Local hook** — `--on-transcript <command>` pipes the transcript JSON to any
  program on stdin. No network, no webhook signing problem, no retry semantics.
- **Export** — JSON and Markdown, per note or in bulk.

## Why not on the phone?

Running Whisper locally needs compute a phone will not give you at usable latency,
and companion pairing means your phone keeps working normally and untouched. maktoob
is the receiving-side layer for someone at a desk. A mobile client is future work.

## Licence

MIT — see [LICENSE](LICENSE). Third-party dependency licences, including
whatsmeow's MPL-2.0, are listed in [NOTICE](NOTICE).
