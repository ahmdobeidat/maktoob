// Command maktoob transcribes WhatsApp voice notes locally.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
)

const usage = `maktoob — read your voice notes, on your own machine.

Usage:
  maktoob import <file>...   transcribe audio files
  maktoob list               show stored notes
  maktoob show <id>          show one transcript

Flags:
  -data   directory for media, database and session state (default "data")
  -asr    whisper-server base URL (default "http://127.0.0.1:8642")
  -model  model name recorded against transcripts (default "large-v3-turbo")
  -lang   language to transcribe as, or "auto" to detect (default "ar")
`

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "maktoob:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		fmt.Print(usage)
		return nil
	}

	command, rest := args[0], args[1:]

	fs := flag.NewFlagSet(command, flag.ContinueOnError)
	dataDir := fs.String("data", "data", "directory for media, database and session state")
	asrURL := fs.String("asr", "http://127.0.0.1:8642", "whisper-server base URL")
	model := fs.String("model", "large-v3-turbo", "model name recorded against transcripts")
	// Language is pinned rather than auto-detected, because detection runs on
	// the first 30-second window and is unreliable on a short note. The pin is
	// exposed because getting it wrong is not a soft failure: whisper will
	// transcribe confidently into the language it was told, and neither the
	// confidence score nor the no-speech probability detects the mismatch.
	lang := fs.String("lang", "ar", "language to transcribe as, or \"auto\" to detect")
	if err := fs.Parse(rest); err != nil {
		return err
	}

	// Interrupting must leave the database consistent rather than killing a
	// transaction, so the signal cancels the context and unwinds normally.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfg := config{
		dataDir: *dataDir,
		asrURL:  *asrURL,
		model:   *model,
		lang:    *lang,
	}

	switch command {
	case "import":
		return cmdImport(ctx, cfg, fs.Args())
	case "list":
		return cmdList(ctx, cfg)
	case "show":
		if fs.NArg() != 1 {
			return fmt.Errorf("show requires exactly one note id")
		}
		return cmdShow(ctx, cfg, fs.Arg(0))
	case "help", "-h", "--help":
		fmt.Print(usage)
		return nil
	default:
		return fmt.Errorf("unknown command %q\n\n%s", command, usage)
	}
}

type config struct {
	dataDir string
	asrURL  string
	model   string
	lang    string
}

func (c config) dbPath() string   { return filepath.Join(c.dataDir, "maktoob.db") }
func (c config) mediaDir() string { return filepath.Join(c.dataDir, "media") }

// ensureDataDir creates the data directory with owner-only permissions.
//
// This directory holds private media, transcripts, and eventually the WhatsApp
// session store, which contains the account's identity keys. Anyone who can
// read it can impersonate the linked account, so the mode is deliberate rather
// than inherited from the process umask.
func (c config) ensureDataDir() error {
	if err := os.MkdirAll(c.dataDir, 0o700); err != nil {
		return fmt.Errorf("create data directory: %w", err)
	}
	return os.Chmod(c.dataDir, 0o700)
}
