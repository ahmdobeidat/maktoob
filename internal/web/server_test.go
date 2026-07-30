package web

import (
	"context"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ahmdobeidat/maktoob/internal/store"
)

const (
	arabicHello  = "\u0645\u0631\u062D\u0628\u0627"                                             // marhaba
	arabicSchool = "\u0645\u062F\u0631\u0633\u0629"                                             // madrasa
	arabicLine   = "\u0645\u0631\u062D\u0628\u0627 \u0643\u064A\u0641 \u062D\u0627\u0644\u0643" // marhaba kayf halak
)

type fixture struct {
	t      *testing.T
	srv    *Server
	store  *store.Store
	dir    string
	noteID string
	segIDs []int64
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	dir := t.TempDir()

	st, err := store.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	srv, err := New(Options{Store: st})
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	return &fixture{t: t, srv: srv, store: st, dir: dir}
}

// seed writes one note with three segments: a clean line, a low-confidence
// line, and a suspect line. Between them they exercise every marker the
// interface can render.
func (f *fixture) seed() *fixture {
	f.t.Helper()
	ctx := context.Background()

	if err := f.store.UpsertChat(ctx, "chat-1", "Umm Ahmad"); err != nil {
		f.t.Fatalf("upsert chat: %v", err)
	}

	media := filepath.Join(f.dir, "note-1.ogg")
	if err := os.WriteFile(media, []byte("OggS-not-really-audio"), 0o600); err != nil {
		f.t.Fatalf("write media: %v", err)
	}

	f.noteID = "note-1"
	ok, err := f.store.CreateNote(ctx, store.Note{
		ID:         f.noteID,
		ChatID:     "chat-1",
		Source:     "whatsapp",
		Sender:     "sender-alias",
		SenderName: "Ahmad",
		MediaPath:  media,
		DurationMS: 12400,
		ReceivedAt: time.Now().Add(-time.Hour),
		Status:     store.StatusDone,
	})
	if err != nil || !ok {
		f.t.Fatalf("create note: %v (ok=%v)", err, ok)
	}

	if err := f.store.ReplaceSegments(ctx, f.noteID, []store.Segment{
		{Idx: 0, StartMS: 0, EndMS: 4000, ASRText: arabicLine, AvgLogprob: -0.05},
		{Idx: 1, StartMS: 4000, EndMS: 8000, ASRText: arabicSchool, AvgLogprob: -1.6},
		{Idx: 2, StartMS: 8000, EndMS: 12400, ASRText: "thanks for watching",
			AvgLogprob: -0.02, NoSpeechProb: 0.95, Suspect: true},
	}); err != nil {
		f.t.Fatalf("replace segments: %v", err)
	}

	_, segs, err := f.store.GetNote(ctx, f.noteID)
	if err != nil {
		f.t.Fatalf("get note: %v", err)
	}
	for _, s := range segs {
		f.segIDs = append(f.segIDs, s.ID)
	}
	return f
}

func (f *fixture) do(method, target string, body io.Reader) *httptest.ResponseRecorder {
	f.t.Helper()
	req := httptest.NewRequest(method, target, body)
	// Browsers send this on same-origin requests; the guard uses it first.
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	rec := httptest.NewRecorder()
	f.srv.ServeHTTP(rec, req)
	return rec
}

// --- pages ---------------------------------------------------------------

func TestListPageRendersNotes(t *testing.T) {
	f := newFixture(t).seed()

	rec := f.do(http.MethodGet, "/", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200\n%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()

	for _, want := range []string{"Ahmad", "Umm Ahmad", "/note/note-1", "12.4s", "transcribed"} {
		if !strings.Contains(body, want) {
			t.Errorf("list page missing %q", want)
		}
	}
	if !strings.Contains(body, `id="live"`) || !strings.Contains(body, `aria-live="polite"`) {
		t.Error("list page has no polite live region; SSE updates would be silent")
	}
	if !strings.Contains(body, "skip-link") {
		t.Error("list page has no skip link")
	}
}

func TestListPageEmptyState(t *testing.T) {
	f := newFixture(t)

	rec := f.do(http.MethodGet, "/", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), English.NoNotes) {
		t.Error("empty database did not render the empty state")
	}
}

func TestNotePageRendersTranscript(t *testing.T) {
	f := newFixture(t).seed()

	rec := f.do(http.MethodGet, "/note/"+f.noteID, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200\n%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()

	if !strings.Contains(body, arabicLine) {
		t.Error("transcript text is missing from the page")
	}
	// The single most breakable requirement in the whole interface: Arabic in a
	// container that is not marked rtl renders with punctuation and numbers on
	// the wrong side, and it looks like a font problem rather than a bug.
	if !strings.Contains(body, `lang="ar" dir="rtl"`) {
		t.Error("transcript container is not marked lang=ar dir=rtl")
	}
	if !strings.Contains(body, `<audio`) || !strings.Contains(body, "/api/notes/note-1/audio") {
		t.Error("note page has no audio player pointed at the note")
	}
	for _, id := range f.segIDs {
		if !strings.Contains(body, `id="seg-`+strconv.FormatInt(id, 10)+`"`) {
			t.Errorf("segment %d has no anchor to link a search result to", id)
		}
	}
}

// Colour is not a channel every reader has. Both warnings must exist as text.
func TestNotePageWritesWarningsAsWords(t *testing.T) {
	f := newFixture(t).seed()

	body := f.do(http.MethodGet, "/note/"+f.noteID, nil).Body.String()

	if !strings.Contains(body, English.LowConfMark) {
		t.Error("low-confidence line carries no worded marker")
	}
	if !strings.Contains(body, English.SuspectMark) {
		t.Error("suspect line carries no worded marker")
	}
	if !strings.Contains(body, English.MarksLegend) {
		t.Error("page does not explain what the markers mean")
	}
}

func TestNotePageMissingAudioIsStated(t *testing.T) {
	f := newFixture(t).seed()
	// Delete the media the way a user cleaning their disk would.
	note, _, err := f.store.GetNote(context.Background(), f.noteID)
	if err != nil {
		t.Fatalf("get note: %v", err)
	}
	if err := os.Remove(note.MediaPath); err != nil {
		t.Fatalf("remove media: %v", err)
	}

	body := f.do(http.MethodGet, "/note/"+f.noteID, nil).Body.String()
	if strings.Contains(body, "<audio") {
		t.Error("page offers a player for audio that is not on disk")
	}
	if !strings.Contains(body, English.AudioMissing) {
		t.Error("page does not say the audio is gone")
	}
}

func TestUnknownNoteIs404(t *testing.T) {
	f := newFixture(t).seed()

	if rec := f.do(http.MethodGet, "/note/does-not-exist", nil); rec.Code != http.StatusNotFound {
		t.Errorf("page status = %d, want 404", rec.Code)
	}
	if rec := f.do(http.MethodGet, "/api/notes/does-not-exist", nil); rec.Code != http.StatusNotFound {
		t.Errorf("api status = %d, want 404", rec.Code)
	}
}

// --- search --------------------------------------------------------------

func TestSearchPageLinksToTheMatchingSegment(t *testing.T) {
	f := newFixture(t).seed()

	rec := f.do(http.MethodGet, "/?q="+arabicSchool, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()

	// The href must point at the hit's own note. An earlier version reached for
	// the page-level note here, which is nil on the list page, and every search
	// result linked to /#seg-N.
	want := "/note/" + f.noteID + "#seg-" + strconv.FormatInt(f.segIDs[1], 10)
	if !strings.Contains(body, want) {
		t.Errorf("search result does not link to %q\n%s", want, body)
	}
	if !strings.Contains(body, English.MatchesIn) {
		t.Error("search results do not show the matching lines")
	}
}

func TestSearchWithNoResults(t *testing.T) {
	f := newFixture(t).seed()

	body := f.do(http.MethodGet, "/?q=zzzznotalanguage", nil).Body.String()
	if !strings.Contains(body, English.NoResults) {
		t.Error("a search with no hits did not say so")
	}
}

func TestChatFilterNarrowsTheList(t *testing.T) {
	f := newFixture(t).seed()

	if body := f.do(http.MethodGet, "/?chat=chat-1", nil).Body.String(); !strings.Contains(body, "/note/note-1") {
		t.Error("filtering by the note's own chat hid it")
	}
	if body := f.do(http.MethodGet, "/?chat=chat-other", nil).Body.String(); strings.Contains(body, "/note/note-1") {
		t.Error("filtering by a different chat still showed the note")
	}
}

// --- JSON API ------------------------------------------------------------

func TestAPINotesList(t *testing.T) {
	f := newFixture(t).seed()

	rec := f.do(http.MethodGet, "/api/notes", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	var got struct {
		Notes []struct {
			ID     string `json:"id"`
			Chat   string `json:"chat"`
			Sender string `json:"sender"`
			Status string `json:"status"`
		} `json:"notes"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, rec.Body.String())
	}
	if len(got.Notes) != 1 || got.Notes[0].ID != f.noteID {
		t.Fatalf("got %+v, want the seeded note", got.Notes)
	}
	if got.Notes[0].Sender != "Ahmad" || got.Notes[0].Chat != "Umm Ahmad" {
		t.Errorf("list entry lost its display names: %+v", got.Notes[0])
	}
}

func TestAPINotesSearchGroupsByNote(t *testing.T) {
	f := newFixture(t).seed()

	rec := f.do(http.MethodGet, "/api/notes?q="+arabicHello, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	var got struct {
		Query   string `json:"query"`
		Results []struct {
			Note    struct{ ID string } `json:"note"`
			Matches []struct {
				ID   int64  `json:"id"`
				Text string `json:"text"`
			} `json:"matches"`
		} `json:"results"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, rec.Body.String())
	}
	if len(got.Results) != 1 {
		t.Fatalf("got %d result groups, want 1", len(got.Results))
	}
	if len(got.Results[0].Matches) == 0 {
		t.Fatal("result group carries no matching lines")
	}
	if got.Results[0].Note.ID != f.noteID {
		t.Errorf("group is for note %q, want %q", got.Results[0].Note.ID, f.noteID)
	}
}

func TestAPINoteMatchesTheExportShape(t *testing.T) {
	f := newFixture(t).seed()

	api := f.do(http.MethodGet, "/api/notes/"+f.noteID, nil)
	exp := f.do(http.MethodGet, "/api/notes/"+f.noteID+"/export?format=json", nil)

	if api.Code != http.StatusOK || exp.Code != http.StatusOK {
		t.Fatalf("statuses = %d and %d, want 200", api.Code, exp.Code)
	}
	if api.Body.String() != exp.Body.String() {
		t.Error("the read API and the JSON export disagree about the same note")
	}
	if !strings.Contains(api.Body.String(), arabicLine) {
		t.Error("API response lost the Arabic transcript")
	}
}

func TestExportMarkdownIsADownload(t *testing.T) {
	f := newFixture(t).seed()

	rec := f.do(http.MethodGet, "/api/notes/"+f.noteID+"/export?format=md", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/markdown") {
		t.Errorf("content type = %q, want text/markdown", ct)
	}
	if cd := rec.Header().Get("Content-Disposition"); !strings.Contains(cd, `filename="maktoob-note-1.md"`) {
		t.Errorf("content disposition = %q", cd)
	}
	if !strings.HasPrefix(rec.Body.String(), "# ") {
		t.Error("markdown export does not start with a heading")
	}
}

func TestExportRejectsUnknownFormat(t *testing.T) {
	f := newFixture(t).seed()

	rec := f.do(http.MethodGet, "/api/notes/"+f.noteID+"/export?format=exe", nil)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

// --- audio ---------------------------------------------------------------

func TestAudioServesTheOriginalWithRangeSupport(t *testing.T) {
	f := newFixture(t).seed()

	rec := f.do(http.MethodGet, "/api/notes/"+f.noteID+"/audio", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "audio/ogg" {
		t.Errorf("content type = %q, want audio/ogg", ct)
	}
	if rec.Body.String() != "OggS-not-really-audio" {
		t.Errorf("body = %q, want the stored media", rec.Body.String())
	}

	// Without Range, seeking inside a long note fails in some browsers.
	req := httptest.NewRequest(http.MethodGet, "/api/notes/"+f.noteID+"/audio", nil)
	req.Header.Set("Range", "bytes=0-3")
	ranged := httptest.NewRecorder()
	f.srv.ServeHTTP(ranged, req)

	if ranged.Code != http.StatusPartialContent {
		t.Errorf("range request status = %d, want 206", ranged.Code)
	}
	if ranged.Body.String() != "OggS" {
		t.Errorf("range body = %q, want the first four bytes", ranged.Body.String())
	}
}

func TestAudioMissingOnDiskIs404(t *testing.T) {
	f := newFixture(t).seed()
	note, _, _ := f.store.GetNote(context.Background(), f.noteID)
	os.Remove(note.MediaPath)

	if rec := f.do(http.MethodGet, "/api/notes/"+f.noteID+"/audio", nil); rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

// --- corrections ---------------------------------------------------------

func TestPatchSegmentStoresAndReturnsTheStoredRow(t *testing.T) {
	f := newFixture(t).seed()

	target := f.segIDs[1]
	rec := f.do(http.MethodPatch, "/api/segments/"+strconv.FormatInt(target, 10),
		strings.NewReader(`{"text":"`+arabicHello+`"}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200\n%s", rec.Code, rec.Body.String())
	}

	var got segmentView
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Text != arabicHello {
		t.Errorf("returned text = %q, want the correction", got.Text)
	}
	if !got.Edited {
		t.Error("returned segment is not marked edited")
	}
	// The edit flag comes from a timestamp the client never sent, so this is
	// what proves the response was read back rather than echoed.
	if got.ID != target {
		t.Errorf("returned segment id = %d, want %d", got.ID, target)
	}

	// And it must survive a reload.
	body := f.do(http.MethodGet, "/note/"+f.noteID, nil).Body.String()
	if !strings.Contains(body, arabicHello) {
		t.Error("correction is not on the page after saving")
	}
	if !strings.Contains(body, English.EditedMark) {
		t.Error("corrected line is not marked as edited")
	}
}

func TestPatchUnknownSegmentIs404(t *testing.T) {
	f := newFixture(t).seed()

	rec := f.do(http.MethodPatch, "/api/segments/999999", strings.NewReader(`{"text":"x"}`))
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

func TestPatchRejectsBadInput(t *testing.T) {
	f := newFixture(t).seed()

	if rec := f.do(http.MethodPatch, "/api/segments/notanumber",
		strings.NewReader(`{"text":"x"}`)); rec.Code != http.StatusBadRequest {
		t.Errorf("non-numeric id status = %d, want 400", rec.Code)
	}
	if rec := f.do(http.MethodPatch, "/api/segments/"+strconv.FormatInt(f.segIDs[0], 10),
		strings.NewReader(`not json`)); rec.Code != http.StatusBadRequest {
		t.Errorf("malformed body status = %d, want 400", rec.Code)
	}
}

// --- origin guard --------------------------------------------------------

// This interface has no authentication because there is no account to
// authenticate. That makes the origin check the only thing standing between a
// tab the user opened and a page that writes to their transcripts.
func TestStateChangingRequestsRefuseOtherOrigins(t *testing.T) {
	f := newFixture(t).seed()
	segPath := "/api/segments/" + strconv.FormatInt(f.segIDs[0], 10)

	for _, tc := range []struct {
		name    string
		headers map[string]string
	}{
		{"cross-site fetch metadata", map[string]string{"Sec-Fetch-Site": "cross-site"}},
		{"same-site fetch metadata", map[string]string{"Sec-Fetch-Site": "same-site"}},
		{"foreign origin header", map[string]string{"Origin": "https://evil.example"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPatch, segPath, strings.NewReader(`{"text":"pwned"}`))
			for k, v := range tc.headers {
				req.Header.Set(k, v)
			}
			rec := httptest.NewRecorder()
			f.srv.ServeHTTP(rec, req)

			if rec.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want 403", rec.Code)
			}
		})
	}

	// And nothing was written.
	_, segs, err := f.store.GetNote(context.Background(), f.noteID)
	if err != nil {
		t.Fatalf("get note: %v", err)
	}
	if segs[0].Edited() {
		t.Error("a refused cross-origin request still modified the transcript")
	}
}

func TestSameOriginAndToollessRequestsAreAllowed(t *testing.T) {
	f := newFixture(t).seed()
	segPath := "/api/segments/" + strconv.FormatInt(f.segIDs[0], 10)

	// curl and scripts send neither header, and driving this API from a shell
	// is a supported thing to do.
	req := httptest.NewRequest(http.MethodPatch, segPath, strings.NewReader(`{"text":"from a script"}`))
	rec := httptest.NewRecorder()
	f.srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("header-less request status = %d, want 200", rec.Code)
	}

	// A same-origin Origin header must also pass.
	req = httptest.NewRequest(http.MethodPatch, segPath, strings.NewReader(`{"text":"from the page"}`))
	req.Header.Set("Origin", "http://"+req.Host)
	rec = httptest.NewRecorder()
	f.srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("same-origin request status = %d, want 200", rec.Code)
	}
}

// --- import --------------------------------------------------------------

func TestImportWithoutAPipelineIsRefusedNotCrashed(t *testing.T) {
	f := newFixture(t)

	body, contentType := multipartAudio(t, "note.ogg", "OggS-fake")
	req := httptest.NewRequest(http.MethodPost, "/import", body)
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	rec := httptest.NewRecorder()
	f.srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", rec.Code)
	}
}

func TestImportRefusesOtherOrigins(t *testing.T) {
	f := newFixture(t)

	body, contentType := multipartAudio(t, "note.ogg", "OggS-fake")
	req := httptest.NewRequest(http.MethodPost, "/import", body)
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("Origin", "https://evil.example")
	rec := httptest.NewRecorder()
	f.srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", rec.Code)
	}
}

func multipartAudio(t *testing.T, name, content string) (io.Reader, string) {
	t.Helper()
	var buf strings.Builder
	w := multipart.NewWriter(&buf)
	part, err := w.CreateFormFile("audio", name)
	if err != nil {
		t.Fatalf("create form file: %v", err)
	}
	if _, err := io.WriteString(part, content); err != nil {
		t.Fatalf("write part: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close writer: %v", err)
	}
	return strings.NewReader(buf.String()), w.FormDataContentType()
}

// --- static and headers --------------------------------------------------

func TestStaticAssetsAreEmbedded(t *testing.T) {
	f := newFixture(t)

	for _, path := range []string{"/static/app.css", "/static/app.js"} {
		rec := f.do(http.MethodGet, path, nil)
		if rec.Code != http.StatusOK {
			t.Errorf("%s status = %d, want 200", path, rec.Code)
		}
		if rec.Body.Len() == 0 {
			t.Errorf("%s served an empty body", path)
		}
	}
}

// The privacy claim is "nothing leaves this machine". A policy that forbids
// every remote origin makes that checkable in devtools rather than trusted.
func TestPagesForbidRemoteOrigins(t *testing.T) {
	f := newFixture(t).seed()

	csp := f.do(http.MethodGet, "/", nil).Header().Get("Content-Security-Policy")
	if csp == "" {
		t.Fatal("no content security policy on the page")
	}
	for _, want := range []string{"default-src 'self'", "connect-src 'self'", "frame-ancestors 'none'"} {
		if !strings.Contains(csp, want) {
			t.Errorf("policy is missing %q: %s", want, csp)
		}
	}
}

// --- helpers -------------------------------------------------------------

func TestSafeExtFallsBackForUnknownTypes(t *testing.T) {
	if safeExt(".ogg") != true {
		t.Error("ogg should be allowed")
	}
	for _, ext := range []string{".sh", ".go", "", ".exe", "../../etc/passwd"} {
		if safeExt(ext) {
			t.Errorf("safeExt(%q) = true, want false", ext)
		}
	}
}

func TestDisplayUploadNameIsNeverAPath(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"note.ogg", "note.ogg"},
		{"../../../etc/passwd", "passwd"},
		{`C:\Users\x\note.ogg`, "note.ogg"},
		{"", "upload"},
	} {
		if got := displayUploadName(tc.in); got != tc.want {
			t.Errorf("displayUploadName(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
	if got := displayUploadName(strings.Repeat("a", 500)); len(got) > 120 {
		t.Errorf("long name was not truncated: %d chars", len(got))
	}
}

func TestParseLimit(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want int
	}{
		{"", 50}, {"abc", 50}, {"0", 50}, {"-3", 50}, {"10", 10}, {"99999", 1000},
	} {
		if got := parseLimit(tc.in, 50); got != tc.want {
			t.Errorf("parseLimit(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

// Both list shapes have to describe a note the same way. A search result
// without chat_id cannot be fed back into the chat filter the same endpoint
// documents, which makes the two halves of one API disagree.
func TestSearchResultsCarryTheSameNoteFieldsAsTheList(t *testing.T) {
	f := newFixture(t).seed()

	list := f.do(http.MethodGet, "/api/notes", nil)
	search := f.do(http.MethodGet, "/api/notes?q="+arabicHello, nil)

	var listed struct {
		Notes []map[string]any `json:"notes"`
	}
	var found struct {
		Results []struct {
			Note map[string]any `json:"note"`
		} `json:"results"`
	}
	if err := json.Unmarshal(list.Body.Bytes(), &listed); err != nil {
		t.Fatalf("unmarshal list: %v", err)
	}
	if err := json.Unmarshal(search.Body.Bytes(), &found); err != nil {
		t.Fatalf("unmarshal search: %v", err)
	}
	if len(listed.Notes) == 0 || len(found.Results) == 0 {
		t.Fatal("fixture produced no note in one of the two shapes")
	}

	for key, want := range listed.Notes[0] {
		got, ok := found.Results[0].Note[key]
		if !ok {
			t.Errorf("search result is missing %q, which the list includes", key)
			continue
		}
		if got != want {
			t.Errorf("%q = %v in search, %v in list", key, got, want)
		}
	}
}

// An empty segment list means four different things. Saying "no speech was
// detected" while a note is still queued tells a deaf user their voice note was
// silent when in fact nothing has run yet — and with JavaScript off it says so
// permanently. This is the product's core promise failing to its worst default.
func TestEmptyTranscriptSaysWhichKindOfEmptyItIs(t *testing.T) {
	ctx := context.Background()

	for _, tc := range []struct {
		status  string
		want    string
		mustNot string
	}{
		{store.StatusPending, English.StillTranscribing, English.NoSpeech},
		{store.StatusConverting, English.StillTranscribing, English.NoSpeech},
		{store.StatusTranscribing, English.StillTranscribing, English.NoSpeech},
		{store.StatusFailed, English.CouldNotTranscribe, English.NoSpeech},
		{store.StatusNoSpeech, English.NoSpeech, English.StillTranscribing},
	} {
		t.Run(tc.status, func(t *testing.T) {
			f := newFixture(t)
			if err := f.store.UpsertChat(ctx, "chat-1", "Umm Ahmad"); err != nil {
				t.Fatalf("upsert: %v", err)
			}
			ok, err := f.store.CreateNote(ctx, store.Note{
				ID: "n1", ChatID: "chat-1", Source: "whatsapp",
				MediaPath:  filepath.Join(f.dir, "n1.ogg"),
				ReceivedAt: time.Now(), Status: tc.status,
			})
			if err != nil || !ok {
				t.Fatalf("create: %v", err)
			}

			body := f.do(http.MethodGet, "/note/n1", nil).Body.String()
			if !strings.Contains(body, tc.want) {
				t.Errorf("a %s note does not say %q", tc.status, tc.want)
			}
			if strings.Contains(body, tc.mustNot) {
				t.Errorf("a %s note wrongly says %q", tc.status, tc.mustNot)
			}
		})
	}
}

// Seeking and correcting are script-driven. Rendered visible without
// JavaScript they are buttons that focus, take a keypress and do nothing — on a
// forty-line transcript, eighty dead tab stops before the export links.
func TestScriptedControlsShipHidden(t *testing.T) {
	f := newFixture(t).seed()

	body := f.do(http.MethodGet, "/note/"+f.noteID, nil).Body.String()
	if !strings.Contains(body, "data-js-only hidden") {
		t.Error("the seek control is not hidden for readers without JavaScript")
	}
}
