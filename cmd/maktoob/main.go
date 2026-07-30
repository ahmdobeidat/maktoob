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
  maktoob serve              read your notes in a browser, on this machine
  maktoob import <file>...   transcribe audio files
  maktoob list               show stored notes
  maktoob show <id>          show one transcript
  maktoob export <id>        write one transcript to stdout
  maktoob pair               link a WhatsApp device by scanning a QR code
  maktoob logout             unlink the device (transcripts are untouched)
  maktoob purge              delete everything maktoob has stored

Flags:
  -data     directory for media, database and session state (default "data")
  -asr      whisper-server base URL (default "http://127.0.0.1:8642")
  -model    model name recorded against transcripts (default "large-v3-turbo")
  -lang     language to transcribe as, or "auto" to detect (default "ar")
  -addr     address for serve to listen on (default "127.0.0.1:8765")
  -format   export format, "json" or "md" (default "json")
  -yes      answer yes to purge's confirmation prompt
  -verbose  print whatsmeow's own diagnostics to stderr. These contain your
            contacts' phone numbers in the clear, so do not use this where
            stderr is redirected to a file or captured by a service manager.
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
	// Off by default because what it turns on is not merely noisy. whatsmeow
	// logs un-aliased JIDs at Warn and Error in ordinary operation, so with this
	// set, redirecting stderr writes contacts' phone numbers to a file outside
	// data/ and defeats the alias salt. The help text says so rather than
	// leaving the user to discover it in a log.
	verbose := fs.Bool("verbose", false,
		"print whatsmeow diagnostics to stderr; these include contacts' phone numbers")
	// Loopback by default, and the help text does not offer a recipe for
	// changing it. There is no authentication and no account model, so binding
	// this to a reachable interface publishes one person's private messages to
	// the network. It is a flag rather than a constant because a user who knows
	// they are behind something else may need it.
	addr := fs.String("addr", "127.0.0.1:8765", "address for serve to listen on")
	format := fs.String("format", "json", "export format: \"json\" or \"md\"")
	assumeYes := fs.Bool("yes", false, "answer yes to purge's confirmation prompt")
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
		verbose: *verbose,
	}

	switch command {
	case "serve":
		return cmdServe(ctx, cfg, *addr)
	case "import":
		return cmdImport(ctx, cfg, fs.Args())
	case "list":
		return cmdList(ctx, cfg)
	case "show":
		if fs.NArg() != 1 {
			return fmt.Errorf("show requires exactly one note id")
		}
		return cmdShow(ctx, cfg, fs.Arg(0))
	case "export":
		if fs.NArg() != 1 {
			return fmt.Errorf("export requires exactly one note id")
		}
		return cmdExport(ctx, cfg, fs.Arg(0), *format, os.Stdout)
	case "purge":
		return cmdPurge(ctx, cfg, *assumeYes, os.Stdin, os.Stdout)
	case "pair":
		return cmdPair(ctx, cfg)
	case "logout":
		return cmdLogout(ctx, cfg)
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
	verbose bool
}

func (c config) dbPath() string      { return filepath.Join(c.dataDir, "maktoob.db") }
func (c config) mediaDir() string    { return filepath.Join(c.dataDir, "media") }
func (c config) sessionPath() string { return filepath.Join(c.dataDir, "session.db") }
func (c config) saltPath() string    { return filepath.Join(c.dataDir, "salt") }

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
