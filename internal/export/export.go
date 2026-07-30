// Package export renders a stored note as JSON or Markdown.
//
// Export exists so a transcript can leave maktoob without leaving the machine
// it was transcribed on. It is the answer to "what happens to my data if I stop
// using this", and that answer is only credible if the output is plain, whole,
// and readable without maktoob.
//
// The package depends on internal/store and nothing else in the app, so both
// the HTTP endpoint and the CLI command render byte-identical output from the
// same code.
package export

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/ahmdobeidat/maktoob/internal/asr"
	"github.com/ahmdobeidat/maktoob/internal/store"
)

// Format is an output encoding.
type Format string

const (
	FormatJSON     Format = "json"
	FormatMarkdown Format = "md"
)

// ParseFormat validates a user-supplied format name. An empty name is JSON,
// because that is the machine-readable default the API documents.
func ParseFormat(s string) (Format, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "json":
		return FormatJSON, nil
	case "md", "markdown":
		return FormatMarkdown, nil
	default:
		return "", fmt.Errorf("unknown export format %q: use \"json\" or \"md\"", s)
	}
}

// Write renders a note in the given format.
func Write(w io.Writer, f Format, n store.Note, segs []store.Segment) error {
	switch f {
	case FormatMarkdown:
		return Markdown(w, n, segs)
	case FormatJSON:
		return JSON(w, n, segs)
	default:
		return fmt.Errorf("unknown export format %q", f)
	}
}

// noteDoc is the exported JSON shape. It is a separate type from store.Note on
// purpose: the stored row carries local filesystem paths, and an export is a
// file the user may well send to someone else. Nothing here reveals where on
// disk the audio lives.
type noteDoc struct {
	ID         string       `json:"id"`
	Source     string       `json:"source"`
	Chat       string       `json:"chat"`
	Sender     string       `json:"sender"`
	ReceivedAt time.Time    `json:"received_at"`
	DurationMS int64        `json:"duration_ms"`
	Status     string       `json:"status"`
	Error      string       `json:"error,omitempty"`
	Model      string       `json:"model,omitempty"`
	Text       string       `json:"text"`
	Segments   []segmentDoc `json:"segments"`
}

type segmentDoc struct {
	Idx        int        `json:"idx"`
	StartMS    int64      `json:"start_ms"`
	EndMS      int64      `json:"end_ms"`
	Text       string     `json:"text"`
	ASRText    string     `json:"asr_text"`
	Edited     bool       `json:"edited"`
	EditedAt   *time.Time `json:"edited_at,omitempty"`
	Confidence float64    `json:"confidence"`
	NoSpeech   float64    `json:"no_speech_prob"`
	Suspect    bool       `json:"suspect"`
}

// JSON writes the note as an indented JSON document.
func JSON(w io.Writer, n store.Note, segs []store.Segment) error {
	doc := noteDoc{
		ID:         n.ID,
		Source:     n.Source,
		Chat:       displayChat(n),
		Sender:     displaySender(n),
		ReceivedAt: n.ReceivedAt,
		DurationMS: n.DurationMS,
		Status:     n.Status,
		Error:      n.Error,
		Model:      n.Model,
		Text:       PlainText(segs),
		Segments:   make([]segmentDoc, 0, len(segs)),
	}

	for _, s := range segs {
		doc.Segments = append(doc.Segments, segmentDoc{
			Idx:        s.Idx,
			StartMS:    s.StartMS,
			EndMS:      s.EndMS,
			Text:       s.Text(),
			ASRText:    s.ASRText,
			Edited:     s.Edited(),
			EditedAt:   s.EditedAt,
			Confidence: Confidence(s),
			NoSpeech:   s.NoSpeechProb,
			Suspect:    s.Suspect,
		})
	}

	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	// Escaping is off because the payload is Arabic. With Go's default HTML
	// escaping the text survives, but every export a user opens in an editor is
	// full of <-style noise for no benefit: this is a file, not an inline
	// script, and the HTTP layer sets a JSON content type.
	enc.SetEscapeHTML(false)
	return enc.Encode(doc)
}

// Markdown writes the note as a readable document.
//
// Confidence, suspicion and human edits are rendered as words. The web
// interface and the terminal both make that commitment for accessibility
// reasons, and an export that dropped it would be the one artifact where the
// warning silently disappears.
func Markdown(w io.Writer, n store.Note, segs []store.Segment) error {
	var b strings.Builder

	fmt.Fprintf(&b, "# Voice note from %s\n\n", displaySender(n))
	fmt.Fprintf(&b, "- Chat: %s\n", displayChat(n))
	fmt.Fprintf(&b, "- Received: %s\n", n.ReceivedAt.Format(time.RFC3339))
	fmt.Fprintf(&b, "- Length: %s\n", Duration(n.DurationMS))
	fmt.Fprintf(&b, "- Status: %s\n", n.Status)
	if n.Error != "" {
		fmt.Fprintf(&b, "- Error: %s\n", n.Error)
	}
	if n.Model != "" {
		fmt.Fprintf(&b, "- Model: %s\n", n.Model)
	}
	fmt.Fprintf(&b, "- Note id: %s\n\n", n.ID)

	b.WriteString("## Transcript\n\n")

	if len(segs) == 0 {
		b.WriteString("_No speech was detected in this note._\n")
		_, err := io.WriteString(w, b.String())
		return err
	}

	for _, s := range segs {
		fmt.Fprintf(&b, "**[%s – %s]** %s",
			Timecode(s.StartMS), Timecode(s.EndMS), s.Text())
		if notes := Markers(s); len(notes) > 0 {
			fmt.Fprintf(&b, "  _(%s)_", strings.Join(notes, ", "))
		}
		b.WriteString("\n\n")
	}

	b.WriteString("---\n\n")
	b.WriteString("Transcribed locally by maktoob. Machine transcription is not reliable; ")
	b.WriteString("segments marked _low confidence_ or _possible fabrication_ should be checked against the audio.\n")

	_, err := io.WriteString(w, b.String())
	return err
}

// PlainText joins every segment into one readable block.
func PlainText(segs []store.Segment) string {
	parts := make([]string, 0, len(segs))
	for _, s := range segs {
		if t := strings.TrimSpace(s.Text()); t != "" {
			parts = append(parts, t)
		}
	}
	return strings.Join(parts, " ")
}

// Markers describes a segment's caveats in words.
//
// Shared by the exporter and the web templates so a segment never carries a
// warning in one place and not the other.
func Markers(s store.Segment) []string {
	var out []string
	if s.Suspect {
		out = append(out, "possible fabrication")
	} else if Confidence(s) < LowConfidence {
		out = append(out, "low confidence")
	}
	if s.Edited() {
		out = append(out, "edited by hand")
	}
	return out
}

// LowConfidence is the threshold below which a segment is flagged for review.
// It matches the terminal renderer so the two never disagree.
const LowConfidence = 0.5

// Confidence converts a segment's average log probability into a probability.
func Confidence(s store.Segment) float64 {
	return asr.Segment{AvgLogprob: s.AvgLogprob}.Confidence()
}

// Timecode formats a position within a note as m:ss.
func Timecode(ms int64) string {
	if ms < 0 {
		ms = 0
	}
	total := ms / 1000
	return fmt.Sprintf("%d:%02d", total/60, total%60)
}

// Duration formats a note length for a human.
func Duration(ms int64) string {
	if ms <= 0 {
		return "unknown"
	}
	return fmt.Sprintf("%.1fs", float64(ms)/1000)
}

// Filename is a safe download name for a note in the given format.
func Filename(n store.Note, f Format) string {
	ext := "json"
	if f == FormatMarkdown {
		ext = "md"
	}
	return fmt.Sprintf("maktoob-%s.%s", safeID(n.ID), ext)
}

// safeID keeps only characters that cannot break out of a Content-Disposition
// filename. Note ids are generated internally, but this is the one place a note
// id reaches an HTTP header, so it is filtered rather than trusted.
func safeID(id string) string {
	var b strings.Builder
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		}
	}
	if b.Len() == 0 {
		return "note"
	}
	return b.String()
}

func displayChat(n store.Note) string {
	if n.ChatName != "" {
		return n.ChatName
	}
	return n.ChatID
}

func displaySender(n store.Note) string {
	if n.SenderName != "" {
		return n.SenderName
	}
	if n.Sender != "" {
		return n.Sender
	}
	return "unknown"
}
