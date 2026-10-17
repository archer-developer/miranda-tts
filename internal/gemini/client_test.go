package gemini

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func audioJSON(pcm ...[]byte) string {
	var parts []string
	for _, p := range pcm {
		parts = append(parts, fmt.Sprintf(`{"inlineData":{"mimeType":"audio/L16;rate=24000","data":%q}}`, base64.StdEncoding.EncodeToString(p)))
	}
	return fmt.Sprintf(`{"candidates":[{"content":{"parts":[%s]}}]}`, strings.Join(parts, ","))
}

func newTestClient(t *testing.T, handler http.HandlerFunc, keys ...string) *Client {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	old := baseURL
	baseURL = srv.URL
	t.Cleanup(func() { baseURL = old })

	if len(keys) == 0 {
		keys = []string{"k1"}
	}
	c, err := New(Config{APIKeys: keys, Model: "models/test-tts", Voice: "Kore", OverloadTimeout: time.Second}, nil)
	require.NoError(t, err)
	return c
}

func TestNew_RequiresKeysModelVoice(t *testing.T) {
	_, err := New(Config{Model: "m", Voice: "v"}, nil)
	assert.Error(t, err)
	_, err = New(Config{APIKeys: []string{"k"}, Voice: "v"}, nil)
	assert.Error(t, err)
}

func TestSynthesizeOnce_SendsVoiceKeyAndReturnsPCM(t *testing.T) {
	var gotKey, gotPath string
	var gotBody request
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotKey, gotPath = r.Header.Get("x-goog-api-key"), r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_, _ = io.WriteString(w, audioJSON([]byte{1, 2}, []byte{3, 4}))
	})

	pcm, err := c.SynthesizeOnce(context.Background(), "привет", "")

	require.NoError(t, err)
	assert.Equal(t, []byte{1, 2, 3, 4}, pcm)
	assert.Equal(t, "k1", gotKey)
	assert.Equal(t, "/v1beta/models/test-tts:generateContent", gotPath)
	assert.Equal(t, "Kore", gotBody.GenerationConfig.SpeechConfig.VoiceConfig.PrebuiltVoiceConfig.VoiceName)
	assert.Equal(t, "привет", gotBody.Contents[0].Parts[0].Text)
}

func TestSynthesizeOnce_RotatesToNextKeyOnQuota(t *testing.T) {
	var mu sync.Mutex
	var seen []string
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		key := r.Header.Get("x-goog-api-key")
		mu.Lock()
		seen = append(seen, key)
		mu.Unlock()
		if key == "k1" {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		_, _ = io.WriteString(w, audioJSON([]byte{9, 9}))
	}, "k1", "k2")

	pcm, err := c.SynthesizeOnce(context.Background(), "x", "")
	require.NoError(t, err)
	assert.Equal(t, []byte{9, 9}, pcm)

	// k1 is now banned: the next call goes straight to k2.
	_, err = c.SynthesizeOnce(context.Background(), "y", "")
	require.NoError(t, err)
	assert.Equal(t, []string{"k1", "k2", "k2"}, seen)
}

func TestSynthesizeOnce_AllKeysExhaustedIsErrQuotaExceeded(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}, "k1", "k2")

	_, err := c.SynthesizeOnce(context.Background(), "x", "")

	assert.ErrorIs(t, err, ErrQuotaExceeded)
}

func TestSynthesizeOnce_OtherErrorsDoNotRotate(t *testing.T) {
	calls := 0
	c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		calls++
		http.Error(w, "bad key", http.StatusForbidden)
	}, "k1", "k2")

	_, err := c.SynthesizeOnce(context.Background(), "x", "")

	assert.ErrorContains(t, err, "status 403")
	assert.Equal(t, 1, calls)
}

func TestSynthesizeOnce_SlowHeadersCountAsOverload(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-goog-api-key") == "k1" {
			select {
			case <-time.After(400 * time.Millisecond):
			case <-r.Context().Done():
			}
			return
		}
		_, _ = io.WriteString(w, audioJSON([]byte{7, 7}))
	}, "k1", "k2")
	c.cfg.OverloadTimeout = 100 * time.Millisecond

	pcm, err := c.SynthesizeOnce(context.Background(), "x", "")

	require.NoError(t, err)
	assert.Equal(t, []byte{7, 7}, pcm)
}

func TestSynthesizeStream_EmitsEachSSEEventAsItArrives(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/v1beta/models/test-tts:streamGenerateContent", r.URL.Path)
		assert.Equal(t, "sse", r.URL.Query().Get("alt"))
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprintf(w, "data: %s\n\n", audioJSON([]byte{1}))
		w.(http.Flusher).Flush()
		_, _ = fmt.Fprintf(w, "data: %s\n\n", audioJSON([]byte{2}))
	})

	var got [][]byte
	err := c.SynthesizeStream(context.Background(), "x", "", func(p []byte) error {
		got = append(got, append([]byte(nil), p...))
		return nil
	})

	require.NoError(t, err)
	assert.Equal(t, [][]byte{{1}, {2}}, got)
}

func TestSynthesizeStream_FallsBackToBufferedAndRechunks(t *testing.T) {
	big := make([]byte, fallbackFrameBytes*2+10)
	streamCalls := 0
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "streamGenerateContent") {
			streamCalls++
			http.Error(w, "streaming unsupported", http.StatusBadRequest)
			return
		}
		_, _ = io.WriteString(w, audioJSON(big))
	})

	var sizes []int
	emit := func(p []byte) error { sizes = append(sizes, len(p)); return nil }
	require.NoError(t, c.SynthesizeStream(context.Background(), "x", "", emit))
	require.NoError(t, c.SynthesizeStream(context.Background(), "y", "", emit))

	assert.Equal(t, 1, streamCalls, "after one rejection the streaming endpoint is not retried")
	assert.Equal(t, []int{fallbackFrameBytes, fallbackFrameBytes, 10, fallbackFrameBytes, fallbackFrameBytes, 10}, sizes)
}

func TestSynthesizeStream_EmitErrorAbortsWithoutRotating(t *testing.T) {
	calls := 0
	c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		calls++
		_, _ = fmt.Fprintf(w, "data: %s\n\n", audioJSON([]byte{1}))
	}, "k1", "k2")

	err := c.SynthesizeStream(context.Background(), "x", "", func([]byte) error { return fmt.Errorf("client gone") })

	assert.ErrorContains(t, err, "client gone")
	assert.Equal(t, 1, calls)
}

// capture routes the client's logs to a buffer and returns the lines that
// match substr.
func capture(t *testing.T, c *Client) func(substr string) []string {
	t.Helper()
	var mu sync.Mutex
	var buf strings.Builder
	c.logger = slog.New(slog.NewTextHandler(&lockedWriter{mu: &mu, b: &buf}, &slog.HandlerOptions{Level: slog.LevelDebug}))
	return func(substr string) []string {
		mu.Lock()
		defer mu.Unlock()
		var out []string
		for _, l := range strings.Split(buf.String(), "\n") {
			if strings.Contains(l, substr) {
				out = append(out, l)
			}
		}
		return out
	}
}

type lockedWriter struct {
	mu *sync.Mutex
	b  *strings.Builder
}

func (w *lockedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.Write(p)
}

func TestLogging_BufferedRequestLogsModelVoiceAndTiming(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, audioJSON(make([]byte, 48000))) // 1 s
	})
	lines := capture(t, c)

	_, err := c.SynthesizeOnce(context.Background(), "привет", "Puck")
	require.NoError(t, err)

	got := lines("gemini: synthesized")
	require.Len(t, got, 1, "exactly one summary line per request")
	for _, want := range []string{"level=INFO", "mode=buffered", "model=test-tts", "voice=Puck", "chars=6", "synth_ms=", "audio_ms=1000"} {
		assert.Contains(t, got[0], want)
	}
	assert.NotContains(t, got[0], "привет", "the text itself must never be logged")
}

func TestLogging_DefaultVoiceIsLoggedResolved(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, audioJSON([]byte{1, 2})) })
	lines := capture(t, c)

	_, err := c.SynthesizeOnce(context.Background(), "x", "")
	require.NoError(t, err)

	assert.Contains(t, lines("gemini: synthesized")[0], "voice=Kore")
}

func TestLogging_FailureIsLoggedAtErrorLevelWithModelAndVoice(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "bad key", http.StatusForbidden) })
	lines := capture(t, c)

	_, err := c.SynthesizeOnce(context.Background(), "x", "")
	require.Error(t, err)

	got := lines("gemini: synthesis failed")
	require.Len(t, got, 1)
	for _, want := range []string{"level=ERROR", "model=test-tts", "voice=Kore", "synth_ms=", "error="} {
		assert.Contains(t, got[0], want)
	}
	assert.Empty(t, lines("gemini: synthesized"))
}

func TestLogging_StreamLogsFirstAudioTime(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprintf(w, "data: %s\n\n", audioJSON(make([]byte, 4800)))
	})
	lines := capture(t, c)

	require.NoError(t, c.SynthesizeStream(context.Background(), "x", "", func([]byte) error { return nil }))

	got := lines("gemini: synthesized")
	require.Len(t, got, 1)
	for _, want := range []string{"mode=stream ", "first_audio_ms=", "audio_ms=100", "voice=Kore"} {
		assert.Contains(t, got[0], want)
	}
}

func TestLogging_StreamFallbackLogsOneSummaryNotTwo(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "streamGenerateContent") {
			http.Error(w, "unsupported", http.StatusBadRequest)
			return
		}
		_, _ = io.WriteString(w, audioJSON(make([]byte, 4800)))
	})
	lines := capture(t, c)

	require.NoError(t, c.SynthesizeStream(context.Background(), "x", "", func([]byte) error { return nil }))

	got := lines("gemini: synthesized")
	require.Len(t, got, 1)
	assert.Contains(t, got[0], "mode=stream-fallback")
	assert.NotEmpty(t, lines("streaming not usable"))
}
