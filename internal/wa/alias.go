// Package wa links to WhatsApp as a companion device and feeds incoming voice
// notes to a sink.
//
// It imports whatsmeow and the standard library, and nothing else of maktoob's.
// That is enforced in CI rather than merely intended: the README describes this
// package as a replaceable adapter, and a claim a reviewer can check with one
// command is worth more than a paragraph asserting it.
package wa

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"

	"go.mau.fi/whatsmeow/types"
)

// saltLen is 32 bytes because the alias is an HMAC-SHA256 key, not a password
// salt. The phone-number space is about 10^10 and trivially enumerable against
// an unsalted hash; against a secret key of this size it is not.
const saltLen = 32

// Salt is the per-install key that turns WhatsApp identifiers into aliases.
type Salt []byte

// LoadSalt reads the install's alias key, creating it on first run.
//
// The file is owner-only. Losing it is not recoverable: every alias changes, so
// every chat forks into a new row while the old rows keep their display names,
// and the interface shows each conversation twice with nothing to explain it.
// Fingerprint exists so that failure is caught at startup instead of discovered
// in the data.
func LoadSalt(path string) (Salt, error) {
	b, err := os.ReadFile(path)
	switch {
	case err == nil:
		if len(b) != saltLen {
			return nil, fmt.Errorf("salt at %s is %d bytes, want %d", path, len(b), saltLen)
		}
		return Salt(b), nil
	case !os.IsNotExist(err):
		return nil, fmt.Errorf("read salt: %w", err)
	}

	s := make([]byte, saltLen)
	if _, err := rand.Read(s); err != nil {
		return nil, fmt.Errorf("generate salt: %w", err)
	}
	if err := os.WriteFile(path, s, 0o600); err != nil {
		return nil, fmt.Errorf("write salt: %w", err)
	}
	return Salt(s), nil
}

// ChatAlias aliases a chat JID. alt may be the zero JID.
func (s Salt) ChatAlias(chat, alt types.JID) string {
	return s.mac("chat", canonical(chat, alt))
}

// SenderAlias aliases a sender JID. alt may be the zero JID.
func (s Salt) SenderAlias(sender, alt types.JID) string {
	return s.mac("sender", canonical(sender, alt))
}

// MessageAlias aliases a message id, scoped to its chat.
//
// WhatsApp message ids are unique per sender rather than globally, so an
// unscoped key would let one contact's message collide with another's and
// silently suppress a note as a duplicate.
func (s Salt) MessageAlias(chat types.JID, messageID string) string {
	return s.mac("msg", canonical(chat, types.JID{})+"\x00"+messageID)
}

// Fingerprint identifies the salt without revealing it, so a lost or swapped
// salt can be detected before it corrupts the database.
func (s Salt) Fingerprint() string { return s.mac("fingerprint", "maktoob-salt-check") }

// mac domain-separates by kind. Without it, a direct message would produce
// identical chat and sender aliases, since WhatsApp uses the same JID for both.
func (s Salt) mac(kind, value string) string {
	m := hmac.New(sha256.New, s)
	m.Write([]byte(kind))
	m.Write([]byte{0})
	m.Write([]byte(value))
	return hex.EncodeToString(m.Sum(nil))
}

// canonical reduces a JID to one stable string.
//
// Two things would otherwise fork an identity. whatsmeow leaves the device
// suffix on Sender for direct messages, so the same contact writing from their
// phone and their desktop would alias differently. And WhatsApp is migrating
// from phone-number JIDs to @lid, so a chat that switches addressing mode would
// appear twice; when the alternate form is available we normalise toward the
// phone-number form, which is the one that stays stable across that migration.
func canonical(jid, alt types.JID) string {
	if jid.Server == types.HiddenUserServer && alt.Server == types.DefaultUserServer {
		jid = alt
	}
	return jid.ToNonAD().String()
}
