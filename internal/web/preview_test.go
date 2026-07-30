package web

import (
	"context"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ahmdobeidat/maktoob/internal/store"
)

// The design spec requires the list to show the first line of each transcript.
// Without it the list is sender, chat, duration and status — metadata only —
// and the reader has to open every note to find out what it says. For a tool
// whose value is skimming a pile of voice notes, that is most of the point.
func TestListShowsTheFirstLineOfEachTranscript(t *testing.T) {
	f := newFixture(t).seed()

	body := f.do(http.MethodGet, "/", nil).Body.String()

	if !strings.Contains(body, arabicLine) {
		t.Error("the list does not show the first line of the transcript")
	}
	if !strings.Contains(body, `class="note-preview" lang="ar" dir="rtl"`) {
		t.Error("the preview is not marked as Arabic right-to-left")
	}
	// Only the first line. Showing the whole transcript would make the list a
	// wall of text and defeat the point of having a detail page.
	if strings.Contains(body, "thanks for watching") {
		t.Error("the list is showing more than the first line")
	}
}

func TestPreviewPrefersACorrection(t *testing.T) {
	f := newFixture(t).seed()
	ctx := context.Background()

	if err := f.store.EditSegment(ctx, f.segIDs[0], "corrected first line"); err != nil {
		t.Fatalf("edit: %v", err)
	}

	body := f.do(http.MethodGet, "/", nil).Body.String()
	if !strings.Contains(body, "corrected first line") {
		t.Error("the list preview still shows the machine output after a correction")
	}
	if strings.Contains(body, arabicLine) {
		t.Error("the list preview shows text the note no longer says")
	}
}

// A note with no transcript yet must not render an empty preview element.
func TestPreviewAbsentWhenThereIsNoTranscript(t *testing.T) {
	f := newFixture(t)

	ctx := context.Background()
	if err := f.store.UpsertChat(ctx, "chat-1", "Umm Ahmad"); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if _, err := f.store.CreateNote(ctx, newPendingNote(f.dir)); err != nil {
		t.Fatalf("create: %v", err)
	}

	body := f.do(http.MethodGet, "/", nil).Body.String()
	if strings.Contains(body, "note-preview") {
		t.Error("a note with no transcript rendered an empty preview")
	}
}

// Arabic is multi-byte throughout, so a byte-wise truncation would cut a rune
// in half and render a replacement character in the one place this project
// cannot afford to look broken.
func TestPreviewTruncatesOnRuneBoundaries(t *testing.T) {
	long := strings.Repeat("\u0645\u0631\u062D\u0628\u0627 ", 100) // "marhaba " x100

	got := previewLine(long)

	if strings.Contains(got, "�") {
		t.Error("truncation split a rune and produced a replacement character")
	}
	if n := len([]rune(got)); n > previewMax+1 {
		t.Errorf("preview is %d runes, want at most %d plus the ellipsis", n, previewMax)
	}
	if !strings.HasSuffix(got, "…") {
		t.Errorf("a truncated preview does not end with an ellipsis: %q", got)
	}

	// Short input is returned untouched.
	if got := previewLine("short line"); got != "short line" {
		t.Errorf("previewLine(%q) = %q, want it unchanged", "short line", got)
	}
}

// Whitespace in a transcript is not meaningful to a one-line summary, and a
// newline mid-preview breaks the two-line clamp the list depends on.
func TestPreviewCollapsesWhitespace(t *testing.T) {
	if got := previewLine("  one\n\ttwo   three  "); got != "one two three" {
		t.Errorf("previewLine = %q, want %q", got, "one two three")
	}
}

// The model is recorded against every transcript and the label string for it
// already existed. It was simply never rendered.
func TestNotePageShowsWhichModelProducedTheTranscript(t *testing.T) {
	f := newFixture(t).seed()
	ctx := context.Background()

	if err := f.store.SetModel(ctx, f.noteID, "large-v3-turbo"); err != nil {
		t.Fatalf("set model: %v", err)
	}

	body := f.do(http.MethodGet, "/note/"+f.noteID, nil).Body.String()
	if !strings.Contains(body, "large-v3-turbo") {
		t.Error("the note page does not say which model produced the transcript")
	}
	if !strings.Contains(body, English.Model) {
		t.Error("the model is shown without its label")
	}
}

// newPendingNote is a note that has arrived but has no transcript yet.
func newPendingNote(dir string) store.Note {
	return store.Note{
		ID:         "pending-1",
		ChatID:     "chat-1",
		Source:     "whatsapp",
		SenderName: "Ahmad",
		MediaPath:  filepath.Join(dir, "pending-1.ogg"),
		ReceivedAt: time.Now(),
		Status:     store.StatusPending,
	}
}
