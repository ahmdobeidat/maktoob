# Demo run-sheet

A written order of operations for showing maktoob, with the decision points
marked. The point of writing it down is that a live demo fails in ways you
cannot improvise around, and the fallbacks below are not improvisations.

## Before you start

Everything here is `-data demo-data` so a demo never touches your real
transcripts, and never risks showing a real message on a projector.

```sh
# 1. Build the demo database. Requires scripts/seed/fixture.json.
go run ./scripts/seed -data demo-data -force

# 2. Transcription backend, in its own terminal.
vendor-build/whisper.cpp/build/bin/whisper-server \
    --model models/ggml-large-v3-turbo.bin --host 127.0.0.1 --port 8642

# 3. maktoob, in another.
./maktoob serve -data demo-data
```

Open <http://127.0.0.1:8765>.

**Have one real audio file ready** at a path you can type from memory. It is
what makes the live transcription step work, and it is the fallback for everything
else.

### Check before you present

- [ ] `scripts/seed/fixture.json` exists and its text is Arabic, not the English
      placeholder from `fixture.example.json`
- [ ] The list page shows notes, including one low-confidence and one suspect line
- [ ] `whisper-server` answers — import one file and watch it complete
- [ ] Browser zoom at 100%, dark mode decided in advance (the interface supports
      both; switching mid-demo wastes a beat)
- [ ] **Do not run `maktoob pair` on stage.** Pair beforehand or not at all.

## The order

**1. The problem, on the list page.** Notes arrive as opaque audio. Here they are
as text, newest first, with who sent them and how long they are.

**2. Open a note.** The player is pinned at the top; the transcript is beneath in
Arabic, right-to-left. Click any line to seek the audio to that moment.

**3. The honesty story — this is the part that distinguishes the project.**
Point at a line marked *low confidence* and one marked *possible fabrication*.
Say plainly: this is machine transcription, it is weaker on Levantine Arabic than
on English, and the interface tells you where not to trust it. The warnings are
words, not colours, because a colour is not available to every reader.

**4. Correct a line.** Click *Correct this line*, edit, save. Note that the
machine's original is kept, not overwritten — the export carries both.

**5. Search.** Search for a word that appears with a different spelling than you
typed. It matches anyway: hamza and teh marbuta are normalised. This is the
feature that turns a pile of voice notes into something you can look things up in.

**6. Export.** JSON and Markdown. Open the Markdown so it is visibly a plain,
readable document that leaves with the user.

**7. Privacy, in one breath.** Nothing left the machine. Open devtools' network
tab if there is time — the Content-Security-Policy forbids every remote origin,
so the claim is checkable rather than asserted.

**8. Live arrival, if paired.** Send a voice note from a phone; it appears in the
list on its own over SSE, then fills in when transcription finishes.

## Decision points

**If `whisper-server` is not answering.** Do not debug on stage. The seeded
database has finished transcripts in it — the whole demo above works except
step 8 and the live part of step 2. Say the backend is a separate process and
move on.

**If pairing is dead or the account is banned.** Use `maktoob import` or the
upload form at the bottom of the list page. This is exactly why the file path
exists, and saying so out loud is a stronger answer than the demo you lost:
`internal/wa` is one adapter behind a boundary that CI enforces, and everything
downstream survives its removal.

**If the network at the venue is hostile.** Nothing here needs the network
except WhatsApp. Skip step 8.

**If you are running long.** Cut steps 6 and 8. Do not cut step 3 — it is the
thing that separates this from a wrapper around Whisper.

## Questions worth pre-loading

**"What is the word error rate?"**
Not measured yet, and say so. Naming an unmeasured number is the one thing that
loses a technical audience permanently. What you can say: every segment carries
the model's own confidence, likely fabrications are flagged separately from
low confidence, the audio sits next to the text, and any line can be corrected —
the design assumes the transcript is a draft.

**"Isn't this against WhatsApp's terms?"**
Yes, and it is written in the README and in PRIVACY.md rather than left to be
found. whatsmeow is an unofficial client, accounts using it can be banned, and
the architecture keeps it as one replaceable adapter for that reason.

**"What does pairing actually give it?"**
A full multi-device session — every chat, every incoming message, the contact
list. maktoob keeps voice notes and discards the rest, and that is a policy
rather than a capability boundary. The filter is one 26-line function.

**"Why not just use WhatsApp's own transcription?"**
Unavailable on most devices in the region, closed, and it makes decisions about
private audio nobody outside Meta can inspect.

**"Does the sender know?"**
They see the note marked delivered, because a linked device acknowledges
messages at the protocol level. maktoob never marks anything read and never
sends. This is in PRIVACY.md.

## After

```sh
rm -rf demo-data
```
