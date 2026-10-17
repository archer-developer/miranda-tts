package httpserver

import (
	"net/http"

	"github.com/archer-developer/miranda-tts/internal/audiocache"
)

// AudioHandler serves cached clips by "<sha256-hex>.wav" filename. The
// pattern check is what stands between the untrusted URL path and a
// filesystem read, so it rejects anything but that exact shape (including
// path traversal) before building a path.
func AudioHandler(cache *audiocache.Cache) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		filename := r.PathValue("filename")
		if !audiocache.FilenamePattern.MatchString(filename) {
			http.Error(w, "invalid audio filename", http.StatusBadRequest)
			return
		}
		key := filename[:audiocache.KeyHexLen]
		w.Header().Set("Content-Type", "audio/wav")
		http.ServeFile(w, r, cache.Path(key))
	})
}
