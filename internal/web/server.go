// Package web serves maktoob's local interface.
//
// Everything here binds to the loopback interface and has no authentication,
// because there is no account model to authenticate against: this is one
// person's transcripts on one person's machine. That decision is what makes the
// origin checks in this file load-bearing rather than decorative. A page on the
// open web cannot read a localhost response, but it can *send* a request, so
// every state-changing route verifies the request came from this interface.
package web

import (
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/ahmdobeidat/maktoob/internal/export"
	"github.com/ahmdobeidat/maktoob/internal/pipeline"
	"github.com/ahmdobeidat/maktoob/internal/store"
)

//go:embed templates/*.html static/*
var assets embed.FS

// maxUploadBytes caps an imported file. Voice notes are tens of kilobytes; the
// cap is generous for a long recording and still small enough that a stray
// upload cannot fill the disk before the handler notices.
const maxUploadBytes = 64 << 20 // 64 MiB

// maxCorrectionBytes caps a single segment correction. A segment is one spoken
// line, so anything approaching this is not a correction.
const maxCorrectionBytes = 16 << 10

// defaultListLimit is how many notes the list shows before the user filters.
const defaultListLimit = 100

// Options configures a Server.
type Options struct {
	Store    *store.Store
	Pipeline *pipeline.Pipeline
	Broker   *Broker
	Logger   *slog.Logger
	Locale   *Locale

	// WhatsAppState, when set, reports the listener's current state for the
	// header badge. It is a function rather than a value because the state
	// changes underneath the page, and nil when maktoob runs without WhatsApp.
	WhatsAppState func() string
}

// Server is the HTTP interface. It is safe for concurrent use.
type Server struct {
	store    *store.Store
	pipe     *pipeline.Pipeline
	broker   *Broker
	log      *slog.Logger
	loc      Locale
	waState  func() string
	listTmpl *template.Template
	noteTmpl *template.Template
	mux      *http.ServeMux
}

// New builds the server and parses its templates.
//
// Templates are parsed once at startup rather than per request: a syntax error
// then fails the process at launch instead of rendering a broken page to the
// one user who was about to demo it.
func New(opts Options) (*Server, error) {
	if opts.Store == nil {
		return nil, errors.New("web: Store is required")
	}
	s := &Server{
		store:   opts.Store,
		pipe:    opts.Pipeline,
		broker:  opts.Broker,
		log:     opts.Logger,
		loc:     English,
		waState: opts.WhatsAppState,
	}
	if opts.Locale != nil {
		s.loc = *opts.Locale
	}
	if s.log == nil {
		s.log = slog.New(slog.DiscardHandler)
	}
	if s.broker == nil {
		s.broker = NewBroker()
	}

	// Each page gets its own template set. A single set cannot hold two
	// definitions of "content", and separate sets are simpler than renaming the
	// block per page.
	var err error
	if s.listTmpl, err = parsePage("list.html"); err != nil {
		return nil, err
	}
	if s.noteTmpl, err = parsePage("note.html"); err != nil {
		return nil, err
	}

	s.routes()
	return s, nil
}

func parsePage(page string) (*template.Template, error) {
	t, err := template.New("layout.html").Funcs(funcMap()).ParseFS(assets,
		"templates/layout.html", "templates/"+page)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", page, err)
	}
	return t, nil
}

// Broker returns the event broker, so the caller can publish arrivals from the
// WhatsApp adapter without the web package importing it.
func (s *Server) Broker() *Broker { return s.broker }

func (s *Server) routes() {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /{$}", s.handleList)
	mux.HandleFunc("GET /note/{id}", s.handleNotePage)
	mux.HandleFunc("GET /events", s.handleEvents)

	mux.HandleFunc("GET /api/notes", s.handleAPINotes)
	mux.HandleFunc("GET /api/notes/{id}", s.handleAPINote)
	mux.HandleFunc("GET /api/notes/{id}/audio", s.handleAudio)
	mux.HandleFunc("GET /api/notes/{id}/export", s.handleExport)
	mux.HandleFunc("PATCH /api/segments/{id}", s.guard(s.handlePatchSegment))
	// The form-encoded twin of the PATCH above, for browsers with the script
	// blocked. It exists so that "every page works without JavaScript" is a
	// true statement about this interface rather than an aspiration.
	mux.HandleFunc("POST /segments/{id}", s.guard(s.handleFormSegment))
	mux.HandleFunc("POST /import", s.guard(s.handleImport))

	mux.Handle("GET /static/", http.FileServerFS(assets))

	s.mux = mux
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mux.ServeHTTP(w, r)
}

// guard rejects state-changing requests that did not originate from this
// interface.
//
// Sec-Fetch-Site is checked first because every current browser sends it and it
// answers the question directly. Origin is the fallback. A request with neither
// header is allowed through: that is curl or a script the user ran themselves,
// which is a supported way to drive this API, and no browser reaches this point
// without sending at least one of the two.
func (s *Server) guard(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !sameOrigin(r) {
			s.log.Warn("rejected cross-origin request",
				"path", r.URL.Path, "origin", r.Header.Get("Origin"))
			http.Error(w, "cross-origin request refused", http.StatusForbidden)
			return
		}
		next(w, r)
	}
}

func sameOrigin(r *http.Request) bool {
	switch r.Header.Get("Sec-Fetch-Site") {
	case "same-origin", "none":
		return true
	case "cross-site", "same-site":
		return false
	}

	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	if err != nil {
		return false
	}
	return u.Host == r.Host
}

// --- HTML pages ---------------------------------------------------------

type pageData struct {
	Loc      Locale
	Title    string
	Query    string
	ChatID   string
	Chats    []store.Chat
	Notes    []noteView
	Hits     []hitGroup
	Note     *noteView
	Segments []segmentView
	WAState  string
	HasAudio bool
}

func (s *Server) handleList(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	query := strings.TrimSpace(r.URL.Query().Get("q"))
	chatID := r.URL.Query().Get("chat")

	data := pageData{
		Loc:     s.loc,
		Title:   s.loc.NotesHeading,
		Query:   query,
		ChatID:  chatID,
		WAState: s.whatsAppState(),
	}

	chats, err := s.store.ListChats(ctx)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	data.Chats = chats

	if query != "" {
		hits, err := s.store.Search(ctx, query, 200)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		data.Hits = groupHits(hits, chatID, s.loc)
	} else {
		notes, err := s.store.ListNotesByChat(ctx, chatID, defaultListLimit)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		for _, n := range notes {
			data.Notes = append(data.Notes, newNoteView(n, s.loc))
		}
	}

	s.render(w, r, s.listTmpl, data)
}

func (s *Server) handleNotePage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := r.PathValue("id")

	note, segs, err := s.store.GetNote(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		s.notFound(w, r)
		return
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}

	view := newNoteView(note, s.loc)
	data := pageData{
		Loc:      s.loc,
		Title:    s.loc.NoteHeading,
		Note:     &view,
		Segments: newSegmentViews(segs, s.loc),
		WAState:  s.whatsAppState(),
		HasAudio: fileExists(note.MediaPath),
	}
	s.render(w, r, s.noteTmpl, data)
}

func (s *Server) render(w http.ResponseWriter, r *http.Request, t *template.Template, data pageData) {
	// Rendered into memory first. A template that fails halfway through has
	// already written a 200 and half a page, and the user sees a truncated
	// transcript rather than an error.
	var buf strings.Builder
	if err := t.ExecuteTemplate(&buf, "layout.html", data); err != nil {
		s.fail(w, r, fmt.Errorf("render: %w", err))
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Referrer-Policy", "no-referrer")
	// No remote origins are permitted at all. Everything this page needs is
	// embedded in the binary, so a directive that blocks the network entirely
	// costs nothing and makes the privacy claim checkable in devtools.
	w.Header().Set("Content-Security-Policy",
		"default-src 'self'; media-src 'self'; img-src 'self' data:; "+
			"style-src 'self'; script-src 'self'; connect-src 'self'; "+
			"form-action 'self'; frame-ancestors 'none'; base-uri 'none'")
	io.WriteString(w, buf.String())
}

// --- JSON API -----------------------------------------------------------

func (s *Server) handleAPINotes(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	query := strings.TrimSpace(r.URL.Query().Get("q"))
	chatID := r.URL.Query().Get("chat")
	limit := parseLimit(r.URL.Query().Get("limit"), defaultListLimit)

	if query != "" {
		hits, err := s.store.Search(ctx, query, limit)
		if err != nil {
			s.failJSON(w, r, err)
			return
		}
		groups := groupHits(hits, chatID, s.loc)
		out := make([]apiHitGroup, 0, len(groups))
		for _, g := range groups {
			out = append(out, newAPIHitGroup(g))
		}
		writeJSON(w, http.StatusOK, map[string]any{"query": query, "results": out})
		return
	}

	notes, err := s.store.ListNotesByChat(ctx, chatID, limit)
	if err != nil {
		s.failJSON(w, r, err)
		return
	}
	out := make([]apiNote, 0, len(notes))
	for _, n := range notes {
		out = append(out, newAPINote(n))
	}
	writeJSON(w, http.StatusOK, map[string]any{"notes": out})
}

// handleAPINote returns the same document the JSON export produces. One shape
// for both means a client that can read an export can read the API, and the
// documentation only has to describe it once.
func (s *Server) handleAPINote(w http.ResponseWriter, r *http.Request) {
	note, segs, err := s.store.GetNote(r.Context(), r.PathValue("id"))
	if errors.Is(err, store.ErrNotFound) {
		s.notFoundJSON(w)
		return
	}
	if err != nil {
		s.failJSON(w, r, err)
		return
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if err := export.JSON(w, note, segs); err != nil {
		s.log.Error("write note json", "note", note.ID, "err", err)
	}
}

func (s *Server) handleExport(w http.ResponseWriter, r *http.Request) {
	format, err := export.ParseFormat(r.URL.Query().Get("format"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}

	note, segs, err := s.store.GetNote(r.Context(), r.PathValue("id"))
	if errors.Is(err, store.ErrNotFound) {
		s.notFoundJSON(w)
		return
	}
	if err != nil {
		s.failJSON(w, r, err)
		return
	}

	contentType := "application/json; charset=utf-8"
	if format == export.FormatMarkdown {
		contentType = "text/markdown; charset=utf-8"
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Disposition",
		fmt.Sprintf(`attachment; filename="%s"`, export.Filename(note, format)))

	if err := export.Write(w, format, note, segs); err != nil {
		s.log.Error("write export", "note", note.ID, "err", err)
	}
}

// handleAudio serves the original media.
//
// The path comes from the note row, never from the request: the id selects a
// row and the row supplies the path, so there is no user-controlled component
// in the filename and no traversal to defend against. ServeContent is used for
// its Range support, without which seeking inside a long note fails.
func (s *Server) handleAudio(w http.ResponseWriter, r *http.Request) {
	note, _, err := s.store.GetNote(r.Context(), r.PathValue("id"))
	if errors.Is(err, store.ErrNotFound) {
		s.notFoundJSON(w)
		return
	}
	if err != nil {
		s.failJSON(w, r, err)
		return
	}

	f, err := os.Open(note.MediaPath)
	if err != nil {
		s.log.Warn("audio unreadable", "note", note.ID, "err", err)
		http.Error(w, "audio not available", http.StatusNotFound)
		return
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		s.failJSON(w, r, err)
		return
	}

	w.Header().Set("Content-Type", audioContentType(note.MediaPath))
	w.Header().Set("Cache-Control", "private, max-age=3600")
	http.ServeContent(w, r, filepath.Base(note.MediaPath), info.ModTime(), f)
}

type patchSegmentRequest struct {
	Text string `json:"text"`
}

func (s *Server) handlePatchSegment(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad segment id"})
		return
	}

	var req patchSegmentRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, maxCorrectionBytes)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad request body"})
		return
	}

	if err := s.store.EditSegment(ctx, id, req.Text); errors.Is(err, store.ErrNotFound) {
		s.notFoundJSON(w)
		return
	} else if err != nil {
		s.failJSON(w, r, err)
		return
	}

	// Read the row back rather than echoing the request. The client then
	// renders what was stored, including the edit timestamp it did not send.
	seg, err := s.store.GetSegment(ctx, id)
	if err != nil {
		s.failJSON(w, r, err)
		return
	}

	// Other open tabs are showing the old text until they hear about this.
	s.broker.Publish(Event{Kind: KindUpdated, NoteID: seg.NoteID})

	writeJSON(w, http.StatusOK, newSegmentView(seg, s.loc))
}

// handleFormSegment saves a correction submitted as an ordinary HTML form and
// sends the reader back to the line they edited.
//
// The redirect target is derived from the segment's own row, never from the
// request. A "return to" parameter would have been convenient and would have
// been an open redirect.
func (s *Server) handleFormSegment(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "bad segment id", http.StatusBadRequest)
		return
	}

	if err := r.ParseForm(); err != nil {
		http.Error(w, "could not read the form", http.StatusBadRequest)
		return
	}

	// Read the segment first, so a failed edit still knows which page to return
	// to and the reader is not dropped on an error screen with no way back.
	seg, err := s.store.GetSegment(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		s.notFound(w, r)
		return
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}

	text := r.PostFormValue("text")
	if len(text) > maxCorrectionBytes {
		text = text[:maxCorrectionBytes]
	}

	if err := s.store.EditSegment(ctx, id, text); err != nil && !errors.Is(err, store.ErrNotFound) {
		s.fail(w, r, err)
		return
	}

	s.broker.Publish(Event{Kind: KindUpdated, NoteID: seg.NoteID})

	http.Redirect(w, r,
		"/note/"+url.PathEscape(seg.NoteID)+"#seg-"+strconv.FormatInt(id, 10),
		http.StatusSeeOther)
}

func (s *Server) handleImport(w http.ResponseWriter, r *http.Request) {
	if s.pipe == nil {
		writeJSON(w, http.StatusServiceUnavailable,
			map[string]string{"error": "import is not available"})
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxUploadBytes)
	if err := r.ParseMultipartForm(8 << 20); err != nil {
		writeJSON(w, http.StatusBadRequest,
			map[string]string{"error": "could not read the uploaded file"})
		return
	}
	defer r.MultipartForm.RemoveAll()

	file, header, err := r.FormFile("audio")
	if err != nil {
		writeJSON(w, http.StatusBadRequest,
			map[string]string{"error": "no audio file in the request"})
		return
	}
	defer file.Close()

	// filepath.Ext on the client-supplied name is the only thing taken from the
	// upload besides its bytes, and the pipeline uses it to name a file inside
	// MediaDir. Base strips any directory the client tried to smuggle in.
	ext := strings.ToLower(filepath.Ext(filepath.Base(header.Filename)))
	if !safeExt(ext) {
		ext = ".bin"
	}

	id, created, err := s.pipe.IngestReader(r.Context(), file, pipeline.IngestRequest{
		Source:     "import",
		ChatID:     pipeline.ImportChatID,
		ChatName:   "Imported files",
		SenderName: displayUploadName(header.Filename),
		Ext:        ext,
		ReceivedAt: time.Now(),
	})
	if err != nil {
		s.failJSON(w, r, err)
		return
	}

	if created {
		s.broker.Publish(Event{
			Kind:   KindArrived,
			NoteID: id,
			Status: store.StatusPending,
			Text:   s.loc.StatusPending,
		})
	}

	// A browser form post without JavaScript lands on the new note's page, so
	// the reader watches it transcribe rather than hunting for it in the list.
	// The fetch path reads the JSON instead.
	if strings.Contains(r.Header.Get("Accept"), "application/json") {
		writeJSON(w, http.StatusAccepted, map[string]any{"id": id, "created": created})
		return
	}
	http.Redirect(w, r, "/note/"+url.PathEscape(id), http.StatusSeeOther)
}

// --- server-sent events -------------------------------------------------

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Connection", "keep-alive")
	// Without this an intermediary that buffers responses holds every event
	// until the stream closes, which looks exactly like a broken feature.
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	events, cancel := s.broker.Subscribe()
	defer cancel()

	// A comment line every 25 seconds. Browsers and proxies drop an idle stream,
	// and a reconnect storm on an interface that is idle by nature is worse than
	// one byte a minute.
	ping := time.NewTicker(25 * time.Second)
	defer ping.Stop()

	for {
		select {
		case <-r.Context().Done():
			return

		case e, ok := <-events:
			if !ok {
				return
			}
			payload, err := json.Marshal(e)
			if err != nil {
				s.log.Error("marshal event", "err", err)
				continue
			}
			if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", e.Kind, payload); err != nil {
				return
			}
			flusher.Flush()

		case <-ping.C:
			if _, err := io.WriteString(w, ": ping\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

// --- helpers ------------------------------------------------------------

func (s *Server) whatsAppState() string {
	if s.waState == nil {
		return ""
	}
	switch s.waState() {
	case "connected":
		return s.loc.WhatsAppConnected
	case "unpaired", "logged out":
		return s.loc.WhatsAppUnpaired
	default:
		return s.loc.WhatsAppOffline
	}
}

func (s *Server) fail(w http.ResponseWriter, r *http.Request, err error) {
	s.log.Error("request failed", "path", r.URL.Path, "err", err)
	http.Error(w, "something went wrong on this machine", http.StatusInternalServerError)
}

func (s *Server) failJSON(w http.ResponseWriter, r *http.Request, err error) {
	s.log.Error("request failed", "path", r.URL.Path, "err", err)
	writeJSON(w, http.StatusInternalServerError,
		map[string]string{"error": "something went wrong on this machine"})
}

func (s *Server) notFound(w http.ResponseWriter, r *http.Request) {
	http.Error(w, "no such note", http.StatusNotFound)
}

func (s *Server) notFoundJSON(w http.ResponseWriter) {
	writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

func parseLimit(raw string, def int) int {
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return def
	}
	if n > 1000 {
		return 1000
	}
	return n
}

func fileExists(path string) bool {
	if path == "" {
		return false
	}
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

// safeExt allows only the container extensions the converter is expected to
// handle. Anything else is stored as .bin, which ffmpeg still probes by content.
func safeExt(ext string) bool {
	switch ext {
	case ".ogg", ".oga", ".opus", ".m4a", ".mp3", ".mp4", ".wav", ".aac", ".flac", ".webm", ".amr", ".3gp":
		return true
	default:
		return false
	}
}

func audioContentType(path string) string {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".ogg", ".oga", ".opus":
		return "audio/ogg"
	case ".m4a", ".mp4", ".aac":
		return "audio/mp4"
	case ".mp3":
		return "audio/mpeg"
	case ".wav":
		return "audio/wav"
	case ".webm":
		return "audio/webm"
	case ".flac":
		return "audio/flac"
	default:
		return "application/octet-stream"
	}
}

// displayUploadName keeps the uploaded file's name as a label without letting
// it be a path. It is shown, never resolved.
func displayUploadName(name string) string {
	base := filepath.Base(strings.ReplaceAll(name, `\`, "/"))
	if base == "." || base == "/" || base == "" {
		return "upload"
	}
	if len(base) > 120 {
		base = base[:120]
	}
	return base
}
