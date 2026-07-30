package web

// Locale holds every string the interface renders, plus the writing direction
// they imply.
//
// It exists because of a split that runs through this project: the *content* is
// Arabic and always renders right-to-left, while the *chrome* ships in English
// until someone writes the Arabic. Hard-coding English into the templates would
// have made translating the interface a rewrite of every page. Here it is one
// value.
//
// To ship an Arabic interface, write an Arabic Locale, set Lang to "ar" and Dir
// to "rtl", and pass it to New. Nothing else changes: the layout already uses
// CSS logical properties, so it mirrors on its own.
type Locale struct {
	Lang string // BCP-47 tag for the interface chrome
	Dir  string // "ltr" or "rtl"

	AppName string
	Tagline string

	NotesHeading  string
	SearchLabel   string
	SearchHint    string
	SearchButton  string
	ChatLabel     string
	AllChats      string
	ClearFilters  string
	NoNotes       string
	NoNotesHint   string
	NoResults     string
	ResultsFor    string
	MatchesIn     string
	BackToList    string
	Import        string
	ImportLabel   string
	ImportButton  string
	ImportHint    string
	LiveRegion    string
	SkipToContent string

	NoteHeading string
	From        string
	InChat      string
	Received    string
	Length      string
	Status      string
	Model       string
	Transcript  string
	// NoSpeech is only correct for a note that finished with nothing in it.
	// Showing it while a note is still queued tells the user their voice note
	// was silent when in fact nothing has run yet — the worst possible lie for
	// this product to tell, and its default state for the first thirteen
	// seconds of every note.
	NoSpeech           string
	StillTranscribing  string
	CouldNotTranscribe string
	PlayFrom           string
	EditSegment        string
	Save               string
	Cancel             string
	Saving             string
	SaveFailed         string
	EditedMark         string
	LowConfMark        string
	SuspectMark        string
	MarksLegend        string
	ExportJSON         string
	ExportMD           string
	AudioMissing       string

	StatusPending      string
	StatusConverting   string
	StatusTranscribing string
	StatusDone         string
	StatusFailed       string
	StatusNoSpeech     string

	WhatsAppConnected string
	WhatsAppOffline   string
	WhatsAppUnpaired  string

	// Announcements are what a screen reader speaks when the page changes
	// underneath the user. They are sentences rather than status words because
	// "transcribed", spoken alone with no subject, tells nobody anything.
	AnnounceArrived    string
	AnnounceReady      string
	AnnounceFailed     string
	AnnounceNoSpeech   string
	AnnounceSaved      string
	AnnounceSaveFailed string

	PrivacyNote string
}

// English is the default interface language.
//
// The wording is deliberately plain about uncertainty. This tool shows machine
// transcription to people who may not be able to check it against the audio,
// so "possible fabrication" is worth the bluntness where "low confidence"
// would have been comfortable and misleading.
var English = Locale{
	Lang: "en",
	Dir:  "ltr",

	AppName: "maktoob",
	Tagline: "Your voice notes, transcribed on your own machine.",

	NotesHeading:  "Voice notes",
	SearchLabel:   "Search transcripts",
	SearchHint:    "Search matches Arabic spelling variants, so hamza and teh marbuta do not have to be typed exactly.",
	SearchButton:  "Search",
	ChatLabel:     "Filter by chat",
	AllChats:      "All chats",
	ClearFilters:  "Clear filters",
	NoNotes:       "No voice notes yet.",
	NoNotesHint:   "Import an audio file below, or link a WhatsApp device and wait for one to arrive.",
	NoResults:     "Nothing matched that search.",
	ResultsFor:    "Results for",
	MatchesIn:     "Matching lines",
	BackToList:    "Back to all notes",
	Import:        "Import audio",
	ImportLabel:   "Choose an audio file",
	ImportButton:  "Transcribe",
	ImportHint:    "The file is copied into your data directory and transcribed locally. It is not uploaded anywhere.",
	LiveRegion:    "Live updates",
	SkipToContent: "Skip to main content",

	NoteHeading:        "Voice note",
	From:               "From",
	InChat:             "Chat",
	Received:           "Received",
	Length:             "Length",
	Status:             "Status",
	Model:              "Model",
	Transcript:         "Transcript",
	NoSpeech:           "No speech was detected in this note.",
	StillTranscribing:  "This note is still being transcribed. The text will appear here on its own when it is ready.",
	CouldNotTranscribe: "This note could not be transcribed.",
	PlayFrom:           "Play from",
	EditSegment:        "Correct this line",
	Save:               "Save",
	Cancel:             "Cancel",
	Saving:             "Saving",
	SaveFailed:         "Could not save that correction.",
	EditedMark:         "edited by hand",
	LowConfMark:        "low confidence",
	SuspectMark:        "possible fabrication",
	MarksLegend:        "Lines marked low confidence or possible fabrication should be checked against the audio before you rely on them.",
	ExportJSON:         "Export as JSON",
	ExportMD:           "Export as Markdown",
	AudioMissing:       "The audio for this note is not on disk.",

	StatusPending:      "queued",
	StatusConverting:   "converting",
	StatusTranscribing: "transcribing",
	StatusDone:         "transcribed",
	StatusFailed:       "failed",
	StatusNoSpeech:     "no speech",

	WhatsAppConnected: "WhatsApp linked",
	WhatsAppOffline:   "WhatsApp reconnecting",
	WhatsAppUnpaired:  "WhatsApp not linked",

	AnnounceArrived:    "A new voice note arrived and is being transcribed.",
	AnnounceReady:      "A transcript is ready.",
	AnnounceFailed:     "A voice note could not be transcribed.",
	AnnounceNoSpeech:   "A voice note arrived with no speech in it.",
	AnnounceSaved:      "Correction saved.",
	AnnounceSaveFailed: "That correction could not be saved.",

	PrivacyNote: "Everything on this page was transcribed on this machine. Nothing was sent to a transcription service.",
}

// StatusLabel maps a stored status onto its display word.
func (l Locale) StatusLabel(status string) string {
	switch status {
	case "pending":
		return l.StatusPending
	case "converting":
		return l.StatusConverting
	case "transcribing":
		return l.StatusTranscribing
	case "done":
		return l.StatusDone
	case "failed":
		return l.StatusFailed
	case "no_speech":
		return l.StatusNoSpeech
	default:
		return status
	}
}

// Announcement is the sentence spoken when a note reaches the given status.
//
// Returns empty for the transient states, because "converting" is not news to
// anyone and interrupting a screen reader three times per note to say so is
// worse than silence.
func (l Locale) Announcement(status string) string {
	switch status {
	case "done":
		return l.AnnounceReady
	case "failed":
		return l.AnnounceFailed
	case "no_speech":
		return l.AnnounceNoSpeech
	default:
		return ""
	}
}

// MarkerLabel translates an export marker into the interface language, so the
// two never disagree about what a warning is called.
func (l Locale) MarkerLabel(marker string) string {
	switch marker {
	case "possible fabrication":
		return l.SuspectMark
	case "low confidence":
		return l.LowConfMark
	case "edited by hand":
		return l.EditedMark
	default:
		return marker
	}
}
