# Installing maktoob

## The short path

```sh
git clone https://github.com/ahmdobeidat/maktoob
cd maktoob
./scripts/setup.sh
```

The script checks your dependencies, builds `whisper-server` from whisper.cpp,
downloads a model, and builds the maktoob binary. It prints the commands to run
when it finishes.

It never runs `sudo`. If a system package is missing it tells you the command
for your package manager and stops, so you can read it before trusting it. It
also never pairs a WhatsApp account — linking a real account is a deliberate
step, not a side effect of installing.

Re-running it is safe. An existing whisper.cpp checkout is left exactly as it
is, and an already-downloaded model is not fetched again.

## What you need

| Requirement | Why |
|---|---|
| **Go 1.25+** | Builds maktoob |
| **ffmpeg** | Converts every note to 16 kHz mono wav. Needed at runtime, not just at build |
| **cmake**, **make**, **git** | Build whisper.cpp |
| **~4 GB disk** | 1.6 GB model, plus the whisper.cpp build tree |
| **~2 GB RAM free** | `large-v3-turbo` inference |

Debian/Ubuntu:

```sh
sudo apt-get install -y git cmake build-essential ffmpeg
```

Go itself is not in most distributions at a new enough version. Get it from
<https://go.dev/dl/>.

## Running it

Two processes. The transcription backend:

```sh
vendor-build/whisper.cpp/build/bin/whisper-server \
    --model models/ggml-large-v3-turbo.bin \
    --host 127.0.0.1 --port 8642
```

And maktoob:

```sh
./maktoob serve
```

Then open <http://127.0.0.1:8765>.

You can transcribe a file immediately, without WhatsApp:

```sh
./maktoob import path/to/voice-note.ogg
```

or drop it into the upload form at the bottom of the web page.

## Linking WhatsApp

```sh
./maktoob pair
```

This prints a QR code. Scan it from your phone: **WhatsApp → Settings → Linked
Devices → Link a Device**.

**Read [PRIVACY.md](PRIVACY.md) before you do this.** Pairing grants a full
multi-device session — every chat, every incoming message, your contact list —
not only voice notes. maktoob discards the rest as a matter of policy, not
capability. It also uses an unofficial client, which is against WhatsApp's terms
and can get an account banned.

To unlink:

```sh
./maktoob logout
```

Transcripts already on your machine are untouched.

## Choosing a model

Set `MODEL` before running setup:

```sh
MODEL=small ./scripts/setup.sh
```

| Model | Size | Notes |
|---|---|---|
| `large-v3-turbo` | ~1.6 GB | The default |
| `small` | ~466 MB | Smaller and faster. Reasonable if disk or RAM is tight |
| `tiny` | ~75 MB | For checking the pipeline runs end to end |

**Accuracy on Levantine Arabic is not yet measured for this project, for any of
these models.** The table above deliberately makes no accuracy claim: ranking
them without a bake-off would be inventing the number this paragraph admits is
missing. Whisper is known to be weaker on Levantine than on English or Modern
Standard Arabic.

Until a word error rate is published, treat every transcript as a draft to check
against the audio rather than a record of what was said. The interface is built
around that assumption: every line carries the model's own confidence, and lines
that look fabricated are marked separately.

## CPU or GPU

The default build is CPU-only, which is the right choice for this workload.
Notes are seconds long and arrive one at a time, so throughput is not the
constraint and a GPU build adds a large dependency for a benefit you will not
notice.

If you want one anyway, build whisper.cpp yourself with the relevant flag
(`-DGGML_CUDA=ON`, `-DGGML_METAL=ON`, `-DGGML_VULKAN=ON`) and point maktoob at
the server with `-asr`. maktoob talks to `whisper-server` over HTTP and does not
care how it was compiled.

## What it costs to run

Measured on the development machine, CPU-only, `large-v3-turbo`, on short
Levantine voice notes:

- **Latency: roughly 13–15 seconds per note**, and roughly flat across the note
  lengths tested rather than proportional to duration.

That is a handful of notes on one machine, not a benchmark. It is here because
"fast" is not a measurement. Word error rate is the number that matters most and
it is **not yet measured** — see above.

## Common problems

**`connection refused` on import or in the interface.**
`whisper-server` is not running, or is on a different port. maktoob defaults to
`http://127.0.0.1:8642`; change it with `-asr`.

**`ffmpeg: executable file not found`.**
Install ffmpeg. It is needed every time a note is transcribed, not only at
build.

**"the alias salt does not match this database".**
`data/salt` was replaced or lost. Aliases are derived from it, so a swapped salt
would silently fork every chat into a duplicate. Restore the original `data/`
directory, or start a new one — maktoob refuses to run rather than quietly
corrupt the mapping.

**A transcript is fluent but completely wrong.**
Check `-lang`. It is pinned to `ar` by default rather than auto-detected,
because detection runs on the first 30 seconds and is unreliable on short notes.
Pointed at the wrong language, Whisper produces confident nonsense — and neither
the confidence score nor the no-speech probability catches it. Use `-lang auto`
for mixed-language audio, knowing detection may still be wrong.

**Transcription is much slower than 13–15 seconds.**
Check that whisper-server is not falling back to a single thread, and that you
have not loaded a larger model than you meant to.

## Uninstalling

```sh
./maktoob logout          # unlink from WhatsApp first
./maktoob purge           # delete media, transcripts, session, and salt
rm -rf vendor-build models maktoob
```

`purge` shows you exactly what it will delete and makes you type `yes`. Note
that it does **not** unlink your device from WhatsApp — that is what `logout` is
for, and doing it in the other order leaves a live linked device on your
account.
