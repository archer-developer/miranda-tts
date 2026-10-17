package speaker

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/archer-developer/miranda-tts/internal/audiocache"
)

type fakeSynth struct {
	mu    sync.Mutex
	calls []string
	err   error
}

func (f *fakeSynth) SynthesizeOnce(_ context.Context, text, _ string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, text)
	if f.err != nil {
		return nil, f.err
	}
	return make([]byte, audiocache.PCMSampleRate*2/2), nil // 500 ms
}
func (f *fakeSynth) Model() string        { return "m" }
func (f *fakeSynth) DefaultVoice() string { return "Kore" }

type haCall struct {
	domain, service string
	data            map[string]any
}

type fakeHA struct {
	mu    sync.Mutex
	calls []haCall
	err   error
}

func (f *fakeHA) CallService(_ context.Context, domain, service string, data map[string]any) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, haCall{domain, service, data})
	return f.err
}

func newSpeaker(t *testing.T, synth *fakeSynth, ha *fakeHA) *Speaker {
	t.Helper()
	cache, err := audiocache.New(t.TempDir())
	require.NoError(t, err)
	s := New(Config{PublicBaseURL: "http://tts:8794/", ChunkMaxChars: 20}, synth, ha, cache, slog.New(slog.NewTextHandler(io.Discard, nil)))
	s.wait = func(context.Context, time.Duration) error { return nil }
	return s
}

func TestSpeak_SatelliteAnnouncesTextWithoutSynthesizing(t *testing.T) {
	synth, ha := &fakeSynth{}, &fakeHA{}
	s := newSpeaker(t, synth, ha)

	res, err := s.Speak(context.Background(), "assist_satellite.kitchen", "Привет", "")

	require.NoError(t, err)
	assert.True(t, res.Played)
	assert.Empty(t, synth.calls)
	require.Len(t, ha.calls, 1)
	assert.Equal(t, "announce", ha.calls[0].service)
	assert.Equal(t, "Привет", ha.calls[0].data["message"])
	assert.Equal(t, false, ha.calls[0].data["preannounce"])
}

func TestSpeak_MediaPlayerSynthesizesChunksAndPlaysUrls(t *testing.T) {
	synth, ha := &fakeSynth{}, &fakeHA{}
	s := newSpeaker(t, synth, ha)

	res, err := s.Speak(context.Background(), "media_player.station", "Первое предложение. Второе предложение.", "")

	require.NoError(t, err)
	assert.True(t, res.Played)
	assert.Equal(t, []string{"Первое предложение.", "Второе предложение."}, synth.calls)
	require.Len(t, ha.calls, 2)
	for _, c := range ha.calls {
		assert.Equal(t, "play_media", c.service)
		assert.Equal(t, "music", c.data["media_content_type"])
		assert.Contains(t, c.data["media_content_id"], "http://tts:8794/audio/")
		assert.Contains(t, c.data["media_content_id"], ".wav")
	}
	assert.Equal(t, int64(1000), res.DurationMS)
}

func TestSpeak_CachedClipIsNotSynthesizedTwice(t *testing.T) {
	synth, ha := &fakeSynth{}, &fakeHA{}
	s := newSpeaker(t, synth, ha)

	_, err := s.Speak(context.Background(), "media_player.station", "Один раз.", "")
	require.NoError(t, err)
	_, err = s.Speak(context.Background(), "media_player.station", "Один раз.", "")
	require.NoError(t, err)

	assert.Len(t, synth.calls, 1)
	assert.Len(t, ha.calls, 2)
}

func TestSpeak_SynthesisFailureIsReturned(t *testing.T) {
	s := newSpeaker(t, &fakeSynth{err: errors.New("quota")}, &fakeHA{})

	_, err := s.Speak(context.Background(), "media_player.station", "x", "")

	assert.ErrorContains(t, err, "quota")
}

func TestSpeak_RejectsBadInput(t *testing.T) {
	s := newSpeaker(t, &fakeSynth{}, &fakeHA{})
	ctx := context.Background()

	_, err := s.Speak(ctx, "nodot", "x", "")
	assert.Error(t, err)
	_, err = s.Speak(ctx, "light.kitchen", "x", "")
	assert.ErrorContains(t, err, "unsupported entity domain")
	_, err = s.Speak(ctx, "media_player.x", "   ", "")
	assert.ErrorContains(t, err, "empty")
}

func TestSpeak_MediaPlayerWithoutPublicBaseURLFails(t *testing.T) {
	s := newSpeaker(t, &fakeSynth{}, &fakeHA{})
	s.cfg.PublicBaseURL = ""

	_, err := s.Speak(context.Background(), "media_player.x", "x", "")

	assert.ErrorContains(t, err, "public_base_url")
}

func TestStop_InterruptsRunningSpeakAndStopsPlayer(t *testing.T) {
	synth, ha := &fakeSynth{}, &fakeHA{}
	s := newSpeaker(t, synth, ha)
	waiting := make(chan struct{})
	s.wait = func(ctx context.Context, _ time.Duration) error {
		close(waiting)
		<-ctx.Done()
		return ctx.Err()
	}

	type out struct {
		res Result
		err error
	}
	done := make(chan out, 1)
	go func() {
		res, err := s.Speak(context.Background(), "media_player.station", "Длинный текст. Ещё предложение. И ещё одно.", "")
		done <- out{res, err}
	}()
	<-waiting

	stopped, err := s.Stop(context.Background(), "media_player.station")
	require.NoError(t, err)
	assert.True(t, stopped)

	got := <-done
	require.NoError(t, got.err)
	assert.True(t, got.res.Interrupted)
	assert.False(t, got.res.Played)

	ha.mu.Lock()
	defer ha.mu.Unlock()
	last := ha.calls[len(ha.calls)-1]
	assert.Equal(t, "media_stop", last.service)
}

func TestStop_SatelliteReportsNotStopped(t *testing.T) {
	ha := &fakeHA{}
	s := newSpeaker(t, &fakeSynth{}, ha)

	stopped, err := s.Stop(context.Background(), "assist_satellite.kitchen")

	require.NoError(t, err)
	assert.False(t, stopped)
	assert.Empty(t, ha.calls)
}

func TestSpeak_EveryChunkLogsCacheOutcomeModelAndVoice(t *testing.T) {
	var buf strings.Builder
	s := newSpeaker(t, &fakeSynth{}, &fakeHA{})
	s.logger = slog.New(slog.NewTextHandler(&buf, nil))

	_, err := s.Speak(context.Background(), "media_player.station", "Один раз.", "Puck")
	require.NoError(t, err)
	first := buf.String()
	assert.Contains(t, first, `msg="speaker: chunk rendered"`)
	assert.Contains(t, first, "cache=miss")

	buf.Reset()
	_, err = s.Speak(context.Background(), "media_player.station", "Один раз.", "Puck")
	require.NoError(t, err)
	second := buf.String()

	assert.Contains(t, second, "cache=hit")
	assert.NotContains(t, second, "cache=miss")
	for _, want := range []string{"model=m", "voice=Puck", "synth_ms=", "audio_ms=500"} {
		assert.Contains(t, second, want)
	}
}
