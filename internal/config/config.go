// Package config loads the service's YAML configuration and merges it over
// built-in defaults, so the service runs with sane behavior even with no
// config files at all. Default() populates every field, Load() applies
// yaml.Unmarshal on top of it once per file for a cheap partial merge,
// validate() rejects values that Default() would never produce but a
// hand-edited file could.
//
// config/config.yaml.dist is the single source of truth for every available
// field, checked into git with a comment on each one. Everything else under
// config/*.yaml is a real, gitignored override file the service actually
// loads — there can be any number of them (e.g. config.yaml for the bulk of
// it, plus a separate secrets-adjacent-but-not-secret file), each applied in
// the order cmd/<service>/main.go's configFilePaths discovers them
// (alphabetical, via os.ReadDir), later files overriding earlier ones
// field-by-field. This mirrors miranda's internal/config exactly — see that
// package's doc comment for the same pattern applied to a much larger
// config tree.
package config

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// Config is the root of the service's configuration tree.
//
// Secrets never live here: AuthTokenEnv and GeminiAPIKeyEnvs hold only the
// *names* of environment variables, resolved by cmd/miranda-tts/main.go at
// startup. HA_BASE_URL / HA_TOKEN are read straight from the environment
// there as well (same convention as miranda's own TTS wiring).
type Config struct {
	// HTTPAddr serves /healthz, the bearer-gated /mcp endpoint and the
	// unauthenticated GET /audio/{filename} route media players fetch from.
	HTTPAddr     string `yaml:"http_addr"`
	AuthTokenEnv string `yaml:"auth_token_env"`
	// WyomingAddr is the TCP listener Home Assistant's built-in "wyoming"
	// integration connects to (it becomes the Assist pipeline's TTS engine).
	WyomingAddr string `yaml:"wyoming_addr"`

	// GeminiAPIKeyEnvs lists env var names, each holding one Gemini API key;
	// keys are rotated on quota/overload errors.
	GeminiAPIKeyEnvs []string `yaml:"gemini_api_key_envs"`
	// GeminiModel is the speech-synthesis model id used for every request.
	GeminiModel string `yaml:"gemini_model"`
	// GeminiVoice is the default prebuilt voice (e.g. "Kore"); callers may
	// override it per request.
	GeminiVoice string `yaml:"gemini_voice"`
	// Languages are advertised to Home Assistant over Wyoming as the
	// languages the voice can speak.
	Languages []string `yaml:"languages"`

	// ChunkMaxChars bounds one synthesis request on the media_player path
	// (several short sentences are packed into one chunk).
	ChunkMaxChars int `yaml:"chunk_max_chars"`
	// PublicBaseURL is how a media_player reaches this service's /audio route.
	PublicBaseURL string `yaml:"public_base_url"`
	// AudioCacheDir holds rendered clips (content-addressed, never evicted).
	AudioCacheDir string `yaml:"audio_cache_dir"`

	RequestTimeoutSeconds          int `yaml:"request_timeout_seconds"`
	QuotaCooldownSeconds           int `yaml:"quota_cooldown_seconds"`
	MaxQuotaRetryCycles            int `yaml:"max_quota_retry_cycles"`
	QuotaBanMinutes                int `yaml:"quota_ban_minutes"`
	OverloadBanMinutes             int `yaml:"overload_ban_minutes"`
	OverloadResponseTimeoutSeconds int `yaml:"overload_response_timeout_seconds"`

	Logging LoggingConfig `yaml:"logging"`
}

// LoggingConfig controls slog output level and verbosity.
type LoggingConfig struct {
	// Level is one of "debug", "info", "warn", "error". At "debug", verbose
	// per-request logs are routed to logs/debug.log instead of stdout to
	// avoid flooding the systemd journal — see cmd/miranda-tts/main.go.
	Level string `yaml:"level"`
}

// Default returns the built-in configuration. Every field has a safe,
// runnable value except the Gemini key env names, which only name env vars
// the deployment must populate.
func Default() Config {
	return Config{
		HTTPAddr:     ":8794",
		AuthTokenEnv: "MIRANDA_TTS_MCP_TOKEN",
		WyomingAddr:  ":10400",

		GeminiAPIKeyEnvs: []string{"GEMINI_API_KEY_1"},
		GeminiModel:      "gemini-3.8-flash-lite-tts",
		GeminiVoice:      "Kore",
		Languages:        []string{"ru", "en", "de", "fr", "es", "it", "pt", "uk", "pl", "tr", "ja", "ko", "zh"},

		ChunkMaxChars: 200,
		AudioCacheDir: "data/audio-cache",

		RequestTimeoutSeconds:          60,
		QuotaCooldownSeconds:           5,
		MaxQuotaRetryCycles:            2,
		QuotaBanMinutes:                30,
		OverloadBanMinutes:             30,
		OverloadResponseTimeoutSeconds: 15,

		Logging: LoggingConfig{
			Level: "info",
		},
	}
}

// Load reads each YAML file in paths, in order, and merges it over
// Default(). A missing file is not an error — it's simply skipped, so the
// caller can glob a config directory without checking which of the expected
// files actually exist.
//
// yaml.Unmarshal only overwrites fields present in a given document, so
// starting from a fully-populated default struct and unmarshaling each file
// on top gives a cheap partial merge without a separate deep-merge step.
// Later paths win over earlier ones field-by-field; a slice-typed field
// (gemini_api_key_envs, languages) is
// replaced wholesale by whichever file last sets it, so such a field should
// live in exactly one file.
func Load(paths ...string) (Config, error) {
	cfg := Default()

	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return cfg, fmt.Errorf("config: read %s: %w", path, err)
		}

		if err := yaml.Unmarshal(data, &cfg); err != nil {
			return cfg, fmt.Errorf("config: parse %s: %w", path, err)
		}
	}

	if err := cfg.validate(); err != nil {
		return cfg, err
	}

	return cfg, nil
}

func (c Config) validate() error {
	if c.HTTPAddr == "" {
		return fmt.Errorf("config: http_addr must not be empty")
	}
	if c.AuthTokenEnv == "" {
		return fmt.Errorf("config: auth_token_env must not be empty")
	}
	if c.WyomingAddr == "" {
		return fmt.Errorf("config: wyoming_addr must not be empty")
	}
	if len(c.GeminiAPIKeyEnvs) == 0 {
		return fmt.Errorf("config: gemini_api_key_envs must list at least one env var name")
	}
	if c.GeminiModel == "" {
		return fmt.Errorf("config: gemini_model must not be empty")
	}
	if c.GeminiVoice == "" {
		return fmt.Errorf("config: gemini_voice must not be empty")
	}
	if c.ChunkMaxChars <= 0 {
		return fmt.Errorf("config: chunk_max_chars must be positive")
	}
	if c.AudioCacheDir == "" {
		return fmt.Errorf("config: audio_cache_dir must not be empty")
	}
	switch c.Logging.Level {
	case "debug", "info", "warn", "error":
	default:
		return fmt.Errorf("config: logging.level must be one of debug|info|warn|error, got %q", c.Logging.Level)
	}
	return nil
}
