package wa

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/mdp/qrterminal/v3"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/store/sqlstore"
	waLog "go.mau.fi/whatsmeow/util/log"
	"google.golang.org/protobuf/proto"

	_ "modernc.org/sqlite"
)

// ErrNotPaired is returned when an operation needs a linked device and none is
// stored.
var ErrNotPaired = fmt.Errorf("wa: no WhatsApp device is linked, run: maktoob pair")

func init() {
	// Requested history sync is not the same thing as stored history sync.
	// whatsmeow's defaults ask for a 10 GB payload with both day limits unset,
	// so pairing would pull the account's archive whether or not we read it.
	// Zeroing the limits is what makes "forward-only" a description of what we
	// ask for rather than only of what we keep, and the pairing payload is
	// something a reviewer can read.
	//
	// The fields are set individually rather than by replacing the struct, which
	// would silently drop the dozen capability flags whatsmeow sets alongside
	// them.
	hs := store.DeviceProps.HistorySyncConfig
	hs.FullSyncDaysLimit = proto.Uint32(0)
	hs.RecentSyncDaysLimit = proto.Uint32(0)
	hs.StorageQuotaMb = proto.Uint32(0)

	// Without this the device appears in the user's Linked Devices list as
	// "whatsmeow".
	store.SetOSInfo("maktoob", [3]uint32{1, 0, 0})
}

// Connect opens the session store and returns a client for the linked device,
// connecting it if one exists.
//
// The session store holds the account's identity keys: anyone with this file can
// impersonate the WhatsApp account. It is created owner-only, inside a directory
// that is owner-only, and PRIVACY.md says so in as many words.
func Connect(ctx context.Context, sessionPath string, log *slog.Logger) (*whatsmeow.Client, error) {
	if err := os.MkdirAll(filepath.Dir(sessionPath), 0o700); err != nil {
		return nil, fmt.Errorf("create session directory: %w", err)
	}

	waLogger := slogAdapter{log: fallbackLogger(log)}

	dsn := "file:" + sessionPath + "?_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)"
	container, err := sqlstore.New(ctx, "sqlite", dsn, waLogger)
	if err != nil {
		return nil, fmt.Errorf("open session store: %w", err)
	}

	// WAL mode means the identity keys written during pairing land in
	// session.db-wal before they are ever checkpointed into session.db, and
	// modernc.org/sqlite creates all three files at the process umask (0644
	// under a typical 022) rather than inheriting a mode from the DSN. Chmod
	// the sidecars too, or the doc comment above is a claim about one file
	// out of three for as long as the process runs.
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if err := os.Chmod(sessionPath+suffix, 0o600); err != nil && !os.IsNotExist(err) {
			return nil, fmt.Errorf("restrict session store: %w", err)
		}
	}

	device, err := container.GetFirstDevice(ctx)
	if err != nil {
		return nil, fmt.Errorf("read linked device: %w", err)
	}

	return whatsmeow.NewClient(device, waLogger), nil
}

// Pair prints a QR code and blocks until the user scans it or ctx is cancelled.
func Pair(ctx context.Context, client *whatsmeow.Client, out io.Writer) error {
	if client.Store.ID != nil {
		return fmt.Errorf("wa: a device is already linked; run: maktoob logout")
	}

	qrChan, err := client.GetQRChannel(ctx)
	if err != nil {
		return fmt.Errorf("start pairing: %w", err)
	}
	if err := client.Connect(); err != nil {
		return fmt.Errorf("connect: %w", err)
	}

	fmt.Fprintln(out, "Open WhatsApp on your phone, then Settings, Linked Devices, Link a Device.")
	fmt.Fprintln(out)

	for evt := range qrChan {
		switch evt.Event {
		case "code":
			qrterminal.GenerateHalfBlock(evt.Code, qrterminal.L, out)
		case "success":
			fmt.Fprintln(out, "\nLinked. maktoob will transcribe voice notes that arrive from now on.")
			return nil
		case "timeout":
			return fmt.Errorf("pairing timed out, run the command again")
		default:
			// evt.Error carries the actual failure for the "error" event and
			// for whatsmeow's other named error events (e.g. client outdated,
			// scanned without multidevice); falling back to the bare event
			// name only when whatsmeow did not set it avoids reporting
			// "pairing failed: error" when the real cause is one Fprintln
			// away.
			if evt.Error != nil {
				return fmt.Errorf("pairing failed: %w", evt.Error)
			}
			return fmt.Errorf("pairing failed: %s", evt.Event)
		}
	}
	return fmt.Errorf("pairing ended without linking")
}

// Logout unlinks the device and clears the session. Transcripts are untouched:
// unlinking is not deleting, and conflating the two would destroy a user's data
// on a command that does not sound like it should.
func Logout(ctx context.Context, client *whatsmeow.Client) error {
	if client.Store.ID == nil {
		return ErrNotPaired
	}
	if err := client.Logout(ctx); err != nil {
		return fmt.Errorf("logout: %w", err)
	}
	return nil
}

// fallbackLogger returns log, or slog.Default() if log is nil, matching the
// nil-tolerant convention Listener.Log already uses elsewhere in this
// package. A Connect caller that doesn't care about logging shouldn't have
// to pass one just to avoid a nil dereference.
func fallbackLogger(log *slog.Logger) *slog.Logger {
	if log == nil {
		return slog.Default()
	}
	return log
}

// slogAdapter satisfies whatsmeow's waLog.Logger interface over a
// *slog.Logger, so that a caller-supplied logger actually receives
// whatsmeow's connection and pairing diagnostics. Without it, Connect's log
// parameter would be accepted and then ignored, which is worse than not
// having it: a caller who passes a real logger has no reason to expect
// silence back, and pairing failures are exactly when that silence hurts.
type slogAdapter struct {
	log *slog.Logger
}

func (a slogAdapter) Warnf(msg string, args ...interface{})  { a.log.Warn(fmt.Sprintf(msg, args...)) }
func (a slogAdapter) Errorf(msg string, args ...interface{}) { a.log.Error(fmt.Sprintf(msg, args...)) }
func (a slogAdapter) Infof(msg string, args ...interface{})  { a.log.Info(fmt.Sprintf(msg, args...)) }
func (a slogAdapter) Debugf(msg string, args ...interface{}) { a.log.Debug(fmt.Sprintf(msg, args...)) }

// Sub returns a logger scoped to module, mirroring whatsmeow's own submodule
// loggers (e.g. "Client", "Socket") so log lines from different parts of the
// library can still be told apart once they're flowing through slog.
func (a slogAdapter) Sub(module string) waLog.Logger {
	return slogAdapter{log: a.log.With("module", module)}
}
