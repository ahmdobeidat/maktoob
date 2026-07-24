package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

const (
	// "ahmad" with and without the hamza above the initial alef, and "school"
	// with teh marbuta versus plain heh. Written as escapes because bidi glyphs
	// reorder source visually and make a failing assertion unreadable.
	ahmadHamza    = "\u0623\u062D\u0645\u062F"
	ahmadBare     = "\u0627\u062D\u0645\u062F"
	schoolMarbuta = "\u0645\u062F\u0631\u0633\u0629"
	schoolHeh     = "\u0645\u062F\u0631\u0633\u0647"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func seedNote(t *testing.T, s *Store, id string) Note {
	t.Helper()
	ctx := context.Background()
	if err := s.UpsertChat(ctx, "chat-1", "Test Chat"); err != nil {
		t.Fatalf("upsert chat: %v", err)
	}
	n := Note{
		ID:         id,
		ChatID:     "chat-1",
		Source:     "import",
		MediaPath:  "/tmp/" + id + ".ogg",
		DurationMS: 5000,
		ReceivedAt: time.Now(),
	}
	ok, err := s.CreateNote(ctx, n)
	if err != nil {
		t.Fatalf("create note: %v", err)
	}
	if !ok {
		t.Fatalf("create note %q reported duplicate on first insert", id)
	}
	return n
}

// TestForeignKeysEnforced guards the DSN pragma. SQLite defaults foreign_keys
// to OFF, which would silently turn every ON DELETE CASCADE in the schema into
// a no-op. The failure is invisible until orphaned rows accumulate, so it needs
// an explicit test rather than trust in the connection string.
func TestForeignKeysEnforced(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	var on int
	if err := s.db.QueryRowContext(ctx, "PRAGMA foreign_keys").Scan(&on); err != nil {
		t.Fatalf("read pragma: %v", err)
	}
	if on != 1 {
		t.Fatalf("foreign_keys = %d, want 1: ON DELETE CASCADE is a no-op", on)
	}

	seedNote(t, s, "note-1")
	if err := s.ReplaceSegments(ctx, "note-1", []Segment{
		{Idx: 0, StartMS: 0, EndMS: 1000, ASRText: "hello"},
	}); err != nil {
		t.Fatalf("replace segments: %v", err)
	}

	if _, err := s.db.ExecContext(ctx, `DELETE FROM notes WHERE id = ?`, "note-1"); err != nil {
		t.Fatalf("delete note: %v", err)
	}

	var orphans int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM segments WHERE note_id = ?`, "note-1").Scan(&orphans); err != nil {
		t.Fatalf("count segments: %v", err)
	}
	if orphans != 0 {
		t.Errorf("deleting a note left %d orphaned segments", orphans)
	}
}

// TestSearchMatchesOrthographicVariants is the end-to-end version of the
// normalisation guarantee: text indexed in one spelling must be found by a
// query typed in another. A test that only round-tripped identical strings
// would pass while real search was broken.
func TestSearchMatchesOrthographicVariants(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seedNote(t, s, "note-1")

	if err := s.ReplaceSegments(ctx, "note-1", []Segment{
		{Idx: 0, StartMS: 0, EndMS: 2000, ASRText: ahmadHamza + " " + schoolMarbuta},
	}); err != nil {
		t.Fatalf("replace segments: %v", err)
	}

	for _, query := range []string{ahmadBare, ahmadHamza, schoolHeh, schoolMarbuta} {
		n, err := s.countMatches(ctx, query)
		if err != nil {
			t.Fatalf("search %q: %v", query, err)
		}
		if n != 1 {
			t.Errorf("search %q matched %d segments, want 1", query, n)
		}
	}
}

// TestSearchSurvivesPunctuation covers the input that raises a hard FTS5 error
// rather than returning no rows.
func TestSearchSurvivesPunctuation(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seedNote(t, s, "note-1")

	if err := s.ReplaceSegments(ctx, "note-1", []Segment{
		{Idx: 0, StartMS: 0, EndMS: 2000, ASRText: "it is a test"},
	}); err != nil {
		t.Fatalf("replace segments: %v", err)
	}

	for _, query := range []string{"it's", "a-b", "(", "-", "*", "test OR", "col:value"} {
		if _, err := s.countMatches(ctx, query); err != nil {
			t.Errorf("search %q returned an error: %v", query, err)
		}
	}
}

// TestEditPreservesASRText is the correction contract: a human edit annotates
// the machine output, it never overwrites it.
func TestEditPreservesASRText(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seedNote(t, s, "note-1")

	if err := s.ReplaceSegments(ctx, "note-1", []Segment{
		{Idx: 0, StartMS: 0, EndMS: 2000, ASRText: "machine output"},
	}); err != nil {
		t.Fatalf("replace segments: %v", err)
	}

	var segID int64
	if err := s.db.QueryRowContext(ctx, `SELECT id FROM segments WHERE note_id = ?`, "note-1").
		Scan(&segID); err != nil {
		t.Fatalf("read segment id: %v", err)
	}

	if err := s.EditSegment(ctx, segID, "human correction"); err != nil {
		t.Fatalf("edit segment: %v", err)
	}

	var asr, edited string
	if err := s.db.QueryRowContext(ctx,
		`SELECT asr_text, edited_text FROM segments WHERE id = ?`, segID).
		Scan(&asr, &edited); err != nil {
		t.Fatalf("read segment: %v", err)
	}
	if asr != "machine output" {
		t.Errorf("asr_text = %q, want it unchanged", asr)
	}
	if edited != "human correction" {
		t.Errorf("edited_text = %q, want the correction", edited)
	}

	// The correction must be searchable and the superseded text must not be.
	if n, err := s.countMatches(ctx, "correction"); err != nil || n != 1 {
		t.Errorf("corrected text not indexed: matches=%d err=%v", n, err)
	}
	if n, err := s.countMatches(ctx, "machine"); err != nil || n != 0 {
		t.Errorf("superseded text still indexed: matches=%d err=%v", n, err)
	}
}

func TestEditMissingSegment(t *testing.T) {
	s := newTestStore(t)
	if err := s.EditSegment(context.Background(), 9999, "x"); !errors.Is(err, ErrNotFound) {
		t.Errorf("EditSegment on missing row = %v, want ErrNotFound", err)
	}
}

// TestDuplicateWhatsAppMessageIgnored covers redelivery: a message arrives live
// and again through offline sync on reconnect. That is expected traffic, not an
// error, and it must not create a second note or fail the ingest path.
func TestDuplicateWhatsAppMessageIgnored(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if err := s.UpsertChat(ctx, "chat-1", "Test"); err != nil {
		t.Fatalf("upsert chat: %v", err)
	}

	n := Note{
		ID: "note-1", ChatID: "chat-1", Source: "whatsapp",
		WAMessageID: "wa-abc", MediaPath: "/tmp/a.ogg", ReceivedAt: time.Now(),
	}
	if ok, err := s.CreateNote(ctx, n); err != nil || !ok {
		t.Fatalf("first insert: ok=%v err=%v", ok, err)
	}

	n.ID = "note-2"
	ok, err := s.CreateNote(ctx, n)
	if err != nil {
		t.Fatalf("redelivery returned an error: %v", err)
	}
	if ok {
		t.Error("redelivery reported a new note, want duplicate")
	}

	var count int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM notes`).Scan(&count); err != nil {
		t.Fatalf("count notes: %v", err)
	}
	if count != 1 {
		t.Errorf("note count = %d, want 1", count)
	}
}

// TestClaimNextAndRequeue covers the durable queue: claiming burns an attempt,
// a crash leaves the note recoverable, and a note that has exhausted its
// attempts fails permanently instead of looping forever.
func TestClaimNextAndRequeue(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seedNote(t, s, "note-1")

	claimed, err := s.ClaimNext(ctx)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if claimed.ID != "note-1" || claimed.Attempts != 1 {
		t.Errorf("claimed %+v, want note-1 with attempts=1", claimed)
	}

	// Queue is now empty: the note is in flight, not pending.
	if _, err := s.ClaimNext(ctx); !errors.Is(err, ErrNotFound) {
		t.Errorf("second claim = %v, want ErrNotFound", err)
	}

	// Simulate a crash mid-transcription.
	requeued, failed, err := s.RequeueStale(ctx)
	if err != nil {
		t.Fatalf("requeue: %v", err)
	}
	if requeued != 1 || failed != 0 {
		t.Errorf("requeue = (%d, %d), want (1, 0)", requeued, failed)
	}

	// Second and final attempt.
	if _, err := s.ClaimNext(ctx); err != nil {
		t.Fatalf("claim after requeue: %v", err)
	}

	// A second crash exhausts MaxAttempts, so the note must fail rather than
	// return to the queue as a poison pill.
	requeued, failed, err = s.RequeueStale(ctx)
	if err != nil {
		t.Fatalf("requeue after exhaustion: %v", err)
	}
	if failed != 1 {
		t.Errorf("failed = %d, want 1 after exhausting attempts", failed)
	}
	if requeued != 0 {
		t.Errorf("requeued = %d, want 0 after exhausting attempts", requeued)
	}
	if _, err := s.ClaimNext(ctx); !errors.Is(err, ErrNotFound) {
		t.Errorf("exhausted note was reclaimed: %v", err)
	}
}

// TestReplaceSegmentsIsIdempotent covers a retried transcription: the second
// result must replace the first without leaving duplicate rows or stale index
// entries behind.
func TestReplaceSegmentsIsIdempotent(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seedNote(t, s, "note-1")

	first := []Segment{{Idx: 0, StartMS: 0, EndMS: 1000, ASRText: "first attempt"}}
	second := []Segment{{Idx: 0, StartMS: 0, EndMS: 1000, ASRText: "second attempt"}}

	if err := s.ReplaceSegments(ctx, "note-1", first); err != nil {
		t.Fatalf("first: %v", err)
	}
	if err := s.ReplaceSegments(ctx, "note-1", second); err != nil {
		t.Fatalf("second: %v", err)
	}

	var count int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM segments WHERE note_id = ?`, "note-1").Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 1 {
		t.Errorf("segment count = %d, want 1", count)
	}
	if n, err := s.countMatches(ctx, "first"); err != nil || n != 0 {
		t.Errorf("stale index row survived: matches=%d err=%v", n, err)
	}
	if n, err := s.countMatches(ctx, "second"); err != nil || n != 1 {
		t.Errorf("new text not indexed: matches=%d err=%v", n, err)
	}
}

// countMatches runs a user-supplied query through the same path the web layer
// uses, so the tests exercise query building rather than bypassing it.
func (s *Store) countMatches(ctx context.Context, userQuery string) (int, error) {
	q := buildQuery(userQuery)
	if q == "" {
		return 0, nil
	}
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM segments_fts WHERE segments_fts MATCH ?`, q).Scan(&n)
	return n, err
}
