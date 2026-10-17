package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDefault_IsValid(t *testing.T) {
	assert.NoError(t, Default().validate())
}

func TestLoad_MissingFileReturnsDefaults(t *testing.T) {
	cfg, err := Load(filepath.Join(t.TempDir(), "does-not-exist.yaml"))
	require.NoError(t, err)
	assert.Equal(t, Default(), cfg)
}

func TestLoad_OverridesMergeOverDefaults(t *testing.T) {
	path := writeConfigFile(t, "http_addr: \":9999\"\n")

	cfg, err := Load(path)
	require.NoError(t, err)
	assert.Equal(t, ":9999", cfg.HTTPAddr)
	assert.Equal(t, Default().AuthTokenEnv, cfg.AuthTokenEnv, "unset fields keep their default")
}

func TestLoad_InvalidYAMLIsError(t *testing.T) {
	path := writeConfigFile(t, "http_addr: [not a string\n")

	_, err := Load(path)
	assert.Error(t, err)
}

func TestLoad_MultipleFilesMergeInOrderLaterWins(t *testing.T) {
	base := writeConfigFile(t, "http_addr: \":9999\"\nlogging:\n  level: \"debug\"\n")
	override := writeConfigFile(t, "logging:\n  level: \"warn\"\n")

	cfg, err := Load(base, override)
	require.NoError(t, err)

	// override.yaml only touches logging.level; http_addr from base.yaml
	// survives because yaml.Unmarshal only overwrites fields present in the
	// document being applied.
	assert.Equal(t, ":9999", cfg.HTTPAddr)
	assert.Equal(t, "warn", cfg.Logging.Level)
}

func TestLoad_MissingFileAmongExistingOnesIsSkipped(t *testing.T) {
	existing := writeConfigFile(t, "http_addr: \":9999\"\n")
	missing := filepath.Join(t.TempDir(), "does-not-exist.yaml")

	cfg, err := Load(missing, existing)
	require.NoError(t, err)
	assert.Equal(t, ":9999", cfg.HTTPAddr)
}

func TestValidate(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*Config)
		wantErr bool
	}{
		{"valid default", func(*Config) {}, false},
		{"empty http_addr", func(c *Config) { c.HTTPAddr = "" }, true},
		{"empty auth_token_env", func(c *Config) { c.AuthTokenEnv = "" }, true},
		{"empty wyoming_addr", func(c *Config) { c.WyomingAddr = "" }, true},
		{"no gemini key envs", func(c *Config) { c.GeminiAPIKeyEnvs = nil }, true},
		{"empty gemini_model", func(c *Config) { c.GeminiModel = "" }, true},
		{"empty gemini_voice", func(c *Config) { c.GeminiVoice = "" }, true},
		{"non-positive chunk_max_chars", func(c *Config) { c.ChunkMaxChars = 0 }, true},
		{"empty audio_cache_dir", func(c *Config) { c.AudioCacheDir = "" }, true},
		{"invalid logging level", func(c *Config) { c.Logging.Level = "verbose" }, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := Default()
			tt.mutate(&cfg)
			err := cfg.validate()
			if tt.wantErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func writeConfigFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	return path
}

func TestLoad_KeyEnvListIsReplacedWholesale(t *testing.T) {
	path := writeConfigFile(t, "gemini_api_key_envs: [\"A\", \"B\"]\n")

	cfg, err := Load(path)
	require.NoError(t, err)
	assert.Equal(t, []string{"A", "B"}, cfg.GeminiAPIKeyEnvs)
}
