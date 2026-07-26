# internal/wa — WhatsApp Ingest Design

> Date: 2026-07-26
> Package: `internal/wa`
> Status: approved after adversarial review; supersedes the ingest sketch in
> `2026-07-24-maktoob-design.md` §4 and §10
> whatsmeow: pinned at `v0.0.0-20260421083005-5b8886176ff7`

## 1. Scope

`internal/wa` links to WhatsApp as a companion device, filters incoming voice
notes, downloads their audio, replaces every WhatsApp identifier with an
install-scoped alias, and hands the result to a sink. It does not transcribe, it
does not persist, and it does not know that either activity exists.

Out of scope for this package: sending messages, group management, contact sync,
history backfill, and media retry receipts.

## 2. Decisions

These were settled before design and are not revisited here, with one correction
noted in §2.1.

1. **Forward-only.** Only voice notes arriving after pairing are stored.
2. **All chats, groups included**, minus four mandatory drops: `status@broadcast`,
   `IsFromMe`, view-once, newsletters. Voice notes only — an `AudioMessage` with
   `PTT` false is an attachment, not a voice note, and is ignored.
3. **Every WhatsApp identifier is aliased** under a per-install key: sender JID,
   chat JID, and message ID. Push names are kept for display.
4. **`pair` is interactive and exits; `serve` runs web, ingest and the
   transcription worker in one process.**
5. **`internal/wa` imports nothing else of ours.** The adapter lives in
   `cmd/maktoob`.

### 2.1 Correction to decision 1

"Forward-only" describes what maktoob stores, not what it requests. whatsmeow's
default `DeviceProps.HistorySyncConfig` sets a 10 GB storage quota with no day
limits, so pairing requests a history sync payload whether or not we read it
(`store/clientpayload.go`).

Pairing therefore sets both limits to zero before generating the QR:

```go
store.DeviceProps.HistorySyncConfig.FullSyncDaysLimit = proto.Uint32(0)
store.DeviceProps.HistorySyncConfig.RecentSyncDaysLimit = proto.Uint32(0)
```

`events.HistorySync` is additionally ignored, so the policy holds even if the
request is honoured differently than expected. If for any reason the limits
cannot be set, PRIVACY.md must say "history sync is received and discarded"
rather than "no history sync". The two are not the same claim and only one of
them would survive a judge reading the pairing payload.

While setting device props, pairing also calls `store.SetOSInfo("maktoob", ...)`.
The default is the literal string `whatsmeow`, which is what would otherwise
appear in the user's Linked Devices list.

## 3. Boundary

`internal/wa` defines a one-method sink and depends on nothing downstream.
`cmd/maktoob` supplies the pipeline as a closure.

**`internal/pipeline` must never implement `wa.Sink`.** If it did, the dependency
arrow would reverse and deleting `internal/wa` would break the pipeline, the
import path and the web layer — worse coupling than a direct call, shipped under
a comment claiming the opposite. The adapter is a closure in the composition
root precisely so this cannot happen by accident.

The claim is mechanically checkable, and CI checks it:

```sh
go list -deps ./internal/wa      | grep maktoob/internal/ && exit 1
go list -deps ./internal/pipeline | grep maktoob/internal/wa && exit 1
```

A package that imports nothing of ours also cannot report anything to us. Every
outbound signal must therefore be an explicit field on `Listener`. There are
exactly two: `Sink` and `OnState`. Adding a third is a design change, not an
implementation detail.

## 4. Types

```go
package wa

// VoiceNote is one downloaded voice note with every WhatsApp identifier already
// reduced to an install-scoped alias. Nothing downstream can recover a phone
// number from this struct.
type VoiceNote struct {
    ChatAlias    string    // alias("chat", chat JID)
    ChatName     string    // group subject, or push name for a DM; may be empty
    SenderAlias  string    // alias("sender", sender JID)
    SenderName   string    // WhatsApp push name, display only
    DedupeKey    string    // alias("msg", chatJID + "\x00" + messageID)
    ReceivedAt   time.Time
    DurationHint int64     // AudioMessage.Seconds * 1000; 0 when absent
    Ext          string    // ".ogg"
    Audio        io.Reader // nil when the download failed; see DownloadErr
    DownloadErr  string    // non-empty means audio is unavailable, permanently
}

// Sink receives voice notes. It is deliberately the only channel through which
// internal/wa reaches the rest of maktoob.
//
// Nothing in internal/pipeline may implement this interface. The adapter lives
// in cmd/maktoob so the dependency arrow only ever points out of this package.
type Sink interface {
    Ingest(ctx context.Context, n VoiceNote) error
}

type SinkFunc func(ctx context.Context, n VoiceNote) error

func (f SinkFunc) Ingest(ctx context.Context, n VoiceNote) error { return f(ctx, n) }

// State is the connection state reported to the composition root. LoggedOut is
// terminal: the session is dead remotely and only re-pairing recovers it.
type State int

const (
    Disconnected State = iota
    Connected
    LoggedOut
)

// Downloader is the slice of *whatsmeow.Client that wa actually uses, declared
// here so tests can substitute a fake. *whatsmeow.Client satisfies it.
type Downloader interface {
    Download(ctx context.Context, msg whatsmeow.DownloadableMessage) ([]byte, error)
}

type Listener struct {
    Client  *whatsmeow.Client
    Down    Downloader
    Sink    Sink
    OnState func(State)
    Salt    []byte
    Log     *slog.Logger

    jobs   chan job
    cancel context.CancelFunc
    wg     sync.WaitGroup
}

// Start registers the event handler and starts the worker. It does not block.
func (l *Listener) Start(ctx context.Context) error

// Close cancels the worker's context and waits for in-flight work to finish.
func (l *Listener) Close() error
```

`whatsmeow.DownloadableMessage` is a four-method interface over direct path,
media key and two hashes, so the fake is trivial.

## 5. Aliasing

```
alias(kind, jid) = hex(HMAC-SHA256(salt, kind + "\x00" + canonical(jid)))
kind ∈ {"chat", "sender", "msg"}
canonical(jid) = jid.ToNonAD().String(), resolved to a single addressing mode
```

Three properties, each of which exists because its absence is a real bug:

**Device stripping.** For a DM, whatsmeow sets `Chat = from.ToNonAD()` but leaves
`Sender = from` with its device suffix intact. `JID.String()` emits
`user:device@server` whenever `Device > 0`, so the same contact writing from
their phone and from their linked desktop would otherwise produce two different
sender aliases, and every DM sender would eventually fork into two.

**Addressing mode.** WhatsApp is mid-migration from phone-number JIDs to `@lid`.
`MessageSource` carries `SenderAlt` holding the other form. Without resolving to
one mode, a chat that switches forks into a second `chats` row and the user sees
the same conversation twice with nothing to explain it.

The resolution rule is explicit: if the JID's server is `types.HiddenUserServer`
and the corresponding `Alt` field holds a JID on `types.DefaultUserServer`, alias
the `Alt` form; otherwise alias the JID as given. This normalises toward the
phone-number form, which is the one that stays stable across the migration.

**Domain separation.** For a DM, `Chat == Sender.ToNonAD()`, so without the
`kind` prefix the chat alias and sender alias would be byte-identical.

### Salt

32 random bytes at `data/salt`, mode `0600`, created once on first run.
`wa.LoadSalt(path)` owns it, which keeps the package dependency-free.

If the salt is lost, every alias changes: chats fork, old rows keep their display
names, and the interface shows each conversation twice with no explanation. That
failure is silent, which is the kind we do not ship.

The guard respects the boundary. `wa.LoadSalt` returns the salt and a
`Fingerprint() string` equal to `hex(HMAC-SHA256(salt, "maktoob-salt-check"))`.
`cmd/maktoob` compares that against a value stored in the store's `meta` table,
writing it on first run and refusing to start on mismatch with a message naming
the cause. `internal/wa` never touches the database.

## 6. Filter

Evaluated in `onEvent`, in this order, on every incoming message:

| test | drop when |
|---|---|
| message shape | no `AudioMessage` |
| voice note | `!audio.GetPTT()` |
| own message | `evt.Info.IsFromMe` |
| status | `evt.Info.Chat == types.StatusBroadcastJID` |
| newsletter | `evt.Info.Chat.Server == types.NewsletterServer` |
| view-once | `evt.IsViewOnce \|\| evt.IsViewOnceV2 \|\| evt.IsViewOnceV2Extension \|\| audio.GetViewOnce()` |

The view-once test checks four fields on purpose. `events.Message.IsViewOnce` is
set only when the message arrived inside a `ViewOnceMessage` wrapper, but a
view-once voice note from a current client sets `AudioMessage.ViewOnce` instead.
Checking only the event field lets the common case through, which would break a
mandatory drop.

`Ext` comes from a hard-coded three-entry mimetype map defaulting to `.ogg`.
Resolving through `mime.ExtensionsByType` depends on the host's `/etc/mime.types`
and can return `.oga`, which breaks browser playback.

## 7. Concurrency

```
whatsmeow handlerQueueLoop
  └─> onEvent(evt)                     returns in microseconds
        filter → alias → build job
        select { case jobs <- job: case <-ctx.Done(): }
  └─> worker goroutine
        Download            network, retries, seconds
        Sink.Ingest         one file write + one INSERT, milliseconds
```

whatsmeow processes one node at a time and tolerates a slow handler for
10 × 30 s before continuing it in the background (`client.go`, `handlerQueueLoop`).
`Download` is a CDN round trip with retries, so it cannot run on the handler
thread.

**The channel is 256 deep and the send blocks.** An earlier draft spawned a
goroutine on overflow to avoid dropping notes; that is unbounded concurrency, and
a busy group would produce N simultaneous downloads each buffering a whole file
into memory. A blocking send propagates backpressure into whatsmeow's own
bounded 2048-deep queue, which is built to absorb it.

**`jobs` is never closed.** Closing it would let a blocked sender panic on send.
Shutdown is by context: `Close` calls `cancel`, the worker returns, and the
`WaitGroup` makes `Close` wait for in-flight work. `Start` does not block.

**One worker is sufficient.** `Sink.Ingest` does not transcribe — it writes a
file and inserts a `pending` row. The queue that governs transcription already
exists in `notes.status`, drained by `ProcessNext`. The channel here exists for
exactly one reason: getting `Download` off whatsmeow's handler thread.

`SynchronousAck` stays at its default `false`. Setting it true would ack after
`onEvent` returns, which is still before the download, so it buys nothing and
couples handler latency to the network.

## 8. Error handling

### There is no redelivery

whatsmeow dispatches the ack and delivery receipt in a goroutine concurrently
with the event handler. By the time `onEvent` returns, the server has already
been told the message was delivered. **A note we fail to record is gone
permanently.** Every decision below follows from that.

### Download failure

whatsmeow retries internally. `DownloadMediaWithPath` returns immediately on
403, 404 and 410 without trying other hosts, which is CDN expiry — the only real
recovery is a media retry receipt, which is out of scope. So a download failure
here is usually permanent.

We record it. The sink receives a `VoiceNote` with `Audio == nil` and
`DownloadErr` set, and the adapter inserts a row with `status='failed'`, the
error text, and `media_path = ''`. The interface already renders failed notes as
a visible row with the error attached.

Dropping it instead would mean a deaf user is never told that a voice note
arrived at all. Spec §6 commits to "a failed transcription must never make the
voice note disappear"; a failed download is the same commitment.

`media_path TEXT NOT NULL` is satisfied by the empty string, so **no schema
change is needed** for this.

### Shutdown

On context cancel the listener stops accepting new events, then drains `jobs`
under a 5 second grace period, recording a metadata-only failed row for each
undelivered job. These are metadata structs whose downloads have not started, so
the drain is fast. Without it, everything queued at Ctrl-C is lost silently.

A handler blocked on a full channel when cancellation arrives takes the
`<-ctx.Done()` branch of the send. That job goes to the same drain path rather
than being discarded, so it is recorded as failed like any queued job. This is
the one place where the two shutdown mechanisms interact, and getting it wrong
reintroduces exactly the silent loss the drain exists to prevent.

An in-flight download is allowed to finish. If it cannot finish within the grace
period it is recorded as failed like any other.

### Other failures

| failure | behaviour |
|---|---|
| `Sink.Ingest` returns error | logged, listener continues; killing the connection over a disk error helps nobody |
| `io.Copy` fails mid-write | partial file removed, failed row recorded |
| `events.LoggedOut` | worker stops, `OnState(LoggedOut)` fires, `serve` shows a re-pair banner |
| `events.Disconnected` / `Connected` | `OnState` fires; whatsmeow auto-reconnects, nothing else to do |
| declared media length over 64 MB | rejected before download; the field is peer-supplied |

`LoggedOut` needs `OnState` specifically because decision 5 forbids this package
from recording anything itself. Without it, a session that dies mid-demo renders
a page that looks completely normal and simply stops updating.

## 9. Store changes

**`notes.sender_name TEXT`**, plus the field on `store.Note` and the three
statements that read or write it. `chats.display_name` cannot serve this: in a
group it holds the group subject, and we still need to know who spoke.

**A migration path.** `Open` executes a schema of `CREATE TABLE IF NOT EXISTS`
statements with no version table, so an existing database silently keeps the old
table and every statement naming the new column fails with `no such column`. A
fresh clone never reproduces it. `Open` gains an idempotent
`ALTER TABLE notes ADD COLUMN sender_name TEXT` that swallows the duplicate-column
error. Ten lines, and it covers every schema change still to come.

**`CreateNote` honours `n.Status`**, defaulting to `StatusPending`. It currently
hardcodes pending, so recording a failed row would take an insert followed by a
`SetStatus` — two statements, and a crash between them leaves a *pending* note
with an empty `media_path` that `ClaimNext` will happily claim and hand to ffmpeg
as an empty path. One atomic insert removes the window.

**`ClaimNext` gains `AND media_path <> ''`.** Belt and braces: a media-less row
can never be claimed regardless of how it reached pending. `RequeueStale` only
touches `converting` and `transcribing`, so failed rows correctly stay failed
across restarts.

**A `meta` table** holding the salt check from §5.

## 10. Pipeline changes

```go
type IngestRequest struct {
    Source       string    // "whatsapp" | "import"
    ChatID       string    // opaque; ImportChatID for uploads
    ChatName     string
    Sender       string    // opaque alias, never a JID
    SenderName   string
    WAMessageID  string    // opaque dedupe key; "" for imports
    ReceivedAt   time.Time
    Ext          string
    DurationHint int64     // used only when ffprobe fails
    Status       string    // "" means pending; "failed" for a failed download
    Error        string
}

// IngestReader writes r into the media directory and queues the note. A nil r
// records a media-less row, which is how a failed download is preserved.
//
// created reports whether a new note was inserted. False means the dedupe key
// was already present: the freshly written file is removed and id names the
// existing note.
func (p *Pipeline) IngestReader(ctx context.Context, r io.Reader, req IngestRequest) (id string, created bool, err error)
```

`Ingest(ctx, src)` remains as a thin wrapper filling in the import defaults, so
the CLI and its tests are untouched. `copyFile` becomes `writeMedia(dst, r)`,
keeping mode `0600` — the single enforcement point for the file-mode privacy
claim, and the reason audio crosses the boundary as an `io.Reader`. A `[]byte`
would force a second write site with its own mode argument, and `POST /import`
hands us a multipart reader anyway. The memory argument for `io.Reader` does not
apply: `Download` returns `[]byte`, so the file is already resident.

The `created` return also fixes a live bug. `pipeline.go` discards
`CreateNote`'s `ok` value, and `CreateNote` does `ON CONFLICT(wa_message_id) DO
NOTHING`. On a duplicate delivery today we write the audio, skip the insert, and
return a UUID with no row behind it: an orphan file and a caller-visible lie.
Dormant only because imports have `wa_message_id = NULL` and SQLite's UNIQUE
ignores NULLs. Removing the file on `created=false` is safe — it is a fresh UUID
path nothing else can reference, with no row for `ClaimNext` to find.

On duplicate, `IngestReader` returns the existing note's id rather than an empty
string, so a caller can link to it.

## 11. Adapter

The adapter is the code most likely to be wrong — dedupe handling, chat upsert,
failed-row construction, timestamp clamping — so it does not live inline in
`main`. It lives in `cmd/maktoob/wasink.go` with `wasink_test.go` beside it,
tested against a temp-file store. The boundary is preserved and the risky logic
is covered.

`ReceivedAt` is clamped to `min(evt.Info.Timestamp, now)`. The timestamp is
peer-supplied and drives both `ORDER BY received_at DESC` in `ListNotes` and
`ORDER BY received_at ASC` in `ClaimNext`, so a future-dated message would pin
itself to the top of the list permanently.

## 12. Privacy

Tracing every column of `maktoob.db`:

| column | contents | identifying |
|---|---|---|
| `chats.id` | alias, or `'import'` | no |
| `chats.display_name` | group subject or push name | **often** |
| `notes.sender` | alias | no |
| `notes.sender_name` | push name | **often** |
| `notes.wa_message_id` | aliased dedupe key | no |
| `notes.media_path` | `data/media/<uuid>.ogg` | no |
| `segments.asr_text` | the transcript | **unavoidably** |

Push names are user-chosen and in Jordan are very often a bare phone number, so
the two display columns do carry numbers. The transcripts carry far more.

The decisive limit is outside our database. whatsmeow writes every sender's raw
JID and push name into its own session store on every incoming message, writes
the full address book on app-state sync, and stores the account's own number and
identity keys in the same file. None of that is reachable from our code and none
of it is optional. `strings data/session.db | grep @s.whatsapp.net` produces a
list of phone numbers in about thirty seconds, and a judge who has used
whatsmeow will know to try it.

The salt defends against a *partial* leak — a backup of `maktoob.db` alone, a bug
that serves the DB over HTTP, a file attached to a support thread. That is a real
threat, and the defence is real: the phone-number space is trivially enumerable
against an unsalted hash and infeasible against a secret 32-byte key. Against
someone who copies all of `data/`, it buys nothing, because they take the salt
and the session store together.

PRIVACY.md must therefore say, in substance:

> maktoob's own database stores no WhatsApp identifiers. Chat, sender and message
> IDs are replaced by HMAC-SHA256 aliases under a per-install key, so a leaked
> copy of `maktoob.db` alone reveals no phone numbers.
>
> This is not full anonymisation and we will not claim it is. Display names come
> from WhatsApp push names, which are user-chosen and are sometimes phone
> numbers. Transcripts contain whatever was said. And the WhatsApp session store
> in the same directory, written by the whatsmeow library rather than by maktoob,
> holds your own number, your contacts' numbers and push names, and your
> account's identity keys in cleartext. Anyone who copies `data/` has the alias
> key alongside the raw identifiers, and can impersonate your WhatsApp account.
> `data/` is `0700`, gitignored, and must never be backed up anywhere you would
> not put your WhatsApp account itself.

The narrower claim is both honest and stronger than the overstated one, because
it survives being checked.

## 13. Tests

Every filter case is synthesisable with no connection and no fixture files:
`events.Message`, `types.MessageInfo` and `waE2E.AudioMessage` are plain structs
with exported fields, and `types.MessageID` is a string alias.

| test | asserts |
|---|---|
| filter table, 9 cases | PTT from contact and PTT in group accepted; status broadcast, newsletter, IsFromMe, wrapper view-once, `AudioMessage.ViewOnce`, non-PTT audio and text all dropped |
| `TestHandlerDoesNotBlock` | fake `Downloader` sleeps 2 s; `onEvent` returns in under 10 ms |
| `TestAliasIsStableAndOpaque` | same JID and salt give the same alias; a device suffix does not change it; a different salt changes it; the phone number does not appear in the output |
| `TestAliasDomainSeparation` | for a DM, chat alias and sender alias differ |
| `TestDedupeKeyIsChatScoped` | the same message ID in two chats yields two keys |
| `TestSinkErrorDoesNotKillListener` | sink errors, the next note still arrives |
| `TestLoggedOutStopsListener` | `OnState(LoggedOut)` fires and the worker stops |
| `TestShutdownDrainsQueue` | jobs queued at cancel are recorded, not lost |
| `TestIngestReaderDuplicate` | second call with the same dedupe key returns `created=false`, the existing id, and leaves no orphan file |
| `TestIngestReaderNilAudio` | records a failed row with empty `media_path` that `ClaimNext` never claims |
| `wasink_test.go` | chat upsert, timestamp clamping, failed-row construction |

`TestHandlerDoesNotBlock` is the one worth pointing a reviewer at: it proves the
300-second stall is designed against rather than hoped away.

The two `go list -deps` guards run in CI beside the existing gofmt, vet and race
checks.

## 14. Deferred

**Group subject lookup.** `ChatName` for a group is not carried on the event; it
needs `Client.GetGroupInfo`, a network call that must never run in `onEvent`. We
ship with an empty `ChatName` for groups and let `UpsertChat`'s existing
`COALESCE(NULLIF(...))` fill it in whenever a group event supplies one. Adding
the explicit fetch is a D5 item if there is slack. It also pulls the full
participant list into memory, which PRIVACY.md would need to mention.

**Media retry receipts.** The only recovery for CDN-expired media. Out of scope.

## 15. Open item

`ClaimNext` orders `received_at ASC`. During a backlog — and at 13 to 15 seconds
per note on a single worker, a group chat produces one easily — the note the user
is sitting there waiting for is transcribed last. For a tool whose value is
"read what was just said to you", that is a real inversion. Switching to `DESC`
is a one-word change that starves old notes during a sustained flood instead.

Ahmad's call, and it does not block implementation: the default is to leave
`ASC` as it stands, since changing it is one word whenever he decides.

Either way, the queue depth belongs in the interface so the user knows the app is
working rather than broken.
