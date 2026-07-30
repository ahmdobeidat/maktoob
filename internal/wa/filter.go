package wa

import (
	"strings"

	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
)

// voiceNote reports whether an incoming message is a voice note maktoob should
// keep, and returns the audio when it is.
//
// Every drop here is deliberate:
//
//   - Not PTT: an attached mp3 is a file someone sent, not a voice note. The
//     product is voice notes.
//   - IsFromMe: our own notes are not an accessibility problem for us.
//   - Status broadcast: every contact's Status voice post arrives as an ordinary
//     message. Without this the list floods on the first day.
//   - Newsletter: channel content, not a conversation.
//   - View once: the sender chose ephemerality. Transcribing it to a permanent
//     searchable database would break that on their behalf, without their
//     knowledge, in a tool that claims to be privacy-first.
func voiceNote(evt *events.Message) (*waE2E.AudioMessage, bool) {
	if evt == nil || evt.Message == nil {
		return nil, false
	}
	audio := evt.Message.GetAudioMessage()
	if audio == nil || !audio.GetPTT() {
		return nil, false
	}
	if evt.Info.IsFromMe {
		return nil, false
	}
	if evt.Info.Chat == types.StatusBroadcastJID || evt.Info.Chat.Server == types.NewsletterServer {
		return nil, false
	}

	// Four view-once signals, because they are not interchangeable.
	// events.Message.IsViewOnce is set only when the message arrived inside a
	// ViewOnceMessage wrapper, but a view-once voice note from a current client
	// sets the flag on the AudioMessage instead. Checking only the event field
	// lets the common case straight through.
	if evt.IsViewOnce || evt.IsViewOnceV2 || evt.IsViewOnceV2Extension || audio.GetViewOnce() {
		return nil, false
	}

	return audio, true
}

// extForMimetype maps WhatsApp's audio mimetypes to a file extension.
//
// The table is hard-coded rather than resolved through mime.ExtensionsByType,
// which reads the host's /etc/mime.types and can return ".oga" for ogg. The
// browser plays what we store, so a host-dependent extension is a
// host-dependent bug in the player.
func extForMimetype(mime string) string {
	base, _, _ := strings.Cut(mime, ";")
	switch strings.TrimSpace(strings.ToLower(base)) {
	case "audio/mpeg":
		return ".mp3"
	case "audio/mp4", "audio/aac":
		return ".m4a"
	default:
		return ".ogg"
	}
}
