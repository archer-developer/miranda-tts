// Command miranda-tts is Miranda's text-to-speech service, backed by Gemini's
// speech models. It runs two listeners:
//
//   - a Wyoming TCP server that Home Assistant's built-in "wyoming"
//     integration connects to, making this service the Assist pipeline's
//     streaming TTS engine;
//   - an HTTP server with /healthz, a bearer-gated MCP endpoint (speak/stop,
//     used by Miranda to voice replies that did not come through the
//     pipeline) and the unauthenticated /audio route media players fetch
//     rendered clips from.
//
// Bootstrap: envfile.Load(.env) -> list config/*.yaml (configFilePaths) ->
// config.Load(paths...) -> build the real logger -> resolve secrets -> build
// the Gemini client, audio cache and speaker -> start both listeners ->
// serve until SIGINT/SIGTERM, then shut down gracefully.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/archer-developer/miranda-tts/internal/audiocache"
	"github.com/archer-developer/miranda-tts/internal/config"
	"github.com/archer-developer/miranda-tts/internal/envfile"
	"github.com/archer-developer/miranda-tts/internal/gemini"
	"github.com/archer-developer/miranda-tts/internal/ha"
	"github.com/archer-developer/miranda-tts/internal/httpserver"
	"github.com/archer-developer/miranda-tts/internal/mcpserver"
	"github.com/archer-developer/miranda-tts/internal/speaker"
	"github.com/archer-developer/miranda-tts/internal/wyoming"
)

const (
	dotEnvPath       = ".env"
	defaultConfigDir = "config"
	configDirEnv     = "MIRANDA_TTS_CONFIG_DIR"
	shutdownTimeout  = 10 * time.Second
	debugLogDir      = "logs"
	debugLogFile     = "debug.log"
)

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))

	if err := envfile.Load(dotEnvPath); err != nil {
		logger.Warn("failed to load .env, continuing with process environment", "error", err)
	}

	cfgDir := defaultConfigDir
	if v := os.Getenv(configDirEnv); v != "" {
		cfgDir = v
	}

	configPaths, err := configFilePaths(cfgDir, logger)
	if err != nil {
		logger.Error("fatal", "error", err)
		os.Exit(1)
	}

	cfg, err := config.Load(configPaths...)
	if err != nil {
		logger.Error("fatal", "error", err)
		os.Exit(1)
	}

	realLogger, closeLogger, err := buildLogger(cfg.Logging)
	if err != nil {
		logger.Error("fatal", "error", err)
		os.Exit(1)
	}
	defer closeLogger()
	logger = realLogger

	if err := run(cfg, logger); err != nil {
		logger.Error("fatal", "error", err)
		os.Exit(1)
	}
}

// configFilePaths lists the real config override files the service should
// load: every regular file directly under cfgDir whose name ends in
// ".yaml", sorted by name (os.ReadDir guarantees this) so config.Load's
// merge order — later file wins per field — is deterministic. This
// deliberately excludes config.yaml.dist, since it doesn't end in plain
// ".yaml"; see internal/config's doc comment for the full merge semantics.
//
// os.ReadDir is used instead of filepath.Glob deliberately: Glob treats
// cfgDir as pattern syntax (so a directory literally named e.g.
// "configs[2024]" silently matches nothing instead of being read as a
// literal path) and reports "no matches, no error" for a directory that
// doesn't exist *or* one that exists but can't be read (e.g. a permissions
// mistake) — indistinguishable from "no config files, use defaults" even
// though the latter is a real misconfiguration that should be loud.
// os.ReadDir never interprets cfgDir as a pattern and returns a real error
// for the unreadable case. A missing cfgDir is not treated as an error here
// either — it's the same as an empty one, i.e. "no overrides" — but is
// logged so a typo'd MIRANDA_TTS_CONFIG_DIR is at least visible in the log
// rather than silently producing an all-defaults service.
func configFilePaths(cfgDir string, logger *slog.Logger) ([]string, error) {
	entries, err := os.ReadDir(cfgDir)
	if err != nil {
		if os.IsNotExist(err) {
			logger.Warn("config directory not found, using built-in defaults", "dir", cfgDir)
			return nil, nil
		}
		return nil, fmt.Errorf("main: read config dir %s: %w", cfgDir, err)
	}

	var paths []string
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".yaml") {
			continue
		}
		paths = append(paths, filepath.Join(cfgDir, entry.Name()))
	}

	logger.Info("config files discovered", "dir", cfgDir, "paths", paths)
	return paths, nil
}

func run(cfg config.Config, logger *slog.Logger) error {
	token := os.Getenv(cfg.AuthTokenEnv)
	if token == "" {
		return fmt.Errorf("main: environment variable %s (named by auth_token_env) is not set — refusing to start with no auth token", cfg.AuthTokenEnv)
	}

	var apiKeys []string
	for _, env := range cfg.GeminiAPIKeyEnvs {
		if v := os.Getenv(env); v != "" {
			apiKeys = append(apiKeys, v)
		}
	}
	if len(apiKeys) == 0 {
		return fmt.Errorf("main: none of the environment variables %v (named by gemini_api_key_envs) hold a key", cfg.GeminiAPIKeyEnvs)
	}

	synth, err := gemini.New(gemini.Config{
		APIKeys:         apiKeys,
		Model:           cfg.GeminiModel,
		Voice:           cfg.GeminiVoice,
		RequestTimeout:  time.Duration(cfg.RequestTimeoutSeconds) * time.Second,
		QuotaCooldown:   time.Duration(cfg.QuotaCooldownSeconds) * time.Second,
		MaxRetryCycles:  cfg.MaxQuotaRetryCycles,
		QuotaBan:        time.Duration(cfg.QuotaBanMinutes) * time.Minute,
		OverloadBan:     time.Duration(cfg.OverloadBanMinutes) * time.Minute,
		OverloadTimeout: time.Duration(cfg.OverloadResponseTimeoutSeconds) * time.Second,
	}, logger)
	if err != nil {
		return fmt.Errorf("main: %w", err)
	}

	cache, err := audiocache.New(cfg.AudioCacheDir)
	if err != nil {
		return fmt.Errorf("main: %w", err)
	}

	// Home Assistant credentials are only needed by the MCP speak/stop tools;
	// the Wyoming listener works without them, so their absence is a warning.
	var haCaller speaker.ServiceCaller = unconfiguredHA{}
	if baseURL, haToken := os.Getenv("HA_BASE_URL"), os.Getenv("HA_TOKEN"); baseURL != "" && haToken != "" {
		haCaller = ha.New(baseURL, haToken)
	} else {
		logger.Warn("HA_BASE_URL / HA_TOKEN not set — the MCP speak and stop tools will fail; the Wyoming TTS server is unaffected")
	}

	sp := speaker.New(speaker.Config{PublicBaseURL: cfg.PublicBaseURL, ChunkMaxChars: cfg.ChunkMaxChars}, synth, haCaller, cache, logger)

	wyomingServer, err := wyoming.NewServer(cfg.WyomingAddr, logger, synth, wyoming.Service{
		Model:     cfg.GeminiModel,
		Voice:     cfg.GeminiVoice,
		Languages: cfg.Languages,
	})
	if err != nil {
		return fmt.Errorf("main: %w", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	server := mcpserver.New(logger, sp)
	mcpHandler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, nil)
	handler := httpserver.New(mcpHandler, token, httpserver.AudioHandler(cache))
	httpServer := &http.Server{Addr: cfg.HTTPAddr, Handler: handler}

	logger.Info("service ready", "http_addr", cfg.HTTPAddr, "wyoming_addr", wyomingServer.Addr().String(), "model", cfg.GeminiModel, "voice", cfg.GeminiVoice, "keys", len(apiKeys))

	return serveUntilInterrupted(ctx, httpServer, wyomingServer, logger)
}

// unconfiguredHA stands in for the Home Assistant client when its credentials
// are missing, so speak/stop fail with an actionable message instead of a
// nil dereference.
type unconfiguredHA struct{}

func (unconfiguredHA) CallService(context.Context, string, string, map[string]any) error {
	return errors.New("main: HA_BASE_URL / HA_TOKEN are not set, cannot reach Home Assistant")
}

// serveUntilInterrupted runs the HTTP and Wyoming listeners until ctx is
// cancelled or either fails; a failure of one stops the other.
func serveUntilInterrupted(ctx context.Context, httpServer *http.Server, wyomingServer *wyoming.Server, logger *slog.Logger) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	errCh := make(chan error, 2)
	go func() {
		logger.Info("listening", "addr", httpServer.Addr)
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- fmt.Errorf("http: %w", err)
			return
		}
		errCh <- nil
	}()
	go func() {
		logger.Info("listening (wyoming)", "addr", wyomingServer.Addr().String())
		errCh <- wyomingServer.Serve(ctx)
	}()

	var firstErr error
	received := 0
	select {
	case firstErr = <-errCh:
		received++
	case <-ctx.Done():
	}

	logger.Info("shutting down")
	cancel()
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer shutdownCancel()
	if err := httpServer.Shutdown(shutdownCtx); err != nil && firstErr == nil {
		firstErr = err
	}
	// Each listener goroutine reports exactly once; wait for the rest.
	for ; received < 2; received++ {
		select {
		case err := <-errCh:
			if err != nil && firstErr == nil {
				firstErr = err
			}
		case <-shutdownCtx.Done():
			return firstErr
		}
	}
	return firstErr
}

// buildLogger returns a logger that writes info-and-above to stdout (what
// systemd/journalctl sees) and, at debug level, routes debug-and-below
// records to logs/debug.log instead — keeping the journal readable while
// still capturing verbose per-request detail on disk when needed.
func buildLogger(cfg config.LoggingConfig) (*slog.Logger, func(), error) {
	noop := func() {}

	var level slog.Level
	if err := level.UnmarshalText([]byte(cfg.Level)); err != nil {
		return nil, noop, fmt.Errorf("main: invalid logging.level %q: %w", cfg.Level, err)
	}

	stdoutLevel := level
	if stdoutLevel < slog.LevelInfo {
		stdoutLevel = slog.LevelInfo
	}
	stdoutHandler := slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: stdoutLevel})

	if level > slog.LevelDebug {
		return slog.New(stdoutHandler), noop, nil
	}

	if err := os.MkdirAll(debugLogDir, 0o755); err != nil {
		return nil, noop, fmt.Errorf("main: create debug log dir: %w", err)
	}
	debugPath := filepath.Join(debugLogDir, debugLogFile)
	debugWriter, err := os.OpenFile(debugPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, noop, fmt.Errorf("main: open debug log %s: %w", debugPath, err)
	}
	debugHandler := slog.NewTextHandler(debugWriter, &slog.HandlerOptions{Level: slog.LevelDebug})

	handler := &levelSplitHandler{stdout: stdoutHandler, debugFile: debugHandler}
	return slog.New(handler), func() { _ = debugWriter.Close() }, nil
}

type levelSplitHandler struct {
	stdout    slog.Handler
	debugFile slog.Handler
}

func (h *levelSplitHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.stdout.Enabled(ctx, level) || h.debugFile.Enabled(ctx, level)
}

func (h *levelSplitHandler) Handle(ctx context.Context, r slog.Record) error {
	if r.Level < slog.LevelInfo {
		return h.debugFile.Handle(ctx, r)
	}
	return h.stdout.Handle(ctx, r)
}

func (h *levelSplitHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &levelSplitHandler{
		stdout:    h.stdout.WithAttrs(attrs),
		debugFile: h.debugFile.WithAttrs(attrs),
	}
}

func (h *levelSplitHandler) WithGroup(name string) slog.Handler {
	return &levelSplitHandler{
		stdout:    h.stdout.WithGroup(name),
		debugFile: h.debugFile.WithGroup(name),
	}
}
