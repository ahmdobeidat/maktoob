# maktoob HTTP API

`maktoob serve` exposes a small read-mostly HTTP API alongside the web
interface. It exists so maktoob can be a component in something else rather than
a place your transcripts get stuck.

Everything below is served by the same process as the web pages, on the same
address, and is available the moment `maktoob serve` is running.

## Where it listens

Loopback only, by default:

```
http://127.0.0.1:8765
```

`-addr` changes it. **Think before you do.** There is no authentication and no
account model, because maktoob is one person's transcripts on one person's
machine. Binding to a reachable interface publishes private messages to whoever
can reach that address.

## Authentication

None. See above.

The two routes that change data — `PATCH /api/segments/{id}` and
`POST /import` — verify that the request came from this interface, using
`Sec-Fetch-Site` and falling back to `Origin`. This stops a page you have open
in another tab from writing to your transcripts: it cannot *read* a localhost
response, but without the check it could still *send* a request.

Requests carrying neither header are allowed through, so `curl` and scripts
work. No browser reaches these routes without sending at least one of them.

## Endpoints

| Method | Path | Purpose |
|---|---|---|
| `GET` | `/` | Note list (HTML), with search and chat filter |
| `GET` | `/note/{id}` | One transcript (HTML) |
| `GET` | `/events` | Server-sent events: arrivals and status changes |
| `GET` | `/api/notes` | Note list, or search results, as JSON |
| `GET` | `/api/notes/{id}` | One note with its segments, as JSON |
| `GET` | `/api/notes/{id}/audio` | The original audio, with `Range` support |
| `GET` | `/api/notes/{id}/export` | One note as a JSON or Markdown download |
| `PATCH` | `/api/segments/{id}` | Save a correction to one line |
| `POST` | `/import` | Upload an audio file for transcription |

---

### `GET /api/notes`

Query parameters, all optional:

| Name | Meaning |
|---|---|
| `q` | Full-text search across transcripts. Arabic spelling variants are normalised, so hamza and teh marbuta do not have to match exactly. |
| `chat` | Restrict to one chat id, as returned in `chat_id`. |
| `limit` | Maximum results. Defaults to 100, capped at 1000. |

Without `q`, returns notes newest first:

```json
{
  "notes": [
    {
      "id": "8f2c1a...",
      "chat": "Umm Ahmad",
      "chat_id": "b31f...",
      "sender": "Ahmad",
      "source": "whatsapp",
      "received_at": "2026-07-30T12:04:11+03:00",
      "duration_ms": 12400,
      "status": "done"
    }
  ]
}
```

With `q`, returns one group per note, carrying the lines that matched. A note
with three matching lines is one result, not three:

```json
{
  "query": "...",
  "results": [
    {
      "note": { "id": "8f2c1a...", "chat": "Umm Ahmad", "status": "done" },
      "matches": [
        {
          "id": 42,
          "idx": 1,
          "start_ms": 4000,
          "timecode": "0:04",
          "text": "...",
          "edited": false,
          "suspect": false,
          "low_confidence": true,
          "confidence": 22,
          "markers": ["low confidence"]
        }
      ]
    }
  ]
}
```

`status` is one of `pending`, `converting`, `transcribing`, `done`, `failed`,
`no_speech`. `no_speech` is a success: voice activity detection ran and found
nothing to transcribe.

### `GET /api/notes/{id}`

Returns the same document as `GET /api/notes/{id}/export?format=json`, without
the download headers. One shape for both means a client that can read an export
can read the API.

```json
{
  "id": "8f2c1a...",
  "source": "whatsapp",
  "chat": "Umm Ahmad",
  "sender": "Ahmad",
  "received_at": "2026-07-30T12:04:11+03:00",
  "duration_ms": 12400,
  "status": "done",
  "model": "large-v3-turbo",
  "text": "the whole transcript as one block",
  "segments": [
    {
      "idx": 0,
      "start_ms": 0,
      "end_ms": 4000,
      "text": "what to display",
      "asr_text": "what the machine produced",
      "edited": true,
      "edited_at": "2026-07-30T12:09:00+03:00",
      "confidence": 0.95,
      "no_speech_prob": 0.01,
      "suspect": false
    }
  ]
}
```

`text` is the corrected line where a correction exists, and the machine output
otherwise. `asr_text` is always the machine output. A correction never
overwrites what Whisper produced.

**No filesystem paths appear in any response.** An export is a file you might
send to someone, and where the audio sits on your disk is not part of the
transcript.

#### Reading confidence honestly

`confidence` is `exp(avg_logprob)` — the model's own mean token probability for
that segment. It is a measure of how sure the model was, and **not** a measure
of whether the text is right. Whisper transcribes confidently into whatever
language it was told to expect: point it at English audio with the language
pinned to Arabic and it will produce fluent nonsense at 0.92–0.98 confidence.

`suspect` is a separate and more serious signal. It marks likely fabrication —
Whisper inventing fluent text over silence — using the model's no-speech
probability. A fabrication usually carries *high* token confidence, which is
exactly what makes it dangerous for a reader who cannot check the audio.
Treat `suspect` as the stronger warning.

### `GET /api/notes/{id}/audio`

Serves the original file as received — ogg/opus from WhatsApp — not the 16 kHz
mono wav the transcriber uses. The wav is a downsampled artifact and sounds
worse.

Supports `Range`, which browsers need in order to seek inside a long note.

The path is resolved from the note row by id. No part of it comes from the
request.

### `GET /api/notes/{id}/export`

| Parameter | Values | Default |
|---|---|---|
| `format` | `json`, `md` | `json` |

Returns `Content-Disposition: attachment` with a filename derived from the note
id. Unknown formats are a `400`.

The Markdown export renders confidence, suspicion and human edits as words, the
same way the interface and the terminal do.

### `PATCH /api/segments/{id}`

```sh
curl -X PATCH http://127.0.0.1:8765/api/segments/42 \
  -H 'Content-Type: application/json' \
  -d '{"text":"the corrected line"}'
```

Returns the segment as stored, including the `edited_at` timestamp the client
did not send. Read the response rather than assuming the write took: it is the
row, not an echo.

An empty `text` clears the correction and restores the machine output.

### `POST /import`

`multipart/form-data` with the file in a field named `audio`.

```sh
curl -X POST http://127.0.0.1:8765/import \
  -H 'Accept: application/json' \
  -F 'audio=@note.ogg'
```

Returns `202` with `{"id": "...", "created": true}` when `Accept` includes
`application/json`, and otherwise redirects to the new note's page, so the form
works without JavaScript.

`created: false` means the file was already stored — imports are deduplicated.

Uploads are capped at 64 MiB. The file is copied into your data directory and
transcribed locally; it is not uploaded anywhere.

### `GET /events`

A Server-Sent Events stream. Two event types:

```
event: arrived
data: {"kind":"arrived","note_id":"8f2c1a...","status":"pending","text":"queued"}

event: updated
data: {"kind":"updated","note_id":"8f2c1a...","status":"done","text":"transcribed"}
```

`arrived` fires when a note is accepted and queued. `updated` fires when it
reaches a terminal state. A comment line (`: ping`) is sent every 25 seconds to
keep intermediaries from dropping an idle stream.

**Events can be dropped.** The stream is a notification channel, not a
transaction log. A client that has stopped reading loses events rather than
stalling transcription, because a wedged browser tab must never be able to hold
up the pipeline. Re-fetch on reconnect; do not treat the stream as the source of
truth.

## What there deliberately isn't

- **No outbound webhooks.** A fire-and-forget webhook with no signature and no
  retry is a weaker extensibility story than none. Poll `/api/notes` or read
  `/events`.
- **No write path back to WhatsApp.** maktoob never sends, replies, or marks
  anything read.
- **No bulk delete over HTTP.** Deleting everything is `maktoob purge`, which
  makes you read what it will remove and type `yes`.

## Errors

Errors are JSON on `/api/*` routes and plain text on HTML routes.

| Status | Meaning |
|---|---|
| `400` | Malformed id, body, or format |
| `403` | The request did not come from this interface |
| `404` | No such note or segment, or the audio is not on disk |
| `503` | Import was requested but transcription is not wired up |

Internal failures return a generic message. The detail goes to the server log,
not to the response, because the detail is filesystem paths.
