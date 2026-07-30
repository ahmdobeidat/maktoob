# Privacy

This document describes what maktoob does with your data, and what it cannot
protect you from. Precision matters more here than a strong claim: a
privacy-first project caught overstating its own privacy is worse off than one
that never claimed anything.

If you find something in this document that is not true of the code, that is a
bug. Please report it.

## The short version

Your audio and your transcripts stay on your machine. Nothing is sent to a
transcription service, and there is no telemetry of any kind.

The two things you should understand before pairing a WhatsApp account are in
**What linking actually grants** and **Platform risk**, below. Neither is a
detail.

## Outbound connections, exhaustively

While maktoob is running, it connects to:

1. **WhatsApp's servers**, to maintain the companion-device session — only if
   you have paired an account.
2. **Meta's media CDN**, to download the audio of a voice note. That audio is
   already on Meta's servers; this is the same fetch your phone makes.

And once, at install time:

3. **The Whisper model download**, from Hugging Face, run by `scripts/setup.sh`.

That is the complete list. There is no analytics endpoint, no crash reporter, no
update check, and no third-party transcription API. The web interface sends a
`Content-Security-Policy` that forbids every remote origin, so you can confirm
in your browser's network tab that the page loads nothing from anywhere.

If you never pair a WhatsApp account and use `maktoob import` on files, maktoob
makes no network connections at all after installation.

## Transcription is local

Audio is converted by `ffmpeg` on your machine and transcribed by
`whisper-server` on your machine, over loopback. The model runs on your CPU or
GPU. No audio leaves the machine at any point in that pipeline.

## What linking actually grants

**Pairing gives maktoob a full WhatsApp multi-device session.** That means:

- every chat, not only the ones you care about
- the content of every incoming message, not only voice notes
- your contact list
- a history sync payload when you first link

maktoob keeps incoming voice notes and discards everything else. **That is a
policy, not a capability boundary.** The session could read all of it; the code
chooses not to. You are trusting the code, and the code is there for you to
read — `internal/wa/filter.go` is the whole of it, and it is forty lines.

To be exact about what it keeps and drops. It keeps push-to-talk audio messages
from other people, in every chat — there is no per-chat allowlist, so pairing
means every voice note anyone sends you gets transcribed. It drops:

- attached audio files that are not voice notes
- your own voice notes
- Status broadcasts and newsletter channels
- **view-once messages**, checked four different ways, because the sender chose
  ephemerality and turning that into a permanent searchable row would break it
  on their behalf without their knowledge

If that trust is not one you want to extend, use `maktoob import` and never
pair. Every feature except WhatsApp ingest works without it.

## Other people's data

The people who send you voice notes did not agree to this.

maktoob stores a **salted local alias** for each chat and sender rather than a
phone number. The salt lives in `data/salt` and never leaves your machine, so
the stored identifiers cannot be linked back to phone numbers by anyone who
obtains only the database.

The sender's *display name* is stored as-is, because a transcript with no
indication of who spoke is not usable.

This reduces the harm of a leaked database. It does not make storing other
people's voice notes consensual. Use judgement.

## Data at rest

Everything maktoob stores lives under one directory, `data/` by default:

| Path | Contents |
|---|---|
| `data/media/` | The original audio, exactly as received |
| `data/maktoob.db` | Transcripts, corrections, and the search index |
| `data/session.db` | The WhatsApp session |
| `data/salt` | The key your contact aliases are derived from |

**None of it is encrypted.** maktoob relies on your operating system's file
permissions. The directory is created `0700` and the files `0600`, so other
users on the same machine cannot read them, but anyone with your login — or your
backups — can.

`data/session.db` contains your WhatsApp account's **identity keys**. Anyone who
copies that file can impersonate your WhatsApp account. Backing up `data/` backs
up your credentials. It is gitignored so it cannot be committed by accident, but
nothing stops a general-purpose backup tool from sweeping it up.

## Deleting your data

```sh
maktoob purge
```

This lists exactly what it will delete, warns you that it is permanent, and
requires you to type `yes`. It removes the media, the database, the database's
write-ahead log, the session, and the salt.

The write-ahead log matters: SQLite in WAL mode keeps recent commits in a
sidecar file, so deleting only the main database can leave transcripts
recoverable. `purge` removes both.

`purge` only deletes files maktoob created. If you pointed `-data` at a
directory holding other things, those are left alone.

**`purge` does not unlink your device from WhatsApp.** Deleting the local
session tells WhatsApp nothing; the linked device stays on your account until it
is removed. To actually unlink:

```sh
maktoob logout      # before purging
```

or remove it from Linked Devices on your phone.

There is no hidden state anywhere else on the machine. No files in your home
directory, no system services, no registry entries.

## Logging

maktoob's own logs contain note ids and aliases, never phone numbers.

`-verbose` is different. It forwards whatsmeow's diagnostics to stderr, and
**those contain your contacts' phone numbers in the clear** — whatsmeow logs
un-aliased identifiers at warning and error level during ordinary operation. Do
not use `-verbose` where stderr is redirected to a file or captured by a service
manager, because that writes phone numbers to a file outside `data/` and defeats
the aliasing.

It is off by default and the help text says this.

## The web interface

`maktoob serve` binds to `127.0.0.1` and has **no authentication**, because
there is no account model to authenticate against.

`-addr` can change the bind address. If you point it at an interface other than
loopback, you are publishing your private messages to everyone who can reach
that address. There is no warning dialog; there is this paragraph.

The two routes that modify data check that the request came from the interface
itself, so a page open in another browser tab cannot write to your transcripts.

## Platform risk

maktoob talks to WhatsApp through [whatsmeow](https://github.com/tulir/whatsmeow),
an **unofficial, reverse-engineered client**.

- Using it is **against WhatsApp's terms of service**.
- Accounts using unofficial clients **can be banned**.
- Meta can change the protocol at any time and break it without notice.

Do not pair an account you cannot afford to lose.

This risk is why the architecture keeps WhatsApp at arm's length. `internal/wa`
is one adapter behind a stable internal boundary, and CI enforces that the rest
of the program does not import it. If the WhatsApp path dies tomorrow,
transcription, search, correction, and export keep working through
`maktoob import` and the web upload form.

## What maktoob never does

- Send, reply to, forward, or mark as read any WhatsApp message.
- Store view-once media.
- Upload audio or transcripts anywhere.
- Summarise your messages with a language model, locally or otherwise.
- Phone home, check for updates, or report errors.
