// Package wyoming implements the server side of the Wyoming protocol's
// text-to-speech service, as consumed by Home Assistant's built-in "wyoming"
// integration (which turns it into the Assist pipeline's TTS entity).
package wyoming

// Header is the JSON envelope that precedes every Wyoming message on the wire.
//
// Wyoming protocol 1.9+ separates event data from the header:
//   - DataLength > 0: read exactly that many bytes after the \n as event data JSON.
//   - PayloadLength > 0: read that many bytes of binary payload after the data.
//
// Older clients inline event data in the "data" field. Both formats are
// supported on read; we always write in the 1.9 format.
type Header struct {
	Type          string      `json:"type"`
	Version       string      `json:"version,omitempty"`
	Data          interface{} `json:"data,omitempty"`        // legacy inline format
	DataLength    int         `json:"data_length,omitempty"` // Wyoming 1.9+
	PayloadLength int         `json:"payload_length,omitempty"`
}

// Event type names used by the TTS flow.
const (
	TypeDescribe          = "describe"
	TypeInfo              = "info"
	TypeSynthesize        = "synthesize"
	TypeSynthesizeStart   = "synthesize-start"
	TypeSynthesizeChunk   = "synthesize-chunk"
	TypeSynthesizeStop    = "synthesize-stop"
	TypeSynthesizeStopped = "synthesize-stopped"
	TypeAudioStart        = "audio-start"
	TypeAudioChunk        = "audio-chunk"
	TypeAudioStop         = "audio-stop"
	TypeError             = "error"
)

// InfoData is the response payload for the "describe" request. The field
// name "tts" is mandated by the protocol.
type InfoData struct {
	TTS []TTSProgram `json:"tts"`
}

// Attribution holds the name and URL of who provides a service or voice.
type Attribution struct {
	Name string `json:"name"`
	URL  string `json:"url"`
}

// TTSProgram describes one TTS engine exposed by this server.
type TTSProgram struct {
	Name        string      `json:"name"`
	Description string      `json:"description"`
	Attribution Attribution `json:"attribution"`
	Installed   bool        `json:"installed"`
	Version     string      `json:"version,omitempty"`
	Voices      []TTSVoice  `json:"voices"`
	// SupportsSynthesizeStreaming is false: text is accumulated until
	// synthesize-stop, then audio streams out as Gemini produces it.
	SupportsSynthesizeStreaming bool `json:"supports_synthesize_streaming"`
}

// TTSVoice describes one selectable voice.
type TTSVoice struct {
	Name        string      `json:"name"`
	Description string      `json:"description"`
	Attribution Attribution `json:"attribution"`
	Installed   bool        `json:"installed"`
	Version     string      `json:"version,omitempty"`
	Languages   []string    `json:"languages"`
}

// SynthesizeVoice is the voice selection inside synthesize events.
type SynthesizeVoice struct {
	Name     string `json:"name,omitempty"`
	Language string `json:"language,omitempty"`
	Speaker  string `json:"speaker,omitempty"`
}

// SynthesizeData is the payload of "synthesize" (full text) and
// "synthesize-start" (voice only) events.
type SynthesizeData struct {
	Text  string           `json:"text,omitempty"`
	Voice *SynthesizeVoice `json:"voice,omitempty"`
}

// SynthesizeChunkData is the payload of a "synthesize-chunk" event.
type SynthesizeChunkData struct {
	Text string `json:"text"`
}

// AudioFormatData is the payload of "audio-start" and each "audio-chunk"
// header; the PCM bytes of a chunk follow as the binary payload.
type AudioFormatData struct {
	Rate     int `json:"rate"`
	Width    int `json:"width"`
	Channels int `json:"channels"`
}

// ErrorData is the payload of an "error" event.
type ErrorData struct {
	Text string `json:"text"`
	Code string `json:"code,omitempty"`
}
