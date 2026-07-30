package wa

import (
	"testing"
	"time"

	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"
)

func audioPTT() *waE2E.Message {
	return &waE2E.Message{AudioMessage: &waE2E.AudioMessage{
		PTT:           proto.Bool(true),
		Mimetype:      proto.String("audio/ogg; codecs=opus"),
		Seconds:       proto.Uint32(7),
		DirectPath:    proto.String("/v/t62.7117-24/x"),
		MediaKey:      []byte("key"),
		FileSHA256:    []byte("sha"),
		FileEncSHA256: []byte("enc"),
	}}
}

func message(chat types.JID, m *waE2E.Message) *events.Message {
	return &events.Message{
		Info: types.MessageInfo{
			MessageSource: types.MessageSource{
				Chat:    chat,
				Sender:  types.JID{User: "962790000000", Server: types.DefaultUserServer},
				IsGroup: chat.Server == types.GroupServer,
			},
			ID:        "3EB0ABC",
			PushName:  "Um Ahmad",
			Timestamp: time.Unix(1700000000, 0),
		},
		Message: m,
	}
}

func TestVoiceNoteFilter(t *testing.T) {
	dm := types.JID{User: "962790000000", Server: types.DefaultUserServer}
	group := types.JID{User: "120363000000000000", Server: types.GroupServer}
	news := types.JID{User: "120363000000000001", Server: types.NewsletterServer}

	attachedAudio := &waE2E.Message{AudioMessage: &waE2E.AudioMessage{
		PTT: proto.Bool(false), Mimetype: proto.String("audio/mpeg"),
	}}
	viewOnceOnAudio := audioPTT()
	viewOnceOnAudio.AudioMessage.ViewOnce = proto.Bool(true)

	fromMe := message(dm, audioPTT())
	fromMe.Info.IsFromMe = true

	// All four view-once signals, because PRIVACY.md tells the reader this is
	// "checked four different ways" and two of them had no test behind that
	// claim. They are not interchangeable: which flag gets set depends on the
	// sending client's version, so a regression in any one of them silently
	// starts transcribing messages someone chose to make ephemeral.
	wrapperViewOnce := message(dm, audioPTT())
	wrapperViewOnce.IsViewOnce = true

	wrapperViewOnceV2 := message(dm, audioPTT())
	wrapperViewOnceV2.IsViewOnceV2 = true

	wrapperViewOnceV2Ext := message(dm, audioPTT())
	wrapperViewOnceV2Ext.IsViewOnceV2Extension = true

	cases := []struct {
		name string
		evt  *events.Message
		want bool
	}{
		{"voice note in a direct chat", message(dm, audioPTT()), true},
		{"voice note in a group", message(group, audioPTT()), true},
		{"status broadcast", message(types.StatusBroadcastJID, audioPTT()), false},
		{"newsletter", message(news, audioPTT()), false},
		{"sent by us", fromMe, false},
		{"view once via wrapper", wrapperViewOnce, false},
		{"view once via the v2 wrapper", wrapperViewOnceV2, false},
		{"view once via the v2 extension wrapper", wrapperViewOnceV2Ext, false},
		{"view once on the audio itself", message(dm, viewOnceOnAudio), false},
		{"attached audio file, not a voice note", message(dm, attachedAudio), false},
		{"plain text", message(dm, &waE2E.Message{Conversation: proto.String("hi")}), false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, ok := voiceNote(tc.evt)
			if ok != tc.want {
				t.Fatalf("got accept=%v, want %v", ok, tc.want)
			}
		})
	}
}

func TestExtForMimetype(t *testing.T) {
	cases := map[string]string{
		"audio/ogg; codecs=opus": ".ogg",
		"audio/ogg":              ".ogg",
		"audio/mpeg":             ".mp3",
		"audio/mp4":              ".m4a",
		"":                       ".ogg",
		"application/nonsense":   ".ogg",
	}
	for mime, want := range cases {
		if got := extForMimetype(mime); got != want {
			t.Fatalf("%q: got %q, want %q", mime, got, want)
		}
	}
}
