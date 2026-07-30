package web

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
)

// The script claims every page works with JavaScript blocked. For the
// transcript that was not true: correcting a line existed only as a fetch, so
// the one interaction the project is built around was unavailable to anyone
// running without scripts. This is the form-encoded path that makes the claim
// honest rather than aspirational.
func TestCorrectionWorksWithoutJavaScript(t *testing.T) {
	f := newFixture(t).seed()
	target := f.segIDs[1]
	path := "/segments/" + strconv.FormatInt(target, 10)

	form := strings.NewReader("text=" + url.QueryEscape(arabicHello))
	req := httptest.NewRequest(http.MethodPost, path, form)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	rec := httptest.NewRecorder()
	f.srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303\n%s", rec.Code, rec.Body.String())
	}

	// Back to the exact line that was edited, so the reader is not dumped at
	// the top of a long transcript after every correction.
	want := "/note/" + f.noteID + "#seg-" + strconv.FormatInt(target, 10)
	if got := rec.Header().Get("Location"); got != want {
		t.Errorf("redirected to %q, want %q", got, want)
	}

	body := f.do(http.MethodGet, "/note/"+f.noteID, nil).Body.String()
	if !strings.Contains(body, arabicHello) {
		t.Error("the correction was not saved")
	}
}

// The redirect target is derived from the segment's stored row, never from the
// request. A "return to" parameter would have been convenient and would have
// been an open redirect.
func TestFormCorrectionIgnoresACallerSuppliedRedirect(t *testing.T) {
	f := newFixture(t).seed()
	path := "/segments/" + strconv.FormatInt(f.segIDs[0], 10)

	hostile := "https://" + strings.Join([]string{"attacker", "invalid"}, ".")
	form := strings.NewReader("text=x&return=" + url.QueryEscape(hostile))

	req := httptest.NewRequest(http.MethodPost, path, form)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	rec := httptest.NewRecorder()
	f.srv.ServeHTTP(rec, req)

	loc := rec.Header().Get("Location")
	if strings.Contains(loc, "attacker") {
		t.Errorf("caller steered the redirect to %q", loc)
	}
	if !strings.HasPrefix(loc, "/note/") {
		t.Errorf("redirect = %q, want a path on this interface", loc)
	}
}

func TestFormCorrectionRefusesOtherOrigins(t *testing.T) {
	f := newFixture(t).seed()
	path := "/segments/" + strconv.FormatInt(f.segIDs[0], 10)

	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader("text=overwritten"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	rec := httptest.NewRecorder()
	f.srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}

	body := f.do(http.MethodGet, "/note/"+f.noteID, nil).Body.String()
	if strings.Contains(body, "overwritten") {
		t.Error("a refused cross-origin form post still modified the transcript")
	}
}

// Both halves of the progressive-enhancement pair have to be present in the
// markup: the scripted control hidden until app.js reveals it, and the native
// fallback visible until app.js hides it. Ship only one and the page is either
// broken without scripts or offers the same action twice with them.
func TestBothCorrectionPathsAreRendered(t *testing.T) {
	f := newFixture(t).seed()
	body := f.do(http.MethodGet, "/note/"+f.noteID, nil).Body.String()

	if !strings.Contains(body, `<details class="segment-fallback"`) {
		t.Error("no no-JavaScript correction form on the page")
	}
	if !strings.Contains(body, `action="/segments/`) {
		t.Error("the fallback form does not post anywhere")
	}
	if !strings.Contains(body, `data-js-hidden`) {
		t.Error("the fallback is not marked for the script to hide")
	}
	if !strings.Contains(body, `class="segment-actions" dir="ltr" data-js-only hidden`) {
		t.Error("the scripted edit button is not hidden for readers without JavaScript")
	}
}

// The chat filter puts a name next to a count inside an <option>, whose content
// is text-only so <bdi> is unavailable. Without explicit isolates an Arabic
// chat name reorders and the count lands on the wrong end.
func TestChatFilterIsolatesNameFromCount(t *testing.T) {
	f := newFixture(t).seed()
	body := f.do(http.MethodGet, "/", nil).Body.String()

	const (
		firstStrongIsolate = "⁨"
		popDirectional     = "⁩"
	)
	if !strings.Contains(body, firstStrongIsolate) || !strings.Contains(body, popDirectional) {
		t.Error("chat filter options carry no directional isolates")
	}
}

// Confidence was computed, shipped in the JSON, and never shown to a reader.
// "Low confidence" without the number does not distinguish 45% from 4%, and the
// person reading this cannot check the audio.
func TestFlaggedLinesShowTheConfidenceNumber(t *testing.T) {
	f := newFixture(t).seed()
	body := f.do(http.MethodGet, "/note/"+f.noteID, nil).Body.String()

	if !strings.Contains(body, English.ConfidenceIs) {
		t.Error("a flagged line does not report the model's confidence")
	}
	if !strings.Contains(body, "%") {
		t.Error("confidence is not rendered as a percentage")
	}
}
