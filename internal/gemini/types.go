package gemini

// Request/response shape of Gemini's speech generation via
// models/{model}:generateContent and :streamGenerateContent
// (https://ai.google.dev/gemini-api/docs/speech-generation):
//
//	header: x-goog-api-key: <key>
//	body:   {"contents":[{"parts":[{"text":"..."}]}],
//	         "generationConfig":{"responseModalities":["AUDIO"],
//	           "speechConfig":{"voiceConfig":{"prebuiltVoiceConfig":{"voiceName":"..."}}}}}
//
// Audio comes back base64-encoded in candidates[].content.parts[].inlineData.data
// as 24 kHz / 16-bit / mono PCM (see audiocache.PCM* constants).

type request struct {
	Contents         []content        `json:"contents"`
	GenerationConfig generationConfig `json:"generationConfig"`
}

type content struct {
	Parts []part `json:"parts"`
}

type part struct {
	Text string `json:"text"`
}

type generationConfig struct {
	ResponseModalities []string     `json:"responseModalities"`
	SpeechConfig       speechConfig `json:"speechConfig"`
}

type speechConfig struct {
	VoiceConfig voiceConfig `json:"voiceConfig"`
}

type voiceConfig struct {
	PrebuiltVoiceConfig prebuiltVoiceConfig `json:"prebuiltVoiceConfig"`
}

type prebuiltVoiceConfig struct {
	VoiceName string `json:"voiceName"`
}

type response struct {
	Candidates []candidate `json:"candidates"`
	Error      *apiError   `json:"error,omitempty"`
}

type candidate struct {
	Content responseContent `json:"content"`
}

type responseContent struct {
	Parts []responsePart `json:"parts"`
}

type responsePart struct {
	InlineData inlineData `json:"inlineData"`
}

type inlineData struct {
	MimeType string `json:"mimeType"`
	Data     string `json:"data"`
}

// apiError is Gemini's error body, e.g.
// {"error":{"code":429,"message":"...","status":"RESOURCE_EXHAUSTED"}}.
type apiError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Status  string `json:"status"`
}
