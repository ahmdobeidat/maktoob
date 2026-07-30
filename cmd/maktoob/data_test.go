package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ahmdobeidat/maktoob/internal/store"
)

func testConfig(t *testing.T) config {
	t.Helper()
	return config{
		dataDir: t.TempDir(),
		asrURL:  "http://127.0.0.1:1",
		model:   "test-model",
		lang:    "ar",
	}
}

// seedStore creates the data directory the way a real run would, with one note.
func seedStore(t *testing.T, cfg config) string {
	t.Helper()
	ctx := context.Background()

	st, _, err := open(ctx, cfg)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer st.Close()

	if err := os.MkdirAll(cfg.mediaDir(), 0o700); err != nil {
		t.Fatalf("media dir: %v", err)
	}
	media := filepath.Join(cfg.mediaDir(), "n1.ogg")
	if err := os.WriteFile(media, []byte("OggS"), 0o600); err != nil {
		t.Fatalf("write media: %v", err)
	}

	if err := st.UpsertChat(ctx, "chat-1", "Umm Ahmad"); err != nil {
		t.Fatalf("upsert chat: %v", err)
	}
	ok, err := st.CreateNote(ctx, store.Note{
		ID: "n1", ChatID: "chat-1", Source: "import", SenderName: "Ahmad",
		MediaPath: media, DurationMS: 4000, ReceivedAt: time.Now(),
		Status: store.StatusDone,
	})
	if err != nil || !ok {
		t.Fatalf("create note: %v (ok=%v)", err, ok)
	}
	// Written as escapes rather than glyphs: bidi text reorders source visually,
	// which makes a failing assertion on this line unreadable.
	const marhaba = "\u0645\u0631\u062D\u0628\u0627" // marhaba
	if err := st.ReplaceSegments(ctx, "n1", []store.Segment{
		{Idx: 0, StartMS: 0, EndMS: 4000, ASRText: marhaba, AvgLogprob: -0.1},
	}); err != nil {
		t.Fatalf("segments: %v", err)
	}
	return "n1"
}

// --- export --------------------------------------------------------------

func TestCmdExportWritesBothFormats(t *testing.T) {
	cfg := testConfig(t)
	id := seedStore(t, cfg)
	ctx := context.Background()

	var jsonOut strings.Builder
	if err := cmdExport(ctx, cfg, id, "json", &jsonOut); err != nil {
		t.Fatalf("export json: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal([]byte(jsonOut.String()), &doc); err != nil {
		t.Fatalf("export json is not valid JSON: %v", err)
	}
	if doc["id"] != id {
		t.Errorf("exported id = %v, want %q", doc["id"], id)
	}

	var mdOut strings.Builder
	if err := cmdExport(ctx, cfg, id, "md", &mdOut); err != nil {
		t.Fatalf("export md: %v", err)
	}
	if !strings.HasPrefix(mdOut.String(), "# ") {
		t.Errorf("markdown export does not start with a heading: %q", mdOut.String())
	}
}

func TestCmdExportRejectsBadInput(t *testing.T) {
	cfg := testConfig(t)
	id := seedStore(t, cfg)
	ctx := context.Background()

	var out strings.Builder
	if err := cmdExport(ctx, cfg, id, "pdf", &out); err == nil {
		t.Error("unknown format was accepted")
	}
	if err := cmdExport(ctx, cfg, "no-such-note", "json", &out); err == nil {
		t.Error("unknown note id was accepted")
	}
}

// --- purge ---------------------------------------------------------------

func TestPurgeDeletesEverythingItListed(t *testing.T) {
	cfg := testConfig(t)
	seedStore(t, cfg)

	// A file the user put in the data directory themselves. purge must not
	// touch it, because -data can point at a directory holding other things.
	foreign := filepath.Join(cfg.dataDir, "my-notes.txt")
	if err := os.WriteFile(foreign, []byte("mine"), 0o600); err != nil {
		t.Fatalf("write foreign file: %v", err)
	}

	targets := purgeTargets(cfg)
	if len(targets) == 0 {
		t.Fatal("purge found nothing to delete in a seeded data directory")
	}

	var out strings.Builder
	if err := cmdPurge(context.Background(), cfg, true, strings.NewReader(""), &out); err != nil {
		t.Fatalf("purge: %v", err)
	}

	for _, target := range targets {
		if _, err := os.Stat(target.path); !os.IsNotExist(err) {
			t.Errorf("%s survived purge", target.path)
		}
	}
	if _, err := os.Stat(foreign); err != nil {
		t.Errorf("purge deleted a file it did not create: %v", err)
	}
}

// The salt and the database have to go together. A salt left behind next to a
// deleted database is the one artifact that could still re-link old aliases to
// real contacts; a database left without its salt makes every chat fork on the
// next run.
func TestPurgeRemovesTheSaltAndTheDatabaseTogether(t *testing.T) {
	cfg := testConfig(t)
	seedStore(t, cfg)

	for _, path := range []string{cfg.saltPath(), cfg.dbPath()} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("fixture is missing %s: %v", path, err)
		}
	}

	var out strings.Builder
	if err := cmdPurge(context.Background(), cfg, true, strings.NewReader(""), &out); err != nil {
		t.Fatalf("purge: %v", err)
	}

	for _, path := range []string{cfg.saltPath(), cfg.dbPath()} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("%s survived purge", path)
		}
	}
}

// The salt fingerprint is stored in the database's meta table and checked on
// every startup. If purge removed the salt but left the row, or the row but
// left the salt, the next launch would refuse to start.
func TestMaktoobStartsCleanlyAfterPurge(t *testing.T) {
	cfg := testConfig(t)
	seedStore(t, cfg)
	ctx := context.Background()

	var out strings.Builder
	if err := cmdPurge(ctx, cfg, true, strings.NewReader(""), &out); err != nil {
		t.Fatalf("purge: %v", err)
	}

	st, _, err := open(ctx, cfg)
	if err != nil {
		t.Fatalf("maktoob will not start after a purge: %v", err)
	}
	defer st.Close()

	notes, err := st.ListNotes(ctx, 10)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(notes) != 0 {
		t.Errorf("a purged install still has %d notes", len(notes))
	}
}

func TestPurgeWithoutConfirmationDeletesNothing(t *testing.T) {
	cfg := testConfig(t)
	seedStore(t, cfg)

	for _, answer := range []string{"no", "y", "YES please", "", "  "} {
		t.Run("answer_"+answer, func(t *testing.T) {
			var out strings.Builder
			if err := cmdPurge(context.Background(), cfg, false,
				strings.NewReader(answer+"\n"), &out); err != nil {
				t.Fatalf("purge: %v", err)
			}
			if _, err := os.Stat(cfg.dbPath()); err != nil {
				t.Errorf("answering %q still deleted the database", answer)
			}
			if !strings.Contains(out.String(), "Nothing was deleted") {
				t.Errorf("purge did not say it stopped:\n%s", out.String())
			}
		})
	}
}

func TestPurgeAcceptsTheWholeWord(t *testing.T) {
	cfg := testConfig(t)
	seedStore(t, cfg)

	var out strings.Builder
	if err := cmdPurge(context.Background(), cfg, false,
		strings.NewReader(" Yes \n"), &out); err != nil {
		t.Fatalf("purge: %v", err)
	}
	if _, err := os.Stat(cfg.dbPath()); !os.IsNotExist(err) {
		t.Error("confirmed purge did not delete the database")
	}
}

func TestPurgeWarnsAboutWhatItCannotUndo(t *testing.T) {
	cfg := testConfig(t)
	seedStore(t, cfg)

	var out strings.Builder
	if err := cmdPurge(context.Background(), cfg, true, strings.NewReader(""), &out); err != nil {
		t.Fatalf("purge: %v", err)
	}
	text := out.String()

	// Deleting the local session does not tell WhatsApp anything. A user who
	// believes purge unlinked their device leaves a live linked device on their
	// account, which is a worse outcome than not running purge at all.
	if !strings.Contains(text, "does not unlink") {
		t.Errorf("purge did not say it leaves the device linked:\n%s", text)
	}
	if !strings.Contains(text, "cannot be recovered") {
		t.Errorf("purge did not say the deletion is permanent:\n%s", text)
	}
}

func TestPurgeOnAnEmptyDirectorySaysSo(t *testing.T) {
	cfg := testConfig(t)

	var out strings.Builder
	if err := cmdPurge(context.Background(), cfg, true, strings.NewReader(""), &out); err != nil {
		t.Fatalf("purge: %v", err)
	}
	if !strings.Contains(out.String(), "Nothing to delete") {
		t.Errorf("purge on an empty directory said:\n%s", out.String())
	}
}

func TestPurgeTargetsListOnlyWhatExists(t *testing.T) {
	cfg := testConfig(t)

	if got := purgeTargets(cfg); len(got) != 0 {
		t.Fatalf("empty directory reported %d targets", len(got))
	}

	seedStore(t, cfg)
	got := purgeTargets(cfg)

	found := make(map[string]bool)
	for _, target := range got {
		found[filepath.Base(target.path)] = true
		if _, err := os.Stat(target.path); err != nil {
			t.Errorf("listed a target that does not exist: %s", target.path)
		}
		if target.label == "" {
			t.Errorf("%s is listed with no explanation of what it is", target.path)
		}
	}
	for _, want := range []string{"maktoob.db", "salt", "media"} {
		if !found[want] {
			t.Errorf("purge would not delete %s", want)
		}
	}
}
