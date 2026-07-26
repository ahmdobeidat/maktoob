package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/ahmdobeidat/maktoob/internal/asr"
	"github.com/ahmdobeidat/maktoob/internal/audio"
	"github.com/ahmdobeidat/maktoob/internal/pipeline"
	"github.com/ahmdobeidat/maktoob/internal/store"
	"github.com/ahmdobeidat/maktoob/internal/wa"
)

// open wires the pipeline. Every command needs the store; only import needs
// inference, but building it is free and keeps the wiring in one place.
func open(ctx context.Context, cfg config) (*store.Store, *pipeline.Pipeline, error) {
	if err := cfg.ensureDataDir(); err != nil {
		return nil, nil, err
	}

	st, err := store.Open(cfg.dbPath())
	if err != nil {
		return nil, nil, err
	}

	// Verified on every command, not only the WhatsApp ones. A mismatched salt
	// does not corrupt anything at pair time — it corrupts the next note that
	// gets aliased under it, so the check belongs wherever the database is
	// opened.
	salt, err := wa.LoadSalt(cfg.saltPath())
	if err != nil {
		st.Close()
		return nil, nil, err
	}
	if err := checkSalt(ctx, st, salt); err != nil {
		st.Close()
		return nil, nil, err
	}

	client := asr.NewClient(cfg.asrURL)
	if cfg.lang == "auto" {
		client.Language = ""
	} else if cfg.lang != "" {
		client.Language = cfg.lang
	}

	return st, &pipeline.Pipeline{
		Store:     st,
		Converter: audio.FFmpeg{},
		ASR:       client,
		Detector:  asr.NewDetector(),
		MediaDir:  cfg.mediaDir(),
		Model:     cfg.model,
	}, nil
}

func cmdImport(ctx context.Context, cfg config, files []string) error {
	if len(files) == 0 {
		return errors.New("import requires at least one file")
	}

	st, pl, err := open(ctx, cfg)
	if err != nil {
		return err
	}
	defer st.Close()

	// Recover anything a previous run left mid-flight before adding more.
	requeued, failed, err := st.RequeueStale(ctx)
	if err != nil {
		return err
	}
	if requeued > 0 || failed > 0 {
		fmt.Printf("recovered %d interrupted note(s), %d exceeded retries\n", requeued, failed)
	}

	ids := make([]string, 0, len(files))
	for _, f := range files {
		id, err := pl.Ingest(ctx, f)
		if err != nil {
			return fmt.Errorf("%s: %w", f, err)
		}
		ids = append(ids, id)
		fmt.Printf("queued %s  %s\n", id, f)
	}

	start := time.Now()
	if err := pl.Drain(ctx); err != nil {
		return err
	}
	fmt.Printf("\nprocessed %d note(s) in %s\n\n", len(ids), time.Since(start).Round(time.Millisecond))

	for _, id := range ids {
		if err := printNote(ctx, st, id); err != nil {
			return err
		}
	}
	return nil
}

func cmdList(ctx context.Context, cfg config) error {
	st, _, err := open(ctx, cfg)
	if err != nil {
		return err
	}
	defer st.Close()

	notes, err := st.ListNotes(ctx, 50)
	if err != nil {
		return err
	}
	if len(notes) == 0 {
		fmt.Println("no notes yet")
		return nil
	}

	for _, n := range notes {
		fmt.Printf("%s  %-11s  %6.1fs  %s\n",
			n.ID, n.Status, float64(n.DurationMS)/1000, n.ReceivedAt.Format(time.RFC3339))
	}
	return nil
}

func cmdShow(ctx context.Context, cfg config, id string) error {
	st, _, err := open(ctx, cfg)
	if err != nil {
		return err
	}
	defer st.Close()
	return printNote(ctx, st, id)
}

// printNote renders a transcript for the terminal.
//
// Confidence and suspicion are printed as words rather than colours: this is
// the same commitment the web interface makes, and the reason is the same. A
// marker that only exists as a colour is unavailable to a reader who cannot
// distinguish it, which for an accessibility tool is self-defeating.
func printNote(ctx context.Context, st *store.Store, id string) error {
	note, segs, err := st.GetNote(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		return fmt.Errorf("no note with id %s", id)
	}
	if err != nil {
		return err
	}

	fmt.Printf("note   %s\n", note.ID)
	fmt.Printf("status %s", note.Status)
	if note.Error != "" {
		fmt.Printf("  (%s)", note.Error)
	}
	fmt.Printf("\nlength %.1fs\n\n", float64(note.DurationMS)/1000)

	if len(segs) == 0 {
		fmt.Println("  (no speech detected)")
		fmt.Println()
		return nil
	}

	for _, s := range segs {
		confidence := expConfidence(s.AvgLogprob)
		marker := "   "
		switch {
		case s.Suspect:
			marker = "!! "
		case confidence < 0.5:
			marker = " ? "
		}
		fmt.Printf("%s[%6.2f-%6.2f] conf %.2f  nsp %.3f  %s\n",
			marker,
			float64(s.StartMS)/1000, float64(s.EndMS)/1000,
			confidence, s.NoSpeechProb, s.Text())
	}

	fmt.Println("\n  ' ? ' low confidence      '!! ' possible fabrication")
	fmt.Println()
	return nil
}

func expConfidence(avgLogprob float64) float64 {
	s := asr.Segment{AvgLogprob: avgLogprob}
	return s.Confidence()
}

const saltFingerprintKey = "alias-salt-fingerprint"

// checkSalt refuses to run against a database that was built with a different
// alias salt.
//
// Aliases are derived from the salt, so a lost or swapped salt does not fail
// loudly: every chat quietly forks into a new row, the old rows keep their
// display names, and the user sees each conversation twice with nothing to
// explain it. Catching it here turns a silent data problem into a startup error.
func checkSalt(ctx context.Context, st *store.Store, salt wa.Salt) error {
	want := salt.Fingerprint()

	got, err := st.GetMeta(ctx, saltFingerprintKey)
	if errors.Is(err, store.ErrNotFound) {
		return st.SetMeta(ctx, saltFingerprintKey, want)
	}
	if err != nil {
		return err
	}
	if got != want {
		return fmt.Errorf(
			"the alias salt does not match this database: data/salt has been replaced or lost.\n" +
				"Restore the original data/salt, or start a new database, " +
				"because chats aliased under a different salt cannot be matched to the existing ones")
	}
	return nil
}

func cmdPair(ctx context.Context, cfg config) error {
	client, err := wa.Connect(ctx, cfg.sessionPath(), slog.Default())
	if err != nil {
		return err
	}
	defer client.Disconnect()

	return wa.Pair(ctx, client, os.Stdout)
}

func cmdLogout(ctx context.Context, cfg config) error {
	client, err := wa.Connect(ctx, cfg.sessionPath(), slog.Default())
	if err != nil {
		return err
	}
	defer client.Disconnect()

	if client.Store.ID == nil {
		return wa.ErrNotPaired
	}
	if err := client.Connect(); err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	if err := wa.Logout(ctx, client); err != nil {
		return err
	}

	fmt.Println("Unlinked. Transcripts already stored on this machine are untouched.")
	return nil
}
