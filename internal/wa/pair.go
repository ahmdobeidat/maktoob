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

	dsn := "file:" + sessionPath + "?_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)"
	container, err := sqlstore.New(ctx, "sqlite", dsn, waLog.Noop)
	if err != nil {
		return nil, fmt.Errorf("open session store: %w", err)
	}
	if err := os.Chmod(sessionPath, 0o600); err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("restrict session store: %w", err)
	}

	device, err := container.GetFirstDevice(ctx)
	if err != nil {
		return nil, fmt.Errorf("read linked device: %w", err)
	}

	return whatsmeow.NewClient(device, waLog.Noop), nil
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
