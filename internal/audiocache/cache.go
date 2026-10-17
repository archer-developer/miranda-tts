// Package audiocache stores rendered speech clips on disk, content-addressed
// by exactly the inputs that determine the audio (model, voice, text), and
// wraps raw PCM in a WAV container.
//
// The cache is permanent on purpose — no TTL, no eviction: a hit skips a
// real, quota-limited Gemini call, and disk growth on a home-infra box is an
// accepted trade-off (same reasoning as the cache this was ported from in
// the main miranda repo).
package audiocache

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// Ext is the only container this service renders.
const Ext = "wav"

// KeyHexLen is the length of a Key string: a hex-encoded sha256 digest.
const KeyHexLen = sha256.Size * 2

// FilenamePattern matches exactly "<sha256-hex>.wav", the only shape the HTTP
// route accepts before it builds a filesystem path from untrusted input.
var FilenamePattern = regexp.MustCompile(fmt.Sprintf(`^[0-9a-f]{%d}\.%s$`, KeyHexLen, Ext))

// Cache is a directory of rendered clips plus a plain-text duration sidecar
// per clip.
type Cache struct {
	dir string
}

// New ensures dir exists and returns a Cache rooted there.
func New(dir string) (*Cache, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("audiocache: create dir %s: %w", dir, err)
	}
	return &Cache{dir: dir}, nil
}

// Key hashes exactly what determines the resulting audio, so the same text
// under another model or voice never collides with it.
func Key(model, voice, text string) string {
	sum := sha256.Sum256([]byte(model + "|" + voice + "|" + text))
	return hex.EncodeToString(sum[:])
}

// Path returns the on-disk location of key's clip.
func (c *Cache) Path(key string) string { return filepath.Join(c.dir, key+"."+Ext) }

func (c *Cache) durationPath(key string) string { return filepath.Join(c.dir, key+".dur") }

// Has reports whether key's clip and duration sidecar are both present.
func (c *Cache) Has(key string) bool {
	if _, err := os.Stat(c.Path(key)); err != nil {
		return false
	}
	_, ok := c.Duration(key)
	return ok
}

// Store persists a clip and its exact playback duration in milliseconds. The
// clip is written last so Has never reports a clip whose sidecar is missing.
func (c *Cache) Store(key string, wav []byte, durationMS int) error {
	if err := os.WriteFile(c.durationPath(key), []byte(strconv.Itoa(durationMS)), 0o644); err != nil {
		return fmt.Errorf("audiocache: write duration for %s: %w", key, err)
	}
	tmp := c.Path(key) + ".tmp"
	if err := os.WriteFile(tmp, wav, 0o644); err != nil {
		return fmt.Errorf("audiocache: write clip for %s: %w", key, err)
	}
	if err := os.Rename(tmp, c.Path(key)); err != nil {
		return fmt.Errorf("audiocache: publish clip for %s: %w", key, err)
	}
	return nil
}

// Duration reads back key's stored playback duration in milliseconds.
func (c *Cache) Duration(key string) (ms int, ok bool) {
	data, err := os.ReadFile(c.durationPath(key))
	if err != nil {
		return 0, false
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		return 0, false
	}
	return n, true
}
