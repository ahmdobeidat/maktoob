package web

import (
	"html/template"
	"time"

	"github.com/ahmdobeidat/maktoob/internal/export"
	"github.com/ahmdobeidat/maktoob/internal/store"
)

// noteView is a note prepared for display.
//
// Every derived value — the status word, the formatted length, the marker text
// — is computed here rather than in the template. Templates that can compute
// are templates that can disagree with the JSON API about what a note says,
// and this project shows two renderings of the same transcript.
type noteView struct {
	ID          string
	Href        string
	Chat        string
	Sender      string
	Source      string
	ReceivedAt  time.Time
	ReceivedISO string
	Received    string
	Duration    string
	DurationMS  int64
	Status      string
	StatusLabel string
	Error       string
	Pending     bool
	Failed      bool
}

func newNoteView(n store.Note, loc Locale) noteView {
	return noteView{
		ID:          n.ID,
		Href:        "/note/" + n.ID,
		Chat:        chatName(n),
		Sender:      senderName(n),
		Source:      n.Source,
		ReceivedAt:  n.ReceivedAt,
		ReceivedISO: n.ReceivedAt.Format(time.RFC3339),
		Received:    n.ReceivedAt.Local().Format("2006-01-02 15:04"),
		Duration:    export.Duration(n.DurationMS),
		DurationMS:  n.DurationMS,
		Status:      n.Status,
		StatusLabel: loc.StatusLabel(n.Status),
		Error:       n.Error,
		Pending: n.Status == store.StatusPending ||
			n.Status == store.StatusConverting ||
			n.Status == store.StatusTranscribing,
		Failed: n.Status == store.StatusFailed,
	}
}

// segmentView is one transcript line prepared for display.
type segmentView struct {
	ID      int64  `json:"id"`
	NoteID  string `json:"note_id"`
	Idx     int    `json:"idx"`
	StartMS int64  `json:"start_ms"`
	EndMS   int64  `json:"end_ms"`
	// Start is the seek position in seconds, because that is what the audio
	// element takes.
	Start    float64 `json:"start"`
	Timecode string  `json:"timecode"`
	Text     string  `json:"text"`
	ASRText  string  `json:"asr_text"`
	Edited   bool    `json:"edited"`
	Suspect  bool    `json:"suspect"`
	LowConf  bool    `json:"low_confidence"`
	// Confidence is rounded to whole percent. The underlying float has more
	// digits than the number deserves.
	Confidence int `json:"confidence"`
	// Markers is the caveat list in the interface language. Never encoded by
	// colour alone; the template prints these words.
	Markers []string `json:"markers,omitempty"`
}

func newSegmentView(s store.Segment, loc Locale) segmentView {
	raw := export.Markers(s)
	markers := make([]string, 0, len(raw))
	for _, m := range raw {
		markers = append(markers, loc.MarkerLabel(m))
	}

	conf := export.Confidence(s)
	return segmentView{
		ID:         s.ID,
		NoteID:     s.NoteID,
		Idx:        s.Idx,
		StartMS:    s.StartMS,
		EndMS:      s.EndMS,
		Start:      float64(s.StartMS) / 1000,
		Timecode:   export.Timecode(s.StartMS),
		Text:       s.Text(),
		ASRText:    s.ASRText,
		Edited:     s.Edited(),
		Suspect:    s.Suspect,
		LowConf:    !s.Suspect && conf < export.LowConfidence,
		Confidence: int(conf*100 + 0.5),
		Markers:    markers,
	}
}

func newSegmentViews(segs []store.Segment, loc Locale) []segmentView {
	out := make([]segmentView, 0, len(segs))
	for _, s := range segs {
		out = append(out, newSegmentView(s, loc))
	}
	return out
}

// hitGroup is one note with the lines that matched a search.
type hitGroup struct {
	Note    noteView
	Matches []segmentView
}

// groupHits collapses a flat hit list into one entry per note, preserving the
// order the store returned. chatID, when set, drops notes from other chats.
//
// Grouping happens here rather than in SQL because the store's job is to answer
// "which segments matched"; deciding that three matches in one note is one
// result rather than three is a presentation decision.
func groupHits(hits []store.Hit, chatID string, loc Locale) []hitGroup {
	var out []hitGroup
	index := make(map[string]int, len(hits))

	for _, h := range hits {
		if chatID != "" && h.Note.ChatID != chatID {
			continue
		}
		i, seen := index[h.Note.ID]
		if !seen {
			out = append(out, hitGroup{Note: newNoteView(h.Note, loc)})
			i = len(out) - 1
			index[h.Note.ID] = i
		}
		out[i].Matches = append(out[i].Matches, newSegmentView(h.Segment, loc))
	}
	return out
}

// --- JSON shapes for the list endpoint ----------------------------------

type apiNote struct {
	ID         string    `json:"id"`
	Chat       string    `json:"chat"`
	ChatID     string    `json:"chat_id"`
	Sender     string    `json:"sender"`
	Source     string    `json:"source"`
	ReceivedAt time.Time `json:"received_at"`
	DurationMS int64     `json:"duration_ms"`
	Status     string    `json:"status"`
	Error      string    `json:"error,omitempty"`
}

func newAPINote(n store.Note) apiNote {
	return apiNote{
		ID:         n.ID,
		Chat:       chatName(n),
		ChatID:     n.ChatID,
		Sender:     senderName(n),
		Source:     n.Source,
		ReceivedAt: n.ReceivedAt,
		DurationMS: n.DurationMS,
		Status:     n.Status,
		Error:      n.Error,
	}
}

type apiHitGroup struct {
	Note    apiNote       `json:"note"`
	Matches []segmentView `json:"matches"`
}

func newAPIHitGroup(g hitGroup) apiHitGroup {
	return apiHitGroup{
		Note: apiNote{
			ID:         g.Note.ID,
			Chat:       g.Note.Chat,
			Sender:     g.Note.Sender,
			Source:     g.Note.Source,
			ReceivedAt: g.Note.ReceivedAt,
			DurationMS: g.Note.DurationMS,
			Status:     g.Note.Status,
			Error:      g.Note.Error,
		},
		Matches: g.Matches,
	}
}

// --- template helpers ---------------------------------------------------

func funcMap() template.FuncMap {
	return template.FuncMap{
		// join renders a marker list as one readable phrase.
		"join": func(sep string, items []string) string {
			out := ""
			for i, s := range items {
				if i > 0 {
					out += sep
				}
				out += s
			}
			return out
		},
		"gt0": func(n int) bool { return n > 0 },
	}
}

func chatName(n store.Note) string {
	if n.ChatName != "" {
		return n.ChatName
	}
	return n.ChatID
}

func senderName(n store.Note) string {
	if n.SenderName != "" {
		return n.SenderName
	}
	if n.Sender != "" {
		return n.Sender
	}
	return "unknown"
}
