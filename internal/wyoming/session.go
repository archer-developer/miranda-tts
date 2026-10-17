package wyoming

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strings"

	"github.com/archer-developer/miranda-tts/internal/audiocache"
)

var attribution = Attribution{Name: "Google", URL: "https://ai.google.dev"}

// Session handles one Wyoming client connection.
//
// Home Assistant's streaming TTS path always sends
// synthesize-start, synthesize-chunk*, synthesize (the whole text again, for
// backwards compatibility), synthesize-stop, and only stops reading audio on
// "synthesize-stopped" — so that handshake is implemented in full. A bare
// "synthesize" (HA's non-streaming path, which ends on audio-stop) is served
// as a one-shot request.
type Session struct {
	id      string
	conn    net.Conn
	reader  *Reader
	writer  *Writer
	logger  *slog.Logger
	synth   Synthesizer
	service Service

	// streaming state, set between synthesize-start and synthesize-stop.
	streaming bool
	voice     string
	text      strings.Builder
}

// NewSession constructs a Session for an accepted TCP connection.
func NewSession(id string, conn net.Conn, logger *slog.Logger, synth Synthesizer, service Service) *Session {
	return &Session{
		id:      id,
		conn:    conn,
		reader:  NewReader(conn),
		writer:  NewWriter(conn),
		logger:  logger.With("session_id", id),
		synth:   synth,
		service: service,
	}
}

// Run processes messages until the client disconnects or ctx is cancelled.
func (s *Session) Run(ctx context.Context) {
	defer func() { _ = s.conn.Close() }()

	// Unblock the blocking read when the server is shutting down.
	stop := context.AfterFunc(ctx, func() { _ = s.conn.Close() })
	defer stop()

	for {
		if err := s.handleOne(ctx); err != nil {
			if errors.Is(err, io.EOF) || ctx.Err() != nil {
				s.logger.Info("client disconnected")
				return
			}
			s.logger.Error("session error", "error", err)
			return
		}
	}
}

func (s *Session) handleOne(ctx context.Context) error {
	h, err := s.reader.ReadHeader()
	if err != nil {
		return err
	}
	// Any binary payload must be consumed to keep the stream aligned,
	// whether or not we care about it.
	if h.PayloadLength > 0 {
		if _, err := s.reader.ReadPayload(h.PayloadLength); err != nil {
			return err
		}
	}
	s.logger.Debug("wyoming ← recv", "type", h.Type)

	switch h.Type {
	case TypeDescribe:
		return s.handleDescribe()
	case TypeSynthesizeStart:
		return s.handleSynthesizeStart(h)
	case TypeSynthesizeChunk:
		return s.handleSynthesizeChunk(h)
	case TypeSynthesize:
		return s.handleSynthesize(ctx, h)
	case TypeSynthesizeStop:
		return s.handleSynthesizeStop(ctx)
	default:
		s.logger.Warn("unexpected message type, ignoring", "type", h.Type)
		return nil
	}
}

func (s *Session) handleDescribe() error {
	info := InfoData{TTS: []TTSProgram{{
		Name:        "gemini-tts",
		Description: "Gemini text-to-speech",
		Attribution: attribution,
		Installed:   true,
		Voices: []TTSVoice{{
			Name:        s.service.Voice,
			Description: fmt.Sprintf("%s (%s)", s.service.Voice, s.service.Model),
			Attribution: attribution,
			Installed:   true,
			Languages:   s.service.Languages,
		}},
	}}}
	return s.writer.WriteMessage(TypeInfo, info, nil)
}

func (s *Session) handleSynthesizeStart(h Header) error {
	var d SynthesizeData
	if err := DecodeData(h, &d); err != nil {
		return err
	}
	s.streaming = true
	s.voice = voiceName(d.Voice)
	s.text.Reset()
	return nil
}

func (s *Session) handleSynthesizeChunk(h Header) error {
	var d SynthesizeChunkData
	if err := DecodeData(h, &d); err != nil {
		return err
	}
	s.text.WriteString(d.Text)
	return nil
}

// handleSynthesize serves a bare synthesize; inside a streaming exchange it
// is the redundant full-text copy and is ignored, since the chunks already
// carried the same text.
func (s *Session) handleSynthesize(ctx context.Context, h Header) error {
	if s.streaming {
		return nil
	}
	var d SynthesizeData
	if err := DecodeData(h, &d); err != nil {
		return err
	}
	return s.speak(ctx, d.Text, voiceName(d.Voice))
}

func (s *Session) handleSynthesizeStop(ctx context.Context) error {
	if !s.streaming {
		return nil
	}
	text, voice := s.text.String(), s.voice
	s.streaming = false
	s.text.Reset()

	err := s.speak(ctx, text, voice)
	if err != nil {
		return err
	}
	return s.writer.WriteMessage(TypeSynthesizeStopped, nil, nil)
}

// speak synthesizes text and forwards PCM to the client as it arrives:
// audio-start on the first piece, one audio-chunk per piece, audio-stop at
// the end. A synthesis failure is reported to the client as an "error" event
// (which Home Assistant raises as a TTS error) instead of dropping the
// connection silently; the session itself stays usable.
func (s *Session) speak(ctx context.Context, text, voice string) error {
	format := AudioFormatData{
		Rate:     audiocache.PCMSampleRate,
		Width:    audiocache.PCMBitsPerSample / 8,
		Channels: audiocache.PCMChannels,
	}
	text = strings.TrimSpace(text)
	s.logger.Info("synthesize requested", "model", s.service.Model, "voice", voice, "chars", len([]rune(text)))

	started := false
	emit := func(pcm []byte) error {
		if !started {
			if err := s.writer.WriteMessage(TypeAudioStart, format, nil); err != nil {
				return err
			}
			started = true
		}
		return s.writer.WriteMessage(TypeAudioChunk, format, pcm)
	}

	if text != "" {
		if err := s.synth.SynthesizeStream(ctx, text, voice, emit); err != nil {
			if ctx.Err() != nil || isConnError(err) {
				return err
			}
			s.logger.Error("synthesis failed, sending error event to client", "model", s.service.Model, "voice", voice, "error", err)
			return s.writer.WriteMessage(TypeError, ErrorData{Text: "speech synthesis failed", Code: "synthesis-failed"}, nil)
		}
	}
	if !started {
		// Empty text or empty audio: still deliver a well-formed (empty) clip.
		if err := s.writer.WriteMessage(TypeAudioStart, format, nil); err != nil {
			return err
		}
	}
	return s.writer.WriteMessage(TypeAudioStop, nil, nil)
}

func voiceName(v *SynthesizeVoice) string {
	if v == nil {
		return ""
	}
	return v.Name
}

// isConnError reports a failure writing to the client (as opposed to a
// synthesis failure), after which there is nobody to send an error event to.
func isConnError(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) || errors.Is(err, io.ErrClosedPipe) || errors.Is(err, net.ErrClosed)
}
