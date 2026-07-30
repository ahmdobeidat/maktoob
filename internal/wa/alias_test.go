package wa

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.mau.fi/whatsmeow/types"
)

func testSalt(t *testing.T) Salt {
	t.Helper()
	s, err := LoadSalt(filepath.Join(t.TempDir(), "salt"))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestLoadSaltCreatesOnceWithOwnerOnlyMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "salt")

	first, err := LoadSalt(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 32 {
		t.Fatalf("salt length: got %d, want 32", len(first))
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode: got %o, want 600", info.Mode().Perm())
	}

	second, err := LoadSalt(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != string(second) {
		t.Fatal("salt changed between loads")
	}
}

func TestAliasIsStableAndOpaque(t *testing.T) {
	s := testSalt(t)
	phone := "962790000000"
	base := types.JID{User: phone, Server: types.DefaultUserServer}

	got := s.SenderAlias(base, types.JID{})
	if got == "" {
		t.Fatal("empty alias")
	}
	if strings.Contains(got, phone) {
		t.Fatalf("alias leaks the phone number: %q", got)
	}
	if got != s.SenderAlias(base, types.JID{}) {
		t.Fatal("alias is not stable across calls")
	}

	// The same contact writing from a linked device must not fork.
	withDevice := types.JID{User: phone, Server: types.DefaultUserServer, Device: 3}
	if s.SenderAlias(withDevice, types.JID{}) != got {
		t.Fatal("device suffix produced a different alias")
	}

	// A different install must produce a different alias for the same person.
	other := testSalt(t)
	if other.SenderAlias(base, types.JID{}) == got {
		t.Fatal("alias is not install-scoped")
	}
}

func TestAliasPrefersPhoneNumberForm(t *testing.T) {
	s := testSalt(t)
	pn := types.JID{User: "962790000000", Server: types.DefaultUserServer}
	lid := types.JID{User: "11223344", Server: types.HiddenUserServer}

	// A chat that switches addressing mode must not fork into a second row.
	if s.SenderAlias(lid, pn) != s.SenderAlias(pn, types.JID{}) {
		t.Fatal("lid and pn forms produced different aliases")
	}
}

func TestAliasDomainSeparation(t *testing.T) {
	s := testSalt(t)
	// In a direct message the chat JID and the sender JID are the same value.
	jid := types.JID{User: "962790000000", Server: types.DefaultUserServer}
	if s.ChatAlias(jid, types.JID{}) == s.SenderAlias(jid, types.JID{}) {
		t.Fatal("chat and sender aliases collide for a direct message")
	}
}

func TestMessageAliasIsChatScoped(t *testing.T) {
	s := testSalt(t)
	a := types.JID{User: "962790000000", Server: types.DefaultUserServer}
	b := types.JID{User: "962790000001", Server: types.DefaultUserServer}

	if s.MessageAlias(a, "3EB0ABC") == s.MessageAlias(b, "3EB0ABC") {
		t.Fatal("the same message id in two chats produced one key")
	}
}

func TestFingerprintTracksTheSalt(t *testing.T) {
	a, b := testSalt(t), testSalt(t)
	if a.Fingerprint() == "" {
		t.Fatal("empty fingerprint")
	}
	if a.Fingerprint() == b.Fingerprint() {
		t.Fatal("two different salts share a fingerprint")
	}
}
