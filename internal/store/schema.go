package store

// Processing states for a voice note.
//
// The queue is this column rather than an in-memory channel, so that a crash
// mid-transcription costs one retry instead of a lost note. RequeueStale resets
// the two transient states on startup.
const (
	StatusPending      = "pending"
	StatusConverting   = "converting"
	StatusTranscribing = "transcribing"
	StatusDone         = "done"
	StatusFailed       = "failed"

	// StatusNoSpeech is a success, not a failure: whisper returned no segments
	// at all, which is what a note of pure noise or silence looks like. Without
	// a distinct state this case either masquerades as a failure or produces an
	// empty transcript that looks like a bug. It is reachable on any note that
	// is pure noise, which on a real account happens within the first day.
	StatusNoSpeech = "no_speech"
)

// MaxAttempts bounds retries. Whisper's repetition-loop failure mode on
// degenerate audio can spin far past real time, and without a cap the startup
// requeue turns one bad note into a permanent poison pill that starves the
// queue.
const MaxAttempts = 2

const schema = `
CREATE TABLE IF NOT EXISTS chats (
  id            TEXT PRIMARY KEY,
  display_name  TEXT,
  created_at    INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS notes (
  id            TEXT PRIMARY KEY,
  chat_id       TEXT NOT NULL REFERENCES chats(id) ON DELETE CASCADE,
  source        TEXT NOT NULL,
  sender        TEXT,
  sender_name   TEXT,
  wa_message_id TEXT UNIQUE,
  media_path    TEXT NOT NULL,
  wav_path      TEXT,
  duration_ms   INTEGER NOT NULL DEFAULT 0,
  received_at   INTEGER NOT NULL,
  status        TEXT NOT NULL,
  attempts      INTEGER NOT NULL DEFAULT 0,
  error         TEXT,
  model         TEXT
);

CREATE INDEX IF NOT EXISTS idx_notes_received ON notes(received_at DESC);
CREATE INDEX IF NOT EXISTS idx_notes_status   ON notes(status);

CREATE TABLE IF NOT EXISTS segments (
  id             INTEGER PRIMARY KEY,
  note_id        TEXT NOT NULL REFERENCES notes(id) ON DELETE CASCADE,
  idx            INTEGER NOT NULL,
  start_ms       INTEGER NOT NULL,
  end_ms         INTEGER NOT NULL,
  asr_text       TEXT NOT NULL,
  edited_text    TEXT,
  avg_logprob    REAL,
  no_speech_prob REAL,
  mean_word_p    REAL,
  min_word_p     REAL,
  temperature    REAL,
  suspect        INTEGER NOT NULL DEFAULT 0,
  edited_at      INTEGER,
  UNIQUE(note_id, idx)
);

CREATE INDEX IF NOT EXISTS idx_segments_note ON segments(note_id, idx);

-- The FTS row's rowid is segments.id, so updates and deletes are point lookups
-- rather than full scans of an unindexed column.
--
-- This is an ordinary (not contentless, not external-content) FTS5 table. It
-- stores its own copy of the normalised text, which costs a little space and
-- buys the ability to DELETE a row. Both corrections and retranscription
-- rewrite index rows, and a contentless table rejects DELETE outright.
-- External content is not usable either, because the indexed text is the
-- Arabic-normalised form rather than any column that exists in segments.
CREATE VIRTUAL TABLE IF NOT EXISTS segments_fts USING fts5(text);

-- meta holds small install-scoped values that are not per-note: currently the
-- alias salt fingerprint, which detects a lost or swapped salt before every
-- chat in the database silently forks into a new one.
CREATE TABLE IF NOT EXISTS meta (
  key   TEXT PRIMARY KEY,
  value TEXT NOT NULL
);
`
