package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

// seedChatNote creates a note in a named chat at a chosen time, so the ordering
// and filtering assertions below do not depend on how fast the test runs.
func seedChatNote(t *testing.T, s *Store, id, chatID, chatName string, at time.Time) Note {
	t.Helper()
	ctx := context.Background()
	if err := s.UpsertChat(ctx, chatID, chatName); err != nil {
		t.Fatalf("upsert chat: %v", err)
	}
	n := Note{
		ID:         id,
		ChatID:     chatID,
		Source:     "import",
		SenderName: "Ahmad",
		MediaPath:  "/tmp/" + id + ".ogg",
		DurationMS: 5000,
		ReceivedAt: at,
		Status:     StatusDone,
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

func TestListNotesByChatFilters(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	base := time.Now().Add(-time.Hour)

	seedChatNote(t, s, "a1", "chat-a", "Chat A", base)
	seedChatNote(t, s, "b1", "chat-b", "Chat B", base.Add(time.Minute))
	seedChatNote(t, s, "a2", "chat-a", "Chat A", base.Add(2*time.Minute))

	all, err := s.ListNotesByChat(ctx, "", 50)
	if err != nil {
		t.Fatalf("list all: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("unfiltered list returned %d notes, want 3", len(all))
	}
	// Newest first is what the interface promises; a regression here silently
	// buries the note the user just received.
	if all[0].ID != "a2" || all[2].ID != "a1" {
		t.Errorf("list order = %s,%s,%s, want a2,b1,a1", all[0].ID, all[1].ID, all[2].ID)
	}

	onlyA, err := s.ListNotesByChat(ctx, "chat-a", 50)
	if err != nil {
		t.Fatalf("list chat-a: %v", err)
	}
	if len(onlyA) != 2 {
		t.Fatalf("chat-a returned %d notes, want 2", len(onlyA))
	}
	for _, n := range onlyA {
		if n.ChatID != "chat-a" {
			t.Errorf("filter leaked a note from %q", n.ChatID)
		}
		if n.ChatName != "Chat A" {
			t.Errorf("chat display name = %q, want %q", n.ChatName, "Chat A")
		}
	}

	none, err := s.ListNotesByChat(ctx, "chat-missing", 50)
	if err != nil {
		t.Fatalf("list missing chat: %v", err)
	}
	if len(none) != 0 {
		t.Errorf("unknown chat returned %d notes, want 0", len(none))
	}
}

func TestListNotesByChatRespectsLimit(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	base := time.Now().Add(-time.Hour)

	for i, id := range []string{"n1", "n2", "n3"} {
		seedChatNote(t, s, id, "chat-a", "Chat A", base.Add(time.Duration(i)*time.Minute))
	}

	got, err := s.ListNotesByChat(ctx, "", 2)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("limit 2 returned %d notes", len(got))
	}
	if got[0].ID != "n3" {
		t.Errorf("limit dropped the newest note, got %q first", got[0].ID)
	}
}

func TestListChatsCountsAndOrders(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	base := time.Now().Add(-time.Hour)

	seedChatNote(t, s, "a1", "chat-a", "Chat A", base)
	seedChatNote(t, s, "a2", "chat-a", "Chat A", base.Add(time.Minute))
	seedChatNote(t, s, "b1", "chat-b", "Chat B", base.Add(2*time.Minute))
	// A chat with no notes must not appear: it is a filter option that would
	// always return an empty list.
	if err := s.UpsertChat(ctx, "chat-empty", "Empty"); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	chats, err := s.ListChats(ctx)
	if err != nil {
		t.Fatalf("list chats: %v", err)
	}
	if len(chats) != 2 {
		t.Fatalf("got %d chats, want 2 (the empty chat must be excluded)", len(chats))
	}
	if chats[0].ID != "chat-a" || chats[0].Notes != 2 {
		t.Errorf("busiest chat = %s with %d notes, want chat-a with 2",
			chats[0].ID, chats[0].Notes)
	}
	if chats[1].Notes != 1 {
		t.Errorf("second chat has %d notes, want 1", chats[1].Notes)
	}
}

func TestChatNameFallsBackToAlias(t *testing.T) {
	if got := (Chat{ID: "opaque-alias"}).Name(); got != "opaque-alias" {
		t.Errorf("Name() = %q, want the alias when no display name exists", got)
	}
	if got := (Chat{ID: "opaque-alias", DisplayName: "Umm Ahmad"}).Name(); got != "Umm Ahmad" {
		t.Errorf("Name() = %q, want the display name", got)
	}
}

func TestSearchReturnsSegmentsWithTheirNotes(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	base := time.Now().Add(-time.Hour)

	seedChatNote(t, s, "old", "chat-a", "Chat A", base)
	seedChatNote(t, s, "new", "chat-b", "Chat B", base.Add(time.Hour))

	if err := s.ReplaceSegments(ctx, "old", []Segment{
		{Idx: 0, StartMS: 0, EndMS: 1000, ASRText: ahmadHamza + " went to " + schoolMarbuta, AvgLogprob: -0.1},
	}); err != nil {
		t.Fatalf("segments old: %v", err)
	}
	if err := s.ReplaceSegments(ctx, "new", []Segment{
		{Idx: 0, StartMS: 0, EndMS: 1000, ASRText: "nothing relevant", AvgLogprob: -0.1},
		{Idx: 1, StartMS: 1000, EndMS: 2000, ASRText: "see " + ahmadBare + " tomorrow", AvgLogprob: -0.1},
	}); err != nil {
		t.Fatalf("segments new: %v", err)
	}

	// Searched with the hamza form; one note stored it bare. The normaliser is
	// what makes this one result set instead of two.
	hits, err := s.Search(ctx, ahmadHamza, 50)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(hits) != 2 {
		t.Fatalf("got %d hits, want 2 across both spellings", len(hits))
	}

	// Newest note first, matching the list ordering.
	if hits[0].Note.ID != "new" {
		t.Errorf("first hit is note %q, want the newest", hits[0].Note.ID)
	}
	if hits[0].Segment.Idx != 1 {
		t.Errorf("matched segment idx = %d, want the segment that actually matched", hits[0].Segment.Idx)
	}
	if hits[0].Note.ChatName != "Chat B" {
		t.Errorf("hit carries chat name %q, want %q", hits[0].Note.ChatName, "Chat B")
	}
	if hits[0].Note.Status != StatusDone {
		t.Errorf("hit carries status %q, want it populated", hits[0].Note.Status)
	}
	if hits[0].Note.ReceivedAt.IsZero() {
		t.Error("hit carries a zero received_at")
	}
	if hits[1].Note.ID != "old" || hits[1].Segment.NoteID != "old" {
		t.Errorf("second hit = note %q segment of %q, want both to be the older note",
			hits[1].Note.ID, hits[1].Segment.NoteID)
	}
}

func TestSearchPrefersTheCorrectedText(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seedChatNote(t, s, "n1", "chat-a", "Chat A", time.Now())

	if err := s.ReplaceSegments(ctx, "n1", []Segment{
		{Idx: 0, StartMS: 0, EndMS: 1000, ASRText: "machine guess", AvgLogprob: -0.1},
	}); err != nil {
		t.Fatalf("segments: %v", err)
	}

	segs, err := s.segmentsFor(ctx, "n1")
	if err != nil {
		t.Fatalf("segments: %v", err)
	}
	if err := s.EditSegment(ctx, segs[0].ID, "human correction"); err != nil {
		t.Fatalf("edit: %v", err)
	}

	hits, err := s.Search(ctx, "correction", 50)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(hits) != 1 {
		t.Fatalf("corrected text is not searchable: got %d hits, want 1", len(hits))
	}
	if hits[0].Segment.Text() != "human correction" {
		t.Errorf("hit text = %q, want the correction", hits[0].Segment.Text())
	}
	if !hits[0].Segment.Edited() {
		t.Error("hit does not report the segment as edited")
	}
}

// An empty or punctuation-only box is the user mid-typing, not a fault.
func TestSearchTreatsUnusableQueriesAsNoResults(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	for _, q := range []string{"", "   ", `"`, "()", "*"} {
		hits, err := s.Search(ctx, q, 50)
		if err != nil {
			t.Errorf("Search(%q) returned an error: %v", q, err)
		}
		if len(hits) != 0 {
			t.Errorf("Search(%q) returned %d hits, want 0", q, len(hits))
		}
	}
}

func TestGetSegment(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seedChatNote(t, s, "n1", "chat-a", "Chat A", time.Now())

	if err := s.ReplaceSegments(ctx, "n1", []Segment{
		{Idx: 0, StartMS: 250, EndMS: 1750, ASRText: "hello", AvgLogprob: -0.3, Suspect: true},
	}); err != nil {
		t.Fatalf("segments: %v", err)
	}
	segs, err := s.segmentsFor(ctx, "n1")
	if err != nil {
		t.Fatalf("segments: %v", err)
	}

	got, err := s.GetSegment(ctx, segs[0].ID)
	if err != nil {
		t.Fatalf("get segment: %v", err)
	}
	if got.NoteID != "n1" || got.ASRText != "hello" || got.StartMS != 250 || !got.Suspect {
		t.Errorf("GetSegment returned %+v, want the stored row", got)
	}

	if _, err := s.GetSegment(ctx, 99999); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing segment returned %v, want ErrNotFound", err)
	}
}

func TestCountNotesByStatus(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	base := time.Now()

	seedChatNote(t, s, "n1", "chat-a", "Chat A", base)
	seedChatNote(t, s, "n2", "chat-a", "Chat A", base.Add(time.Minute))
	seedChatNote(t, s, "n3", "chat-a", "Chat A", base.Add(2*time.Minute))
	if err := s.SetStatus(ctx, "n3", StatusFailed, "boom"); err != nil {
		t.Fatalf("set status: %v", err)
	}

	counts, err := s.CountNotes(ctx)
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if counts[StatusDone] != 2 {
		t.Errorf("done = %d, want 2", counts[StatusDone])
	}
	if counts[StatusFailed] != 1 {
		t.Errorf("failed = %d, want 1", counts[StatusFailed])
	}
}
