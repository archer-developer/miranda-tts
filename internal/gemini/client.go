// Package gemini synthesizes speech with Gemini's text-to-speech models.
//
// Two entry points share one key-rotation state: SynthesizeOnce returns a
// complete clip (used for media_player playback), SynthesizeStream hands PCM
// to a callback as it arrives (used by the Wyoming server so Home Assistant
// can start playing before synthesis finishes).
package gemini

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/archer-developer/miranda-llm/keyrotation"

	"github.com/archer-developer/miranda-tts/internal/audiocache"
)

// baseURL is a var so tests can point the client at an httptest.Server.
var baseURL = "https://generativelanguage.googleapis.com"

const (
	generateURLFormat       = "%s/v1beta/models/%s:generateContent"
	streamGenerateURLFormat = "%s/v1beta/models/%s:streamGenerateContent?alt=sse"

	// fallbackFrameBytes is the PCM frame size used when a buffered clip has
	// to be re-chunked for a streaming consumer.
	fallbackFrameBytes = 4096

	maxSSEEventBytes = 16 << 20

	defaultQuotaBan        = 30 * time.Minute
	defaultOverloadBan     = 30 * time.Minute
	defaultOverloadTimeout = 10 * time.Second
	defaultRequestTimeout  = 60 * time.Second
)

// ErrQuotaExceeded is returned once every configured key has failed with a
// quota or overload error across every retry cycle.
var ErrQuotaExceeded = errors.New("gemini: all api keys exhausted or overloaded")

var (
	errKeyQuotaExceeded   = errors.New("gemini: key quota exceeded")
	errKeyOverloaded      = errors.New("gemini: key overloaded")
	errKeyOverloadTimeout = errors.New("gemini: key response exceeded overload timeout")
	errNoAudio            = errors.New("gemini: response had no audio content")
)

// statusError is a non-success HTTP status that is neither quota nor
// overload.
type statusError struct {
	code int
	body string
}

func (e *statusError) Error() string {
	return fmt.Sprintf("gemini: unexpected status %d: %s", e.code, e.body)
}

// Config configures a Client. Zero durations fall back to built-in defaults.
type Config struct {
	APIKeys []string
	Model   string
	Voice   string

	RequestTimeout  time.Duration
	QuotaCooldown   time.Duration
	MaxRetryCycles  int
	QuotaBan        time.Duration
	OverloadBan     time.Duration
	OverloadTimeout time.Duration
}

// Client talks to Gemini's speech API, rotating across API keys.
type Client struct {
	cfg    Config
	http   *http.Client
	logger *slog.Logger
	// bans outlives individual calls so a key found exhausted on one request
	// is skipped by the next without rediscovering the failure.
	bans *keyrotation.Banlist
	// streamUnsupported latches once the model rejected the streaming
	// endpoint, so later requests go straight to the buffered path.
	streamUnsupported atomic.Bool
}

// New validates cfg and builds a Client.
func New(cfg Config, logger *slog.Logger) (*Client, error) {
	if len(cfg.APIKeys) == 0 {
		return nil, errors.New("gemini: no api keys configured")
	}
	if cfg.Model == "" || cfg.Voice == "" {
		return nil, errors.New("gemini: model and voice must be set")
	}
	if logger == nil {
		logger = slog.Default()
	}
	cfg.Model = strings.TrimPrefix(cfg.Model, "models/")
	if cfg.RequestTimeout <= 0 {
		cfg.RequestTimeout = defaultRequestTimeout
	}
	if cfg.QuotaBan <= 0 {
		cfg.QuotaBan = defaultQuotaBan
	}
	if cfg.OverloadBan <= 0 {
		cfg.OverloadBan = defaultOverloadBan
	}
	if cfg.OverloadTimeout <= 0 {
		cfg.OverloadTimeout = defaultOverloadTimeout
	}
	return &Client{cfg: cfg, http: &http.Client{}, logger: logger, bans: keyrotation.NewBanlist()}, nil
}

// Model returns the configured model id (used in cache keys).
func (c *Client) Model() string { return c.cfg.Model }

// DefaultVoice returns the configured default voice.
func (c *Client) DefaultVoice() string { return c.cfg.Voice }

// SynthesizeOnce returns the complete raw PCM for text. An empty voice means
// the configured default. Every call logs one summary line (see logRequest).
func (c *Client) SynthesizeOnce(ctx context.Context, text, voice string) ([]byte, error) {
	if voice == "" {
		voice = c.cfg.Voice
	}
	start := time.Now()
	pcm, err := c.synthesizeOnce(ctx, text, voice)
	c.logRequest(requestSummary{mode: "buffered", text: text, voice: voice, start: start, pcmBytes: len(pcm), err: err})
	return pcm, err
}

func (c *Client) synthesizeOnce(ctx context.Context, text, voice string) ([]byte, error) {
	var pcm []byte
	err := c.rotate(ctx, func(ctx context.Context, i int) error {
		out, err := c.requestOnce(ctx, i, text, voice)
		if err != nil {
			return err
		}
		pcm = out
		return nil
	})
	if err != nil {
		return nil, c.translate(err)
	}
	return pcm, nil
}

// SynthesizeStream calls emit with PCM pieces as they arrive. If the model
// does not support streaming (or returns nothing streamable), it falls back
// to one buffered request re-chunked into frames, so callers always get the
// same callback contract. emit returning an error aborts synthesis. Every
// call logs one summary line (see logRequest).
func (c *Client) SynthesizeStream(ctx context.Context, text, voice string, emit func(pcm []byte) error) error {
	if voice == "" {
		voice = c.cfg.Voice
	}
	start := time.Now()
	sum := requestSummary{text: text, voice: voice, start: start}
	counting := func(p []byte) error {
		if sum.pcmBytes == 0 {
			sum.firstAudio = time.Since(start)
		}
		sum.pcmBytes += len(p)
		return emit(p)
	}

	var err error
	sum.mode, err = c.synthesizeStream(ctx, text, voice, counting)
	sum.err = err
	c.logRequest(sum)
	return err
}

// synthesizeStream does the work of SynthesizeStream and reports which path
// served the request: "stream" (Gemini's streaming endpoint) or
// "stream-fallback" (one buffered request re-chunked into frames).
func (c *Client) synthesizeStream(ctx context.Context, text, voice string, emit func(pcm []byte) error) (string, error) {
	if !c.streamUnsupported.Load() {
		emitted := false
		err := c.rotate(ctx, func(ctx context.Context, i int) error {
			err := c.requestStream(ctx, i, text, voice, func(p []byte) error {
				emitted = true
				return emit(p)
			})
			if err != nil && emitted {
				// Audio already reached the caller; rotating keys would
				// replay the utterance. Drop the sentinel chain so it is
				// not retried.
				return fmt.Errorf("gemini: stream aborted mid-response: %v", err)
			}
			return err
		})
		if err == nil {
			return "stream", nil
		}
		if emitted {
			return "stream", c.translate(err)
		}
		var se *statusError
		switch {
		case errors.As(err, &se) && (se.code == http.StatusBadRequest || se.code == http.StatusNotFound),
			errors.Is(err, errNoAudio):
			c.logger.Warn("gemini: streaming not usable for this model, falling back to buffered synthesis", "model", c.cfg.Model, "error", err)
			c.streamUnsupported.Store(true)
		default:
			return "stream", c.translate(err)
		}
	}

	pcm, err := c.synthesizeOnce(ctx, text, voice)
	if err != nil {
		return "stream-fallback", err
	}
	for len(pcm) > 0 {
		n := min(fallbackFrameBytes, len(pcm))
		if err := emit(pcm[:n]); err != nil {
			return "stream-fallback", err
		}
		pcm = pcm[n:]
	}
	return "stream-fallback", nil
}

// rotate runs attempt across the configured keys with the shared ban list.
func (c *Client) rotate(ctx context.Context, attempt func(ctx context.Context, keyIndex int) error) error {
	cfg := keyrotation.Config{Cycles: c.cfg.MaxRetryCycles, Cooldown: c.cfg.QuotaCooldown}
	return keyrotation.Run(ctx, c.logger, "gemini", len(c.cfg.APIKeys), cfg, c.bans, c.classify, attempt)
}

func (c *Client) classify(err error) (retryable bool, ban time.Duration) {
	switch {
	case errors.Is(err, errKeyQuotaExceeded):
		return true, c.cfg.QuotaBan
	case errors.Is(err, errKeyOverloaded), errors.Is(err, errKeyOverloadTimeout):
		return true, c.cfg.OverloadBan
	}
	return false, 0
}

// translate maps the per-key sentinels that survive keyrotation's wrapping to
// the exported ErrQuotaExceeded.
func (c *Client) translate(err error) error {
	if errors.Is(err, errKeyQuotaExceeded) || errors.Is(err, errKeyOverloaded) || errors.Is(err, errKeyOverloadTimeout) {
		return fmt.Errorf("%w: %v", ErrQuotaExceeded, err)
	}
	return err
}

func buildBody(text, voice string) ([]byte, error) {
	body, err := json.Marshal(request{
		Contents: []content{{Parts: []part{{Text: text}}}},
		GenerationConfig: generationConfig{
			ResponseModalities: []string{"AUDIO"},
			SpeechConfig:       speechConfig{VoiceConfig: voiceConfig{PrebuiltVoiceConfig: prebuiltVoiceConfig{VoiceName: voice}}},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("gemini: marshal request: %w", err)
	}
	return body, nil
}

// post sends one request with one key. A server that does not answer with
// response headers within OverloadTimeout counts as overloaded; the whole
// call (including streaming the body) is bounded by RequestTimeout. On
// success the caller must call release once done with resp.Body.
func (c *Client) post(ctx context.Context, keyIndex int, urlFormat, text, voice string) (*http.Response, func(), error) {
	body, err := buildBody(text, voice)
	if err != nil {
		return nil, nil, err
	}

	reqCtx, cancel := context.WithTimeout(ctx, c.cfg.RequestTimeout)
	var overloaded atomic.Bool
	timer := time.AfterFunc(c.cfg.OverloadTimeout, func() {
		overloaded.Store(true)
		cancel()
	})
	release := func() { timer.Stop(); cancel() }

	url := fmt.Sprintf(urlFormat, baseURL, c.cfg.Model)
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		release()
		return nil, nil, fmt.Errorf("gemini: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-goog-api-key", c.cfg.APIKeys[keyIndex])

	resp, err := c.http.Do(req)
	headersInTime := timer.Stop()
	if err != nil {
		release()
		if overloaded.Load() && ctx.Err() == nil {
			return nil, nil, fmt.Errorf("%w: %v", errKeyOverloadTimeout, err)
		}
		return nil, nil, fmt.Errorf("gemini: request: %w", err)
	}
	if !headersInTime && overloaded.Load() {
		_ = resp.Body.Close()
		release()
		return nil, nil, fmt.Errorf("%w: headers arrived after cancellation", errKeyOverloadTimeout)
	}

	if resp.StatusCode != http.StatusOK {
		defer release()
		defer func() { _ = resp.Body.Close() }()
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		var parsed response
		_ = json.Unmarshal(raw, &parsed)
		switch {
		case resp.StatusCode == http.StatusTooManyRequests || (parsed.Error != nil && parsed.Error.Status == "RESOURCE_EXHAUSTED"):
			return nil, nil, fmt.Errorf("%w (http %d): %s", errKeyQuotaExceeded, resp.StatusCode, raw)
		case resp.StatusCode == http.StatusServiceUnavailable && parsed.Error != nil && parsed.Error.Status == "UNAVAILABLE":
			return nil, nil, fmt.Errorf("%w (http %d): %s", errKeyOverloaded, resp.StatusCode, raw)
		}
		return nil, nil, &statusError{code: resp.StatusCode, body: string(raw)}
	}
	return resp, release, nil
}

func (c *Client) requestOnce(ctx context.Context, keyIndex int, text, voice string) (pcm []byte, err error) {
	start := time.Now()
	defer func() { c.logAttempt("buffered", keyIndex, text, start, err) }()

	resp, release, err := c.post(ctx, keyIndex, generateURLFormat, text, voice)
	if err != nil {
		return nil, err
	}
	defer release()
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("gemini: read response: %w", err)
	}
	var parsed response
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, fmt.Errorf("gemini: decode response: %w", err)
	}
	var out []byte
	if err := forEachAudio(parsed, func(p []byte) error { out = append(out, p...); return nil }); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, errNoAudio
	}
	return out, nil
}

func (c *Client) requestStream(ctx context.Context, keyIndex int, text, voice string, emit func([]byte) error) (err error) {
	start := time.Now()
	defer func() { c.logAttempt("stream", keyIndex, text, start, err) }()

	resp, release, err := c.post(ctx, keyIndex, streamGenerateURLFormat, text, voice)
	if err != nil {
		return err
	}
	defer release()
	defer func() { _ = resp.Body.Close() }()

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), maxSSEEventBytes)
	total := 0
	for scanner.Scan() {
		line := scanner.Text()
		data, ok := strings.CutPrefix(line, "data:")
		if !ok {
			continue
		}
		data = strings.TrimSpace(data)
		if data == "" {
			continue
		}
		var parsed response
		if err := json.Unmarshal([]byte(data), &parsed); err != nil {
			return fmt.Errorf("gemini: decode stream event: %w", err)
		}
		err := forEachAudio(parsed, func(p []byte) error {
			total += len(p)
			return emit(p)
		})
		if err != nil {
			return err
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("gemini: read stream: %w", err)
	}
	if total == 0 {
		return errNoAudio
	}
	return nil
}

// forEachAudio decodes every inline audio part in r and passes it to fn. A
// response carrying an error object (possible mid-stream) is returned as an
// error, classified the same way as HTTP-level failures.
func forEachAudio(r response, fn func([]byte) error) error {
	if r.Error != nil {
		switch r.Error.Status {
		case "RESOURCE_EXHAUSTED":
			return fmt.Errorf("%w: %s", errKeyQuotaExceeded, r.Error.Message)
		case "UNAVAILABLE":
			return fmt.Errorf("%w: %s", errKeyOverloaded, r.Error.Message)
		}
		return fmt.Errorf("gemini: api error %d %s: %s", r.Error.Code, r.Error.Status, r.Error.Message)
	}
	for _, cand := range r.Candidates {
		for _, p := range cand.Content.Parts {
			if p.InlineData.Data == "" {
				continue
			}
			pcm, err := base64.StdEncoding.DecodeString(p.InlineData.Data)
			if err != nil {
				return fmt.Errorf("gemini: decode inline audio: %w", err)
			}
			if err := fn(pcm); err != nil {
				return err
			}
		}
	}
	return nil
}

// logAttempt records one attempt's outcome and duration — never the text or
// the key.
func (c *Client) logAttempt(mode string, keyIndex int, text string, start time.Time, err error) {
	fields := []any{
		"mode", mode, "model", c.cfg.Model, "key_index", keyIndex,
		"chars", len([]rune(text)), "duration_ms", time.Since(start).Milliseconds(),
	}
	switch {
	case err == nil:
		// The per-request summary (logRequest) is the Info-level record;
		// this per-attempt detail is only for debugging key rotation.
		c.logger.Debug("gemini: attempt succeeded", fields...)
	case errors.Is(err, errKeyQuotaExceeded):
		c.logger.Warn("gemini: key quota exceeded, trying next key", append(fields, "error", err)...)
	case errors.Is(err, errKeyOverloaded), errors.Is(err, errKeyOverloadTimeout):
		c.logger.Warn("gemini: key overloaded, trying next key", append(fields, "error", err)...)
	default:
		c.logger.Error("gemini: request failed", append(fields, "error", err)...)
	}
}

// requestSummary describes one finished synthesis request for logRequest.
type requestSummary struct {
	mode       string
	text       string
	voice      string
	start      time.Time
	firstAudio time.Duration // time to first PCM piece; 0 for buffered
	pcmBytes   int
	err        error
}

// logRequest writes the one line per synthesis request that records what was
// asked of Gemini and how it went: model, voice, text length (never the
// text), total synthesis time, time to first audio (streaming) and the
// resulting audio length. Failures are logged at Error level so they are
// never missed; a request cut short by the caller (client disconnected,
// stop) is only a Warn since nothing went wrong on our side.
func (c *Client) logRequest(r requestSummary) {
	fields := []any{
		"mode", r.mode, "model", c.cfg.Model, "voice", r.voice,
		"chars", len([]rune(r.text)),
		"synth_ms", time.Since(r.start).Milliseconds(),
		"audio_ms", audiocache.PCMBytesDurationMS(r.pcmBytes),
	}
	if r.mode != "buffered" {
		fields = append(fields, "first_audio_ms", r.firstAudio.Milliseconds())
	}
	switch {
	case r.err == nil:
		c.logger.Info("gemini: synthesized", fields...)
	case errors.Is(r.err, context.Canceled):
		c.logger.Warn("gemini: synthesis cancelled", append(fields, "error", r.err)...)
	default:
		c.logger.Error("gemini: synthesis failed", append(fields, "error", r.err)...)
	}
}
