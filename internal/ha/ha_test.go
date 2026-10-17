package ha

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCallService_PostsJSONWithBearer(t *testing.T) {
	var gotPath, gotAuth string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotAuth = r.URL.Path, r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
	}))
	defer srv.Close()

	err := New(srv.URL+"/", "tok").CallService(context.Background(), "media_player", "play_media", map[string]any{"entity_id": "media_player.x"})

	require.NoError(t, err)
	assert.Equal(t, "/api/services/media_player/play_media", gotPath)
	assert.Equal(t, "Bearer tok", gotAuth)
	assert.Equal(t, "media_player.x", gotBody["entity_id"])
}

func TestCallService_NonSuccessStatusIsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "nope", http.StatusUnauthorized)
	}))
	defer srv.Close()

	err := New(srv.URL, "bad").CallService(context.Background(), "a", "b", nil)

	assert.ErrorContains(t, err, "status 401")
}
