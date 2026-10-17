// Package speaker plays text aloud on a Home Assistant entity. It is the
// fallback voicing path for requests that did not come through the Assist
// pipeline (Telegram, web UI, reminders): the pipeline path never touches
// this package — HA talks to the Wyoming server directly.
//
// The entity's domain (the part of entity_id before the first dot) selects
// the mechanism:
//
//   - assist_satellite: one assist_satellite.announce call carrying the text;
//     the satellite synthesizes it with its own pipeline's TTS engine (this
//     same service, over Wyoming), so nothing is synthesized here.
//   - media_player (e.g. a Yandex Station): synthesize with Gemini, serve the
//     rendered clip from our /audio route and point media_player.play_media
//     at that URL. Station TTS cannot produce Gemini's voices, so the audio
//     has to be pre-rendered and handed over as a file to fetch.
package speaker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/archer-developer/miranda-tts/internal/audiocache"
	"github.com/archer-developer/miranda-tts/internal/chunk"
)

// playbackSafetyMargin pads a clip's exact duration to cover the delay
// between firing play_media and the device actually rendering audio; waiting
// slightly too long beats starting the next chunk over this one's tail.
const playbackSafetyMargin = 700 * time.Millisecond

// Synthesizer renders text to raw 24 kHz/16-bit/mono PCM.
type Synthesizer interface {
	SynthesizeOnce(ctx context.Context, text, voice string) ([]byte, error)
	Model() string
	DefaultVoice() string
}

// ServiceCaller calls Home Assistant services.
type ServiceCaller interface {
	CallService(ctx context.Context, domain, service string, data map[string]any) error
}

// Result describes what Speak did.
type Result struct {
	// Played is true when the whole text was played.
	Played bool
	// Interrupted is true when a Stop (or a newer Speak on the same entity)
	// cut the playback short.
	Interrupted bool
	// DurationMS is the total length of the audio played, when known.
	DurationMS int64
}

// Config configures a Speaker.
type Config struct {
	// PublicBaseURL is how a media_player reaches this service's /audio route.
	PublicBaseURL string
	ChunkMaxChars int
}

// Speaker implements Speak/Stop. It is safe for concurrent use.
type Speaker struct {
	cfg    Config
	synth  Synthesizer
	ha     ServiceCaller
	cache  *audiocache.Cache
	logger *slog.Logger
	// wait blocks for d or until ctx ends; replaced in tests.
	wait func(ctx context.Context, d time.Duration) error

	mu     sync.Mutex
	seq    uint64
	active map[string]activeCall
}

type activeCall struct {
	id     uint64
	cancel context.CancelFunc
}

// New builds a Speaker.
func New(cfg Config, synth Synthesizer, ha ServiceCaller, cache *audiocache.Cache, logger *slog.Logger) *Speaker {
	if logger == nil {
		logger = slog.Default()
	}
	if cfg.ChunkMaxChars <= 0 {
		cfg.ChunkMaxChars = 200
	}
	return &Speaker{
		cfg: cfg, synth: synth, ha: ha, cache: cache, logger: logger,
		wait:   sleepCtx,
		active: make(map[string]activeCall),
	}
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Speak plays text on entityID, blocking until playback finishes (or is
// interrupted). A newer Speak on the same entity interrupts the running one.
// voice overrides the default voice for media_player playback; satellites
// always use their pipeline's configured voice.
func (s *Speaker) Speak(ctx context.Context, entityID, text, voice string) (Result, error) {
	domain, err := entityDomain(entityID)
	if err != nil {
		return Result{}, err
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return Result{}, errors.New("speaker: text must not be empty")
	}

	ctx, end := s.begin(ctx, entityID)
	defer end()

	switch domain {
	case "assist_satellite":
		return s.announce(ctx, entityID, text)
	case "media_player":
		return s.playOnMediaPlayer(ctx, entityID, text, voice)
	default:
		return Result{}, fmt.Errorf("speaker: unsupported entity domain %q for %s (want assist_satellite or media_player)", domain, entityID)
	}
}

// Stop interrupts a running Speak on entityID and, for media players, stops
// the device. It reports whether anything was stopped. Satellite
// announcements cannot be interrupted through Home Assistant's API, so for
// those Stop only cancels our own wait and reports false.
func (s *Speaker) Stop(ctx context.Context, entityID string) (bool, error) {
	domain, err := entityDomain(entityID)
	if err != nil {
		return false, err
	}

	s.mu.Lock()
	call, running := s.active[entityID]
	s.mu.Unlock()
	if running {
		call.cancel()
	}

	if domain != "media_player" {
		return false, nil
	}
	if err := s.ha.CallService(ctx, "media_player", "media_stop", map[string]any{"entity_id": entityID}); err != nil {
		return running, err
	}
	return true, nil
}

func entityDomain(entityID string) (string, error) {
	domain, name, ok := strings.Cut(entityID, ".")
	if !ok || domain == "" || name == "" {
		return "", fmt.Errorf("speaker: entity_id %q must look like domain.name", entityID)
	}
	return domain, nil
}

// begin registers a cancellable call for entityID, cancelling any previous
// one still running on it.
func (s *Speaker) begin(parent context.Context, entityID string) (context.Context, func()) {
	ctx, cancel := context.WithCancel(parent)

	s.mu.Lock()
	if prev, ok := s.active[entityID]; ok {
		prev.cancel()
	}
	s.seq++
	id := s.seq
	s.active[entityID] = activeCall{id: id, cancel: cancel}
	s.mu.Unlock()

	return ctx, func() {
		s.mu.Lock()
		if cur, ok := s.active[entityID]; ok && cur.id == id {
			delete(s.active, entityID)
		}
		s.mu.Unlock()
		cancel()
	}
}

func (s *Speaker) announce(ctx context.Context, entityID, text string) (Result, error) {
	// preannounce is off: this is a spoken reply, not an attention-getting
	// announcement, so the chime would only add delay.
	err := s.ha.CallService(ctx, "assist_satellite", "announce", map[string]any{
		"entity_id":   entityID,
		"message":     text,
		"preannounce": false,
	})
	if err != nil {
		if ctx.Err() != nil {
			return Result{Interrupted: true}, nil
		}
		return Result{}, err
	}
	return Result{Played: true}, nil
}

type renderedChunk struct {
	url      string
	duration time.Duration
	err      error
}

func (s *Speaker) playOnMediaPlayer(ctx context.Context, entityID, text, voice string) (Result, error) {
	if s.cfg.PublicBaseURL == "" {
		return Result{}, errors.New("speaker: public_base_url is not configured, the media player has no way to fetch synthesized audio")
	}
	if voice == "" {
		voice = s.synth.DefaultVoice()
	}

	// Synthesis runs one chunk ahead of playback: both hit different
	// services, so there is no reason to serialize them. A channel buffer
	// of 1 keeps the synthesizer exactly one chunk ahead.
	synthCtx, stopSynth := context.WithCancel(ctx)
	ch := make(chan renderedChunk, 1)
	// Cancel, then drain until the producer closes ch, so no synthesis (or
	// cache write) outlives this call.
	defer func() {
		stopSynth()
		for range ch {
		}
	}()
	go func() {
		defer close(ch)
		for i, piece := range chunk.Chunk(text, s.cfg.ChunkMaxChars) {
			start := time.Now()
			url, d, cached, err := s.render(synthCtx, piece, voice)
			if err == nil {
				// One line per chunk says whether the clip cache was used, so a
				// hit (no Gemini call, no quota spent) is visible in the log.
				s.logger.Info("speaker: chunk rendered", "index", i, "cache", cacheOutcome(cached),
					"model", s.synth.Model(), "voice", voice, "chars", len([]rune(piece)),
					"synth_ms", time.Since(start).Milliseconds(), "audio_ms", d.Milliseconds())
			}
			select {
			case ch <- renderedChunk{url, d, err}:
			case <-synthCtx.Done():
				return
			}
		}
	}()

	var total time.Duration
	for r := range ch {
		if r.err != nil {
			if ctx.Err() != nil {
				return Result{Interrupted: true, DurationMS: total.Milliseconds()}, nil
			}
			return Result{DurationMS: total.Milliseconds()}, r.err
		}
		// media_content_type is irrelevant to AlexxIT/YandexStation for URL
		// playback (it dispatches on "http(s):// in media_id"); "music"
		// reads clearly in logs. Never "dialog": that reopens the mic.
		err := s.ha.CallService(ctx, "media_player", "play_media", map[string]any{
			"entity_id":          entityID,
			"media_content_id":   r.url,
			"media_content_type": "music",
		})
		if err != nil {
			if ctx.Err() != nil {
				return Result{Interrupted: true, DurationMS: total.Milliseconds()}, nil
			}
			return Result{DurationMS: total.Milliseconds()}, err
		}
		// Station playback never changes alice_state for URL playback, so
		// there is nothing to poll: wait out the clip's exact duration.
		if err := s.wait(ctx, r.duration+playbackSafetyMargin); err != nil {
			return Result{Interrupted: true, DurationMS: total.Milliseconds()}, nil
		}
		total += r.duration
	}
	if ctx.Err() != nil {
		return Result{Interrupted: true, DurationMS: total.Milliseconds()}, nil
	}
	return Result{Played: true, DurationMS: total.Milliseconds()}, nil
}

// render returns the public URL and exact duration of text's clip, synthesizing
// and caching it first unless this exact (model, voice, text) was rendered
// before — a hit skips the quota-limited Gemini call entirely.
func (s *Speaker) render(ctx context.Context, text, voice string) (url string, d time.Duration, cached bool, err error) {
	key := audiocache.Key(s.synth.Model(), voice, text)
	url = strings.TrimRight(s.cfg.PublicBaseURL, "/") + "/audio/" + key + "." + audiocache.Ext

	if s.cache.Has(key) {
		ms, _ := s.cache.Duration(key)
		return url, time.Duration(ms) * time.Millisecond, true, nil
	}

	pcm, err := s.synth.SynthesizeOnce(ctx, text, voice)
	if err != nil {
		return "", 0, false, err
	}
	ms := audiocache.PCMDurationMS(pcm)
	if err := s.cache.Store(key, audiocache.WrapWAV(pcm), ms); err != nil {
		return "", 0, false, err
	}
	return url, time.Duration(ms) * time.Millisecond, false, nil
}

func cacheOutcome(cached bool) string {
	if cached {
		return "hit"
	}
	return "miss"
}
