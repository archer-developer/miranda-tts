package httpserver

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/archer-developer/miranda-tts/internal/audiocache"
)

func testHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("mcp-ok"))
	})
}

func TestHealthz_Unauthenticated(t *testing.T) {
	handler := New(testHandler(), "secret-token", testHandler())

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "ok\n", rec.Body.String())
}

func TestMCP_MissingAuthHeader(t *testing.T) {
	handler := New(testHandler(), "secret-token", testHandler())

	req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}

func TestMCP_WrongToken(t *testing.T) {
	handler := New(testHandler(), "secret-token", testHandler())

	req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	req.Header.Set("Authorization", "Bearer wrong-token")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}

func TestMCP_CorrectToken(t *testing.T) {
	handler := New(testHandler(), "secret-token", testHandler())

	req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	req.Header.Set("Authorization", "Bearer secret-token")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "mcp-ok", rec.Body.String())
}

func TestMCP_EmptyConfiguredTokenFailsClosed(t *testing.T) {
	handler := New(testHandler(), "", testHandler())

	req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	req.Header.Set("Authorization", "Bearer ")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

func TestAudio_ServesCachedClipWithoutAuth(t *testing.T) {
	cache, err := audiocache.New(t.TempDir())
	require.NoError(t, err)
	key := audiocache.Key("m", "v", "t")
	require.NoError(t, cache.Store(key, []byte("RIFFdata"), 10))
	handler := New(testHandler(), "secret-token", AudioHandler(cache))

	req := httptest.NewRequest(http.MethodGet, "/audio/"+key+".wav", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "RIFFdata", rec.Body.String())
	assert.Equal(t, "audio/wav", rec.Header().Get("Content-Type"))
}

func TestAudio_RejectsMalformedFilenames(t *testing.T) {
	cache, err := audiocache.New(t.TempDir())
	require.NoError(t, err)
	handler := New(testHandler(), "secret-token", AudioHandler(cache))

	for _, name := range []string{"..%2f..%2fetc%2fpasswd", "abc.wav", audiocache.Key("m", "v", "t") + ".mp3"} {
		req := httptest.NewRequest(http.MethodGet, "/audio/"+name, nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		assert.Equal(t, http.StatusBadRequest, rec.Code, name)
	}
}

func TestAudio_MissingClipIs404(t *testing.T) {
	cache, err := audiocache.New(t.TempDir())
	require.NoError(t, err)
	handler := New(testHandler(), "secret-token", AudioHandler(cache))

	req := httptest.NewRequest(http.MethodGet, "/audio/"+audiocache.Key("m", "v", "none")+".wav", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusNotFound, rec.Code)
}
