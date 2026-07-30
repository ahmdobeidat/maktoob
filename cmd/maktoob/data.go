package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/ahmdobeidat/maktoob/internal/export"
	"github.com/ahmdobeidat/maktoob/internal/store"
)

func cmdExport(ctx context.Context, cfg config, id, format string, out io.Writer) error {
	f, err := export.ParseFormat(format)
	if err != nil {
		return err
	}

	st, _, err := open(ctx, cfg)
	if err != nil {
		return err
	}
	defer st.Close()

	note, segs, err := st.GetNote(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		return fmt.Errorf("no note with id %s", id)
	}
	if err != nil {
		return err
	}

	return export.Write(out, f, note, segs)
}

// purgeTarget is one thing purge will delete.
type purgeTarget struct {
	path  string
	label string
	dir   bool
}

// purgeTargets lists what maktoob created under the data directory.
//
// The list is explicit rather than "delete the data directory", because -data
// can point anywhere, including somewhere the user keeps other things. Removing
// only what this program wrote is the difference between a destructive command
// and an unpredictable one.
func purgeTargets(cfg config) []purgeTarget {
	db := cfg.dbPath()
	session := cfg.sessionPath()

	targets := []purgeTarget{
		{path: cfg.mediaDir(), label: "downloaded and imported audio", dir: true},
		{path: db, label: "transcripts and search index"},
		// SQLite in WAL mode keeps recent commits in a sidecar file. Deleting
		// only the main database can leave committed transcripts recoverable
		// from the -wal, which is the opposite of what purge promises.
		{path: db + "-wal", label: "database write-ahead log"},
		{path: db + "-shm", label: "database shared memory file"},
		{path: session, label: "WhatsApp session, including the account's identity keys"},
		{path: session + "-wal", label: "WhatsApp session write-ahead log"},
		{path: session + "-shm", label: "WhatsApp session shared memory file"},
		// The salt has to go with the database. It is the key chat and sender
		// aliases are derived from, so keeping it alongside a deleted database
		// leaves the one artifact that could re-link old aliases to real
		// contacts, and keeping the database without the salt makes every chat
		// fork on the next run.
		{path: cfg.saltPath(), label: "contact alias salt"},
	}

	var present []purgeTarget
	for _, t := range targets {
		if _, err := os.Stat(t.path); err == nil {
			present = append(present, t)
		}
	}
	return present
}

func cmdPurge(ctx context.Context, cfg config, assumeYes bool, in io.Reader, out io.Writer) error {
	targets := purgeTargets(cfg)
	if len(targets) == 0 {
		fmt.Fprintf(out, "Nothing to delete: %s holds no maktoob data.\n", cfg.dataDir)
		return nil
	}

	fmt.Fprintf(out, "This will permanently delete the following from %s:\n\n", cfg.dataDir)
	for _, t := range targets {
		suffix := ""
		if t.dir {
			suffix = "/"
		}
		fmt.Fprintf(out, "  %s%s\n      %s\n", filepath.Base(t.path), suffix, t.label)
	}

	// Said plainly because it is the one consequence a user cannot fix
	// afterwards from this machine. Deleting the session locally does not tell
	// WhatsApp anything; the linked device stays on the account's device list
	// until it is removed from the phone.
	fmt.Fprintln(out, "\nTranscripts cannot be recovered after this.")
	fmt.Fprintln(out, "This does not unlink the device from WhatsApp. To do that, run")
	fmt.Fprintln(out, "`maktoob logout` first, or remove it from Linked Devices on your phone.")

	if !assumeYes {
		ok, err := confirm(in, out)
		if err != nil {
			return err
		}
		if !ok {
			fmt.Fprintln(out, "Nothing was deleted.")
			return nil
		}
	}

	var failed []string
	for _, t := range targets {
		if err := os.RemoveAll(t.path); err != nil {
			failed = append(failed, fmt.Sprintf("%s: %v", t.path, err))
		}
	}
	if len(failed) > 0 {
		return fmt.Errorf("some data could not be deleted:\n  %s", strings.Join(failed, "\n  "))
	}

	// Remove the directory only when nothing else is in it. A leftover file the
	// user put there is theirs, not ours to clean up.
	if entries, err := os.ReadDir(cfg.dataDir); err == nil && len(entries) == 0 {
		os.Remove(cfg.dataDir)
	}

	fmt.Fprintln(out, "\nDeleted.")
	return nil
}

// confirm requires the whole word. A single keystroke is too easy to give to a
// prompt the user has not finished reading.
func confirm(in io.Reader, out io.Writer) (bool, error) {
	fmt.Fprint(out, "\nType \"yes\" to confirm: ")

	scanner := bufio.NewScanner(in)
	if !scanner.Scan() {
		if err := scanner.Err(); err != nil {
			return false, fmt.Errorf("read confirmation: %w", err)
		}
		// No input at all, as when stdin is closed. Treated as no.
		return false, nil
	}
	return strings.EqualFold(strings.TrimSpace(scanner.Text()), "yes"), nil
}
