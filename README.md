# miranda-tts

Text-to-speech for the Miranda assistant, backed by Gemini speech models.

It does two jobs:

1. **Home Assistant TTS engine (streaming).** A Wyoming protocol server that
   Home Assistant's built-in *Wyoming Protocol* integration connects to. The
   resulting `tts.*` entity is selected as the Assist pipeline's
   text-to-speech engine; audio is forwarded to HA while Gemini is still
   producing it.
2. **Voicing for non-HA requests.** An MCP server (`speak` / `stop`) Miranda
   calls to read out replies that did not come through the Assist pipeline
   (Telegram, web UI, reminders) on a Home Assistant satellite or media
   player.

Design rationale and rollout plan: [`docs/adr/gemini-tts-ha-streaming-integration.md`](docs/adr/gemini-tts-ha-streaming-integration.md).
Architecture notes for contributors: [`CLAUDE.md`](CLAUDE.md).

## Running

```bash
cp .env.example .env
# fill in MIRANDA_TTS_MCP_TOKEN (openssl rand -hex 32), GEMINI_API_KEY_1[, _2, ...],
# and HA_BASE_URL / HA_TOKEN if you use the MCP speak/stop tools

cp config/config.yaml.dist config/config.yaml
# set public_base_url if media_player playback is needed; override model/voice

make run
```

| Listener | Default | Purpose |
| --- | --- | --- |
| HTTP | `:8794` | `GET /healthz`, bearer-gated `/mcp`, unauthenticated `GET /audio/{sha256}.wav` |
| Wyoming TCP | `:10400` | Home Assistant's Wyoming integration |

## Configuration

The three things you asked to be configurable:

- **API keys** — `gemini_api_key_envs`: a list of environment variable
  *names*, each holding one key. Keys rotate on quota/overload errors.
- **Voice** — `gemini_voice` (default `Kore`); the MCP `speak` tool can
  override it per call for media players.
- **Model** — `gemini_model` (default `gemini-3.8-flash-lite-tts`).

Everything else is documented in [`config/config.yaml.dist`](config/config.yaml.dist).
Secrets are never in YAML, only the names of the env vars that hold them.

## Home Assistant setup

1. Settings → Devices & services → Add integration → **Wyoming Protocol**,
   host `127.0.0.1` (or the service's host), port `10400`.
2. Settings → Voice assistants → *Miranda* pipeline → Text-to-speech → pick
   the new entity (replacing `tts.null_tts`).
3. Once voice replies work, remove the `null_tts` integration and its
   `custom_components/null_tts` directory.

## MCP tools

| Tool | Input | Notes |
| --- | --- | --- |
| `speak` | `text`, `entity_id`, optional `voice` | `assist_satellite.*` → `assist_satellite.announce`; `media_player.*` → synthesize, serve from `/audio`, `media_player.play_media`. Blocks until playback ends. |
| `stop` | `entity_id` | Interrupts `speak` on a `media_player.*`; satellite announcements cannot be interrupted. |

## Development

```bash
make check   # fmt + lint + test — run before every commit
make tools   # installs golangci-lint and goimports
```

## Deploying

```bash
MIRANDA_DEPLOY_HOST=archer@miranda ./scripts/deploy.sh
```

Cross-compiles for `linux/amd64`, installs a `systemd --user` unit and
smoke-tests `/healthz`. `config/*.yaml` and `.env` are never uploaded —
create them on the server by hand on first deploy.

## Project layout

```
cmd/miranda-tts/       entrypoint: config, wiring, both listeners, graceful shutdown
internal/config/       Default() + YAML config + validation
internal/envfile/      .env loader (real env always wins)
internal/gemini/       Gemini speech client: buffered + streaming, key rotation
internal/wyoming/      Wyoming TTS server (HA's pipeline engine)
internal/speaker/      speak/stop onto HA satellites and media players
internal/mcpserver/    speak / stop MCP tools
internal/httpserver/   /healthz, bearer-gated /mcp, unauthenticated /audio
internal/audiocache/   content-addressed clip cache + WAV wrapper
internal/chunk/        sentence-aware text chunking
internal/ha/           HA REST client (service calls only)
scripts/deploy.sh      cross-compile + SSH deploy + systemd --user restart
```
