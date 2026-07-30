package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Chat is one conversation, with how many notes it holds. The count exists so
// the filter control can show it: a chat filter that lists conversations
// without saying how much is behind each one makes the user click to find out.
type Chat struct {
	ID          string
	DisplayName string
	Notes       int
}

// Name returns what should be shown for the chat, falling back to the opaque
// alias when no display name was ever captured.
func (c Chat) Name() string {
	if c.DisplayName != "" {
		return c.DisplayName
	}
	return c.ID
}

// Hit is one search result: the matching segment and the note it belongs to.
//
// The segment travels with the note because the point of a search result is to
// show the line that matched, not merely that the note matched somewhere.
type Hit struct {
	Note    Note
	Segment Segment
}

// ListChats returns every chat that has at least one note, busiest first.
func (s *Store) ListChats(ctx context.Context) ([]Chat, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT c.id, c.display_name, COUNT(n.id) AS n
		FROM chats c
		JOIN notes n ON n.chat_id = c.id
		GROUP BY c.id, c.display_name
		ORDER BY n DESC, c.display_name ASC`)
	if err != nil {
		return nil, fmt.Errorf("list chats: %w", err)
	}
	defer rows.Close()

	var out []Chat
	for rows.Next() {
		var c Chat
		var name sql.NullString
		if err := rows.Scan(&c.ID, &name, &c.Notes); err != nil {
			return nil, fmt.Errorf("list chats: %w", err)
		}
		c.DisplayName = name.String
		out = append(out, c)
	}
	return out, rows.Err()
}

// ListNotesByChat returns notes newest first, optionally limited to one chat.
// An empty chatID means every chat.
func (s *Store) ListNotesByChat(ctx context.Context, chatID string, limit int) ([]Note, error) {
	if limit <= 0 {
		limit = 50
	}

	// Two statements rather than one with `(? = '' OR n.chat_id = ?)`, because
	// that form defeats idx_notes_received: SQLite cannot use the index for the
	// ordering and the filter at once when the filter is hidden behind an OR.
	//
	// The preview is the first line of the transcript, joined in rather than
	// fetched per note. Without it the list is metadata only and the reader has
	// to open every note to discover what it says, which for a tool whose whole
	// point is skimming is most of the value gone.
	//
	// It prefers a human correction over the machine output, exactly as the
	// transcript itself does, so the list never shows text the note no longer
	// says. idx = 0 keeps the join to one row per note.
	const cols = `
		SELECT n.id, n.chat_id, c.display_name, n.source, n.sender, n.sender_name,
		       n.media_path, n.duration_ms, n.received_at, n.status, n.error,
		       COALESCE(NULLIF(sg.edited_text, ''), sg.asr_text, '') AS preview
		FROM notes n
		JOIN chats c ON c.id = n.chat_id
		LEFT JOIN segments sg ON sg.note_id = n.id AND sg.idx = 0`

	var (
		rows *sql.Rows
		err  error
	)
	if chatID == "" {
		rows, err = s.db.QueryContext(ctx, cols+`
			ORDER BY n.received_at DESC
			LIMIT ?`, limit)
	} else {
		rows, err = s.db.QueryContext(ctx, cols+`
			WHERE n.chat_id = ?
			ORDER BY n.received_at DESC
			LIMIT ?`, chatID, limit)
	}
	if err != nil {
		return nil, fmt.Errorf("list notes: %w", err)
	}
	defer rows.Close()

	var out []Note
	for rows.Next() {
		n, err := scanListedNote(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// scanListedNote reads the column set shared by the list queries.
func scanListedNote(rows *sql.Rows) (Note, error) {
	var n Note
	var received int64
	var chatName, sender, senderName, errMsg, preview sql.NullString

	if err := rows.Scan(&n.ID, &n.ChatID, &chatName, &n.Source, &sender, &senderName,
		&n.MediaPath, &n.DurationMS, &received, &n.Status, &errMsg, &preview); err != nil {
		return Note{}, fmt.Errorf("list notes: %w", err)
	}
	n.ChatName, n.Sender = chatName.String, sender.String
	n.SenderName, n.Error = senderName.String, errMsg.String
	n.Preview = preview.String
	n.ReceivedAt = time.UnixMilli(received)
	return n, nil
}

// Search returns matching segments with their notes, most recent note first.
//
// An unusable query returns no results rather than an error, for the same
// reason SearchSegmentIDs does: a search box that reports a failure when the
// user has typed only punctuation is reporting the user's typing as a bug.
func (s *Store) Search(ctx context.Context, userQuery string, limit int) ([]Hit, error) {
	q := buildQuery(userQuery)
	if q == "" {
		return nil, nil
	}
	if limit <= 0 {
		limit = 100
	}

	rows, err := s.db.QueryContext(ctx, `
		SELECT sg.id, sg.note_id, sg.idx, sg.start_ms, sg.end_ms, sg.asr_text,
		       sg.edited_text, sg.avg_logprob, sg.no_speech_prob, sg.suspect,
		       sg.edited_at,
		       n.chat_id, c.display_name, n.source, n.sender, n.sender_name,
		       n.duration_ms, n.received_at, n.status,
		       -- The note's first line, so a search result describes the same
		       -- note the unfiltered list describes. Without it the two list
		       -- shapes disagree about what a note is.
		       COALESCE(NULLIF(first.edited_text, ''), first.asr_text, '') AS preview
		FROM segments_fts f
		JOIN segments sg ON sg.id = f.rowid
		JOIN notes n     ON n.id = sg.note_id
		JOIN chats c     ON c.id = n.chat_id
		LEFT JOIN segments first ON first.note_id = n.id AND first.idx = 0
		WHERE segments_fts MATCH ?
		ORDER BY n.received_at DESC, sg.idx ASC
		LIMIT ?`, q, limit)
	if err != nil {
		return nil, fmt.Errorf("search: %w", err)
	}
	defer rows.Close()

	var out []Hit
	for rows.Next() {
		var h Hit
		var edited, chatName, sender, senderName, preview sql.NullString
		var editedAt sql.NullInt64
		var suspect int
		var received int64

		if err := rows.Scan(
			&h.Segment.ID, &h.Segment.NoteID, &h.Segment.Idx,
			&h.Segment.StartMS, &h.Segment.EndMS, &h.Segment.ASRText,
			&edited, &h.Segment.AvgLogprob, &h.Segment.NoSpeechProb,
			&suspect, &editedAt,
			&h.Note.ChatID, &chatName, &h.Note.Source, &sender, &senderName,
			&h.Note.DurationMS, &received, &h.Note.Status, &preview,
		); err != nil {
			return nil, fmt.Errorf("search: %w", err)
		}

		h.Segment.EditedText = edited.String
		h.Segment.Suspect = suspect == 1
		if editedAt.Valid {
			t := time.UnixMilli(editedAt.Int64)
			h.Segment.EditedAt = &t
		}

		h.Note.ID = h.Segment.NoteID
		h.Note.ChatName, h.Note.Sender, h.Note.SenderName = chatName.String, sender.String, senderName.String
		h.Note.ReceivedAt = time.UnixMilli(received)
		h.Note.Preview = preview.String

		out = append(out, h)
	}
	return out, rows.Err()
}

// GetSegment returns one segment by id.
//
// The web layer uses it after saving a correction, so the response carries the
// row as it was actually stored rather than as the client hoped it would be.
func (s *Store) GetSegment(ctx context.Context, id int64) (Segment, error) {
	var sg Segment
	var edited sql.NullString
	var editedAt sql.NullInt64
	var suspect int

	err := s.db.QueryRowContext(ctx, `
		SELECT id, note_id, idx, start_ms, end_ms, asr_text, edited_text,
		       avg_logprob, no_speech_prob, mean_word_p, min_word_p,
		       temperature, suspect, edited_at
		FROM segments WHERE id = ?`, id).
		Scan(&sg.ID, &sg.NoteID, &sg.Idx, &sg.StartMS, &sg.EndMS, &sg.ASRText,
			&edited, &sg.AvgLogprob, &sg.NoSpeechProb, &sg.MeanWordP,
			&sg.MinWordP, &sg.Temperature, &suspect, &editedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Segment{}, ErrNotFound
	}
	if err != nil {
		return Segment{}, fmt.Errorf("get segment: %w", err)
	}

	sg.EditedText = edited.String
	sg.Suspect = suspect == 1
	if editedAt.Valid {
		t := time.UnixMilli(editedAt.Int64)
		sg.EditedAt = &t
	}
	return sg, nil
}

// CountNotes returns how many notes are stored, by status.
func (s *Store) CountNotes(ctx context.Context) (map[string]int, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT status, COUNT(*) FROM notes GROUP BY status`)
	if err != nil {
		return nil, fmt.Errorf("count notes: %w", err)
	}
	defer rows.Close()

	out := make(map[string]int)
	for rows.Next() {
		var status string
		var n int
		if err := rows.Scan(&status, &n); err != nil {
			return nil, fmt.Errorf("count notes: %w", err)
		}
		out[status] = n
	}
	return out, rows.Err()
}
