package export

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/ahmdobeidat/maktoob/internal/store"
)

func sampleNote() (store.Note, []store.Segment) {
	edited := time.Date(2026, 7, 30, 12, 5, 0, 0, time.UTC)
	n := store.Note{
		ID:         "note-abc123",
		ChatID:     "chat-opaque-alias",
		ChatName:   "Umm Ahmad",
		Source:     "whatsapp",
		Sender:     "sender-opaque-alias",
		SenderName: "Ahmad",
		MediaPath:  "/home/secret/data/media/note-abc123.ogg",
		WavPath:    "/home/secret/data/media/note-abc123.wav",
		DurationMS: 12400,
		ReceivedAt: time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC),
		Status:     store.StatusDone,
		Model:      "large-v3-turbo",
	}
	segs := []store.Segment{
		{ID: 1, NoteID: n.ID, Idx: 0, StartMS: 0, EndMS: 4000,
			ASRText: "raw machine text", EditedText: "corrected by a human",
			AvgLogprob: -0.05, EditedAt: &edited},
		{ID: 2, NoteID: n.ID, Idx: 1, StartMS: 4000, EndMS: 9000,
			ASRText: "quiet mumbling", AvgLogprob: -1.5},
		{ID: 3, NoteID: n.ID, Idx: 2, StartMS: 9000, EndMS: 12400,
			ASRText: "thanks for watching", AvgLogprob: -0.02,
			NoSpeechProb: 0.94, Suspect: true},
	}
	return n, segs
}

// The export is a file the user may send to someone else. A local filesystem
// path in it discloses their home directory and their data layout to whoever
// receives it, so its absence is a property worth asserting rather than a
// happy accident of the struct tags.
func TestJSONOmitsLocalPaths(t *testing.T) {
	n, segs := sampleNote()

	var buf bytes.Buffer
	if err := JSON(&buf, n, segs); err != nil {
		t.Fatalf("JSON: %v", err)
	}

	for _, leak := range []string{n.MediaPath, n.WavPath, "/home/secret", ".wav"} {
		if strings.Contains(buf.String(), leak) {
			t.Errorf("export leaked %q:\n%s", leak, buf.String())
		}
	}
}

func TestJSONPrefersEditedTextButKeepsTheOriginal(t *testing.T) {
	n, segs := sampleNote()

	var buf bytes.Buffer
	if err := JSON(&buf, n, segs); err != nil {
		t.Fatalf("JSON: %v", err)
	}

	var doc struct {
		Text     string `json:"text"`
		Segments []struct {
			Text    string  `json:"text"`
			ASRText string  `json:"asr_text"`
			Edited  bool    `json:"edited"`
			Suspect bool    `json:"suspect"`
			Conf    float64 `json:"confidence"`
		} `json:"segments"`
	}
	if err := json.Unmarshal(buf.Bytes(), &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if len(doc.Segments) != 3 {
		t.Fatalf("got %d segments, want 3", len(doc.Segments))
	}
	if doc.Segments[0].Text != "corrected by a human" {
		t.Errorf("text = %q, want the human correction", doc.Segments[0].Text)
	}
	if doc.Segments[0].ASRText != "raw machine text" {
		t.Errorf("asr_text = %q, want the machine original preserved", doc.Segments[0].ASRText)
	}
	if !doc.Segments[0].Edited {
		t.Error("edited = false on a segment with an edit timestamp")
	}
	if doc.Segments[1].Edited {
		t.Error("edited = true on an untouched segment")
	}
	if !strings.Contains(doc.Text, "corrected by a human") {
		t.Errorf("joined text used the machine output: %q", doc.Text)
	}
	if doc.Segments[1].Conf > 0.5 {
		t.Errorf("confidence = %v for avg_logprob -1.5, want < 0.5", doc.Segments[1].Conf)
	}
}

// Arabic is the point of the project. Go's encoder escapes non-ASCII in some
// configurations, and a transcript that survives only as \u0645... is not a
// readable export.
func TestJSONKeepsArabicReadable(t *testing.T) {
	n, _ := sampleNote()
	segs := []store.Segment{{Idx: 0, ASRText: "مرحبا كيف حالك", AvgLogprob: -0.1}}

	var buf bytes.Buffer
	if err := JSON(&buf, n, segs); err != nil {
		t.Fatalf("JSON: %v", err)
	}
	if !strings.Contains(buf.String(), "مرحبا كيف حالك") {
		t.Errorf("Arabic text was escaped out of the export:\n%s", buf.String())
	}
}

func TestMarkdownMarksCaveatsInWords(t *testing.T) {
	n, segs := sampleNote()

	var buf bytes.Buffer
	if err := Markdown(&buf, n, segs); err != nil {
		t.Fatalf("Markdown: %v", err)
	}
	out := buf.String()

	for _, want := range []string{
		"Voice note from Ahmad",
		"Umm Ahmad",
		"corrected by a human",
		"edited by hand",
		"low confidence",
		"possible fabrication",
		"note-abc123",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("markdown export missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "/home/secret") {
		t.Errorf("markdown export leaked a local path:\n%s", out)
	}
}

func TestMarkdownHandlesNoSpeech(t *testing.T) {
	n, _ := sampleNote()

	var buf bytes.Buffer
	if err := Markdown(&buf, n, nil); err != nil {
		t.Fatalf("Markdown: %v", err)
	}
	if !strings.Contains(buf.String(), "No speech") {
		t.Errorf("empty transcript rendered without explanation:\n%s", buf.String())
	}
}

func TestMarkersDoNotDoubleFlag(t *testing.T) {
	// A suspect segment is usually high-confidence — that is exactly what makes
	// it dangerous. Reporting it as "low confidence" too would be wrong and
	// would bury the more serious marker.
	got := Markers(store.Segment{AvgLogprob: -0.02, NoSpeechProb: 0.9, Suspect: true})
	if len(got) != 1 || got[0] != "possible fabrication" {
		t.Errorf("Markers = %v, want only the fabrication marker", got)
	}
}

func TestParseFormat(t *testing.T) {
	for _, tc := range []struct {
		in      string
		want    Format
		wantErr bool
	}{
		{"", FormatJSON, false},
		{"json", FormatJSON, false},
		{" JSON ", FormatJSON, false},
		{"md", FormatMarkdown, false},
		{"markdown", FormatMarkdown, false},
		{"pdf", "", true},
		{"../../etc/passwd", "", true},
	} {
		got, err := ParseFormat(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("ParseFormat(%q) accepted an unknown format", tc.in)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseFormat(%q): %v", tc.in, err)
		}
		if got != tc.want {
			t.Errorf("ParseFormat(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// The note id reaches a Content-Disposition header. Nothing generates ids with
// quotes or newlines today, but this is the boundary where that would stop
// being a harmless internal detail.
func TestFilenameStripsHeaderBreakingCharacters(t *testing.T) {
	n := store.Note{ID: `ab"c` + "\r\nX-Injected: 1"}
	got := Filename(n, FormatMarkdown)

	if strings.ContainsAny(got, "\"\r\n ;") {
		t.Errorf("Filename = %q, still contains header-breaking characters", got)
	}
	if !strings.HasSuffix(got, ".md") {
		t.Errorf("Filename = %q, want a .md suffix", got)
	}
	if Filename(store.Note{ID: "///"}, FormatJSON) != "maktoob-note.json" {
		t.Errorf("an id with nothing safe left should fall back, got %q",
			Filename(store.Note{ID: "///"}, FormatJSON))
	}
}

func TestTimecodeAndDuration(t *testing.T) {
	for _, tc := range []struct{ ms, want any }{
		{int64(0), "0:00"},
		{int64(4000), "0:04"},
		{int64(65000), "1:05"},
		{int64(-5), "0:00"},
		{int64(3_600_000), "60:00"},
	} {
		if got := Timecode(tc.ms.(int64)); got != tc.want {
			t.Errorf("Timecode(%v) = %q, want %q", tc.ms, got, tc.want)
		}
	}
	if got := Duration(0); got != "unknown" {
		t.Errorf("Duration(0) = %q, want %q", got, "unknown")
	}
	if got := Duration(12400); got != "12.4s" {
		t.Errorf("Duration(12400) = %q, want %q", got, "12.4s")
	}
}

func TestWriteDispatchesOnFormat(t *testing.T) {
	n, segs := sampleNote()

	var j, m bytes.Buffer
	if err := Write(&j, FormatJSON, n, segs); err != nil {
		t.Fatalf("Write json: %v", err)
	}
	if err := Write(&m, FormatMarkdown, n, segs); err != nil {
		t.Fatalf("Write md: %v", err)
	}
	if !json.Valid(j.Bytes()) {
		t.Error("json format did not produce valid JSON")
	}
	if !strings.HasPrefix(m.String(), "# ") {
		t.Errorf("md format did not produce markdown: %q", m.String()[:20])
	}
	if err := Write(&m, Format("xml"), n, segs); err == nil {
		t.Error("Write accepted an unknown format")
	}
}
