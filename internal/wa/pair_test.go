package wa

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	waLog "go.mau.fi/whatsmeow/util/log"
)

// whatsmeow logs un-aliased JIDs at Warn and Error during ordinary operation:
// device-list hash changes, verified-name parse failures, push-name history
// sync, retry-receipt handling. slog.Default() emits at Info and above, so
// wiring the adapter in by default puts contacts' phone numbers on stderr,
// where `2> log.txt` or journald writes them to a file outside data/. That is
// the partial disclosure the alias salt exists to prevent, so silence has to be
// the default and the diagnostics have to be asked for.
func TestWhatsmeowLoggerIsSilentUnlessAsked(t *testing.T) {
	if got := whatsmeowLogger(slog.Default(), false); got != waLog.Noop {
		t.Fatalf("default logger is %T, want waLog.Noop", got)
	}

	if _, ok := whatsmeowLogger(slog.Default(), true).(slogAdapter); !ok {
		t.Fatal("-verbose did not install the slog adapter")
	}
}

// The Noop logger has to actually swallow the levels that carry JIDs, not just
// the ones nobody reads. Warnf and Errorf are the two that matter.
func TestNoopLoggerDropsWarnAndError(t *testing.T) {
	var buf bytes.Buffer
	restore := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(restore) })

	l := whatsmeowLogger(nil, false)
	l.Warnf("%s's device list hash changed", "962790000000")
	l.Errorf("Failed to parse %s's verified name details", "962790000000")
	l.Sub("Client").Warnf("retry request from %s", "962790000000")

	if strings.Contains(buf.String(), "962790000000") {
		t.Fatalf("a phone number reached the log by default: %q", buf.String())
	}
}

// The opt-in path must genuinely work, or -verbose is a lie in the other
// direction and pairing failures debug themselves in silence.
func TestVerboseLoggerForwardsToSlog(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, nil))

	whatsmeowLogger(log, true).Warnf("pairing said %s", "no")

	if !strings.Contains(buf.String(), "pairing said no") {
		t.Fatalf("-verbose dropped a warning: %q", buf.String())
	}
}
