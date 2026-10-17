# miranda-tts — project notes for Claude Code

Text-to-speech service for the Miranda assistant, backed by Gemini's speech
models. A sibling of [miranda](../miranda) (the "brain": LLM orchestration,
MCP *client*) and [miranda-stt](../miranda-stt) (speech-to-text — the mirror
image of this service, and the precedent for its Home Assistant side).
Scaffolded from [miranda-service-skeleton](../miranda-service-skeleton); the
Conventions below are inherited from it and binding.

The design rationale, rejected alternatives and rollout order live in
`docs/adr/gemini-tts-ha-streaming-integration.md`.

## Conventions

- **Write explanatory comments** — doc-comments on exported symbols,
  comments on non-obvious logic and on *why* a decision was made. This
  family of projects intentionally diverges from a terse/no-comments
  default: these are small home-infra services maintained intermittently,
  and future-you (or Claude, months later) benefits more from carried-forward
  reasoning than from terse code.
- **No Docker, no CGO.** Single static Go binary (`CGO_ENABLED=0`). If a
  service needs a database, use `modernc.org/sqlite` (pure-Go) — never
  `mattn/go-sqlite3` or anything else requiring cgo. The binary runs
  host-native under `systemd --user`, not in a container.
- **Config**: every field has a Go-level default in `internal/config.Default()`.
  `config/config.yaml.dist` is the single, checked-into-git source of truth
  documenting every available field and its default — it is never loaded by
  the running service. A real deployment provides any number of
  `config/*.yaml` files (a single `config.yaml`, or split by topic), each
  gitignored (only `config.yaml.dist` is tracked). `main.go`'s
  `configFilePaths` lists them (`os.ReadDir`, not `filepath.Glob` — a
  directory that's missing, unreadable, or named with glob metacharacters
  must be handled explicitly and loudly, not silently swallowed as "no
  matches") and passes the resulting paths, in that order, to
  `config.Load(paths...)`, which starts from `Default()` and unmarshals each
  file on top of it in turn — a deliberate partial-merge trick via
  `yaml.Unmarshal` into an already-populated struct, applied once per file,
  so unset keys keep their default and a later file overrides only the
  fields it actually sets. A missing file is not an error. Call a
  `validate()` method at the end of `Load()` to reject values `Default()`
  would never produce but a hand-edited file could (empty required strings,
  a max below a min, an unrecognized enum value, ...).
- **Secrets are never in YAML.** Config stores only the *name* of an
  environment variable (`auth_token_env`, `api_key_env`, ...); the actual
  value is read from the process environment at startup via `os.Getenv`.
  The service refuses to start (fatal, logged, `os.Exit(1)`) if a required
  one is empty. This lets config be committed to git safely and audited
  without ever containing a secret.
- **`.env` is dev-only convenience, real env always wins.** `internal/envfile.Load`
  sets a variable only if it isn't already set in the process environment —
  never overwrite. This means the exact same binary invocation works
  unchanged whether secrets come from a checked-out `.env` (local dev) or a
  real environment (`systemd`'s `EnvironmentFile=`, a process supervisor).
  A missing `.env` file is not an error, just a `Warn` log line.
- **Error wrapping**: `fmt.Errorf("<package/function>: <what>: %w", err)`
  everywhere — the package or function name as the message prefix makes
  wrapped error chains readable without needing to look at a stack trace.
- **No DI framework, no HTTP router/middleware library.** Plain `net/http`
  with `http.ServeMux`'s method-and-path patterns (`"GET /healthz"`).
  Dependencies are wired manually, top-down, in one `run()` function in
  `main.go` — construct stores/clients, pass them (or narrow interfaces over
  them) into the next layer's constructor. No container, no reflection-based
  wiring.
- **Module path convention**: `github.com/archer-developer/<service-name>`.
  Binary and `cmd/` directory are named after the service
  (`cmd/miranda-tts`, not `cmd/server` or `cmd/main`).
- **No Docker, no CI.** Every sibling service is deployed by hand-rolled
  `scripts/deploy.sh` over SSH to a single host as a `systemd --user`
  service — no `Dockerfile`, no `.github/workflows`. `make check` run
  locally before committing is the actual quality gate. Don't add CI
  config or a Dockerfile to a new service without discussing it first —
  it's a deliberate scope decision made across the whole family, not an
  oversight.

## Architecture

Two independent surfaces share one Gemini client:

```
Home Assistant (Assist pipeline)               Miranda orchestrator (MCP client)
        | Wyoming TCP :10400                          | HTTP :8794, bearer token
        v                                             v
  internal/wyoming (Server/Session)         internal/httpserver (/healthz, /mcp, /audio)
        |                                             |
        |                                    internal/mcpserver (speak, stop)
        |                                             |
        |                                    internal/speaker ---> internal/ha ---> HA REST
        |                                             |
        +-------------------+  internal/gemini  +-----+     (cache: internal/audiocache)
```

- **Wyoming surface** — what makes this the Assist pipeline's TTS engine.
  Home Assistant's *built-in* `wyoming` integration (no custom Python
  component) connects to `wyoming_addr`, reads `describe` → `info`, and
  creates a `tts.*` entity. For every utterance HA's streaming path sends
  `synthesize-start`, `synthesize-chunk`*, `synthesize` (duplicate of the
  whole text — ignored once a stream started), `synthesize-stop`, and
  **stops reading only on `synthesize-stopped`**, not on `audio-stop`.
  `internal/wyoming/session.go` implements that handshake; a bare
  `synthesize` (HA's non-streaming path) ends on `audio-stop`. PCM is
  forwarded as `audio-chunk` events the moment Gemini produces it. This
  surface never calls Home Assistant's REST API — HA plays the audio on
  whichever device started the turn. Same wire code as `miranda-stt`
  (`protocol.go` is copied from it; only `types.go`/`session.go` differ).
- **MCP surface** — `speak` / `stop`, used by Miranda to voice replies that
  did *not* arrive through the pipeline (Telegram, web UI, reminders).
  `internal/speaker` picks the mechanism from the entity's domain:
  `assist_satellite.*` → `assist_satellite.announce` with the text (the
  satellite synthesizes through its own pipeline = this service over
  Wyoming); `media_player.*` → synthesize, cache, serve at
  `GET /audio/{sha256}.wav`, `media_player.play_media` with that URL
  (`media_content_type: music`, **never** `dialog` — it reopens the mic),
  then wait out the clip's exact duration (the station's `alice_state`
  never changes for URL playback). Miranda resolves friendly names to
  entity_ids; this service only receives concrete entity_ids.
- **`/audio/{filename}` is deliberately unauthenticated** — media players
  cannot send bearer tokens. The filename must match
  `^[0-9a-f]{64}\.wav$` (checked before any filesystem access), and the
  name is a content hash, so it cannot be guessed. This is the one
  deviation from the skeleton's two-route mux.
- **`internal/gemini`** — `SynthesizeOnce` (full clip) and
  `SynthesizeStream` (callback per PCM piece via `:streamGenerateContent
  ?alt=sse`). If the model rejects the streaming endpoint (HTTP 400/404) or
  returns no audio there, it latches to buffered synthesis re-chunked into
  4 KiB frames, so the Wyoming session sees the same contract either way.
  Keys rotate via `miranda-llm/keyrotation` with a ban list shared across
  calls; rotation only happens before the first audio byte of a stream.
  Audio is always 24 kHz / 16-bit / mono (`internal/audiocache` constants).
- **`internal/audiocache`** — content-addressed (`sha256(model|voice|text)`),
  permanent, no eviction (a hit skips a quota-limited API call).

Configuration: see `config/config.yaml.dist`. `HA_BASE_URL` / `HA_TOKEN` are
read from the environment directly (like miranda's own HA client); without
them the service still runs and serves Wyoming, and only `speak`/`stop`
fail.

### Logging

- **One `Info` line per synthesis request**, written by `internal/gemini`
  (so both the Wyoming and the MCP path get it): `gemini: synthesized` with
  `mode` (`buffered` / `stream` / `stream-fallback`), `model`, `voice`
  (resolved — the default voice is logged by name), `chars`, `synth_ms`
  (total synthesis time), `audio_ms` (length of the result) and, for
  streams, `first_audio_ms`. Never the text itself.
- **Clip cache (media_player path only):** every chunk logs
  `speaker: chunk rendered` with `cache=hit` or `cache=miss` plus the same
  `model`/`voice`/`synth_ms`/`audio_ms` fields. A hit means no Gemini call
  (and no quota spent), so it has no `gemini: synthesized` line; a miss is
  followed by one. The Wyoming path never uses the cache.
- **Failures are always `Error`**: `gemini: synthesis failed` (same fields
  plus `error`), plus the caller-side lines (`synthesis failed, sending error
  event to client`, `speak failed`, `stop failed`, `session error`, `fatal`).
  A request the caller abandoned (client disconnect, `stop`) is only `Warn`
  (`gemini: synthesis cancelled`).
- Per-attempt detail (which key index, key rotation) is `Warn` on quota /
  overload and `Debug` on success; `logging.level: debug` routes it to
  `logs/debug.log`.
- Secrets (API keys, tokens) are never logged.

### Home Assistant setup (once, after deploy)

1. Settings → Devices & services → Add integration → **Wyoming Protocol**,
   host/port = this service's `wyoming_addr` (alongside the existing
   `miranda-stt` entry on :10300).
2. Settings → Voice assistants → the Miranda pipeline → Text-to-speech →
   the new Wyoming entity (replacing `tts.null_tts`).
3. Only after that works: remove the `null_tts` integration and its
   `custom_components/null_tts` directory.

### Known open points

- Gemini's streaming TTS request shape was not verified against a live key
  when this was written; the fallback above keeps the service correct if it
  is unsupported. Check the logs for "streaming not usable" after the first
  real request.
- WAV served to the Yandex Station over URL playback is unverified on real
  hardware (miranda's own `gemini_tts` path carries the same caveat).
- `stop` cannot interrupt `assist_satellite.announce`.

## Security requirements

- Bearer token auth on every non-health endpoint, compared with
  `crypto/subtle.ConstantTimeCompare` — never `==` on secrets.
- An empty/unconfigured token must **fail closed** (500, "server
  misconfigured"), not accept arbitrary requests.
- `/healthz` and `/audio/{filename}` are the only unauthenticated
  endpoints. `/healthz` must not leak anything beyond liveness (no version
  strings, stack traces, or config values); `/audio` serves only
  hash-named clips and validates the name before touching the filesystem.
- Never log secret values (tokens, API keys) — logging the *name* of the
  env var they come from is fine and often useful for debugging startup
  failures.

## Testing

```bash
make test    # go test ./... -race
```

- Use `testify` (`assert` for non-fatal checks, `require` for fatal
  preconditions like setup errors) — no other assertion or mocking
  framework.
- Prefer one short, descriptively-named `Test*` function per scenario over
  a single sprawling table, unless the cases are truly a flat enumeration
  of input→output pairs (e.g. `validate()` rejecting each bad field) — then
  a table is clearer.
- Test helpers that build fixtures should call `t.Helper()` and register
  cleanup with `t.Cleanup(...)`, not bare `defer`, so cleanup still runs if
  the helper itself fails a `require`.
- Prefer small, narrow interfaces (like `miranda_diary`'s `Embedder`) over a
  mocking framework — a hand-written fake implementing a two-method
  interface is simpler than generated mocks and keeps the interface honest
  (if it's annoying to fake, it's probably too wide).
- In-memory backends for tests where available (`sqlite.Open(":memory:")`)
  — fast, no filesystem setup, no cleanup risk.
- No CI is configured for this family of services — `make check` (which
  includes `test`) run locally before every commit is the actual gate.
  Don't skip it because "it's just a small change."
- Add an integration test (a black-box test wiring together fakes and real
  storage, see `miranda`'s `test/integration`) once a service's tool
  handlers start composing multiple domain packages — a single MCP
  microservice with 1-3 simple tools usually doesn't need one; unit tests
  against in-memory backends are sufficient at that scale.

## Code quality and static analysis

```bash
make fmt     # gofmt + goimports
make lint    # golangci-lint run ./...
make check   # fmt + lint + test — run before every commit
```

- `.golangci.yml` uses the v2 schema with a deliberately minimal linter set
  (`errcheck`, `govet`, `ineffassign`, `staticcheck`, `unused`) — resist the
  urge to pile on `gofumpt`/`revive`/`gocritic` by default. If you add a
  linter or an exclusion, add a comment explaining why, the same way the
  existing `errcheck` exclusion for `Close`/`Write` explains itself.
- `make tools` installs `golangci-lint` and `goimports` if missing.
- Run `make check` (not just `go build`) before considering any change
  done — a change that builds but fails `go vet`/`staticcheck` or isn't
  `gofmt`-clean is not finished.

## Deploying

```bash
MIRANDA_DEPLOY_HOST=user@host ./scripts/deploy.sh
```

- Cross-compiles `GOOS=linux GOARCH=amd64 CGO_ENABLED=0`, ships the binary
  and a generated `systemd --user` unit over `scp`/`ssh`, restarts the
  service, then polls `/healthz` a few times as a smoke test before
  declaring success.
- `config/*.yaml` (everything except the tracked `config.yaml.dist`
  template) and `.env` are **never touched by deploy** — they live on the
  target host and hold real, environment-specific values. Create them by
  hand on first deploy (copy `config.yaml.dist` to `config/config.yaml` and
  edit); the service's own startup check (empty required env var → fatal) is
  the safety net if `.env` is forgotten.
- The deploy host is passed via `MIRANDA_DEPLOY_HOST`, not hardcoded, so a
  copy-pasted `deploy.sh` can't silently deploy to the wrong machine.

## What's deliberately not here

- **Docker / containers**, **CI/CD**, **an HTTP framework**, **a DI
  container**, **a mocking framework** — same reasons as every service in
  this family (see the skeleton).
- **A custom Home Assistant Python component** — HA's built-in `wyoming`
  integration already provides a streaming TTS entity for any Wyoming TTS
  server, exactly as it does for `miranda-stt`.
- **Multi-user data / encryption at rest** — this service stores no
  per-user data; cached clips are keyed by content only.
- **Text-streaming input** (`supports_synthesize_streaming`) — Miranda's
  conversation entity returns a complete reply, so text is accumulated until
  `synthesize-stop`; only the *audio* streams.
- **MP3 output** — clips are WAV; add an encoder only if the Yandex Station
  turns out not to play WAV over URL.
