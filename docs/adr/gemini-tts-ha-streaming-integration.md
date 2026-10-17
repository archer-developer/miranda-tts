# ADR: Gemini-based streaming TTS service with Home Assistant integration

## Status

Accepted. The `miranda-tts` service described below is implemented (see "Implementation notes"). The changes in the main `miranda` repo (section 3) and the Home Assistant rollout (section 4) are still to do.

## Context

Miranda currently has no real Home Assistant TTS integration. Its Assist pipeline's
`tts_engine` is `tts.null_tts`, a stub (`custom_components/null_tts/tts.py` on the HA
host) that discards whatever text it receives and returns a short silent clip. It exists
purely so HA's Assist pipeline validation doesn't reject an unconfigured TTS engine.

Real voicing happens through a separate, pipeline-bypassing channel: Miranda's
`internal/tts` package (in the main `miranda` repo) calls `media_player.play_media`
directly against the Yandex Station ("Alice" smart speaker)'s HA entity, over HA's REST
API, independent of the Assist pipeline concept entirely.

Because `ha-integration/miranda/conversation.py` *also* returns the reply text to HA as
the conversation agent's spoken response (`response.async_set_speech(...)`), HA's own
pipeline *also* tries to run it through whatever `tts_engine` is configured. Today that's
harmless because `null_tts` is silent — but it means there are already two voicing paths
racing for the same turn, and replacing `null_tts` with a real engine without fixing this
would make the assistant speak every `ha_assist` reply twice. This double-voicing is
already documented as a known wart in Miranda's own `README.md` (the "Home Assistant
integration" section) and `CLAUDE.md`.

Separately, Miranda has non-HA input sources (Telegram, a web UI, CLI, scheduled
reminders) that have no Assist pipeline to speak through at all. Today, voicing for
those sources only happens if the model explicitly calls a `speak_reply` tool, which
also goes through the same direct-to-Yandex-Station channel.

### Why now

Modern Home Assistant (≥ 2025.5) supports true **streaming** TTS synthesis at the
`TextToSpeechEntity` level (`async_stream_tts_audio`, `TTSAudioRequest`/
`TTSAudioResponse`) — audio can start playing before synthesis of the full utterance
finishes. We want a real, Gemini-backed TTS engine with this property, installed as a
standard Assist pipeline step, replacing both `null_tts` and the ad hoc Yandex-Station
channel with one coherent mechanism.

### Key precedent that shapes this design

Miranda's sibling speech-to-text service, `miranda-stt` (`/Users/alexandersamusevich/
localhost/home-assistant/miranda-stt`), already proves the right shape for the
HA-facing side of a service like this. It runs a **hand-rolled Wyoming protocol TCP
server** (`internal/wyoming/`, ~664 lines, no external protocol dependency — its
`go.mod` only pulls in `coder/websocket` and `yaml.v3`), and Home Assistant's own
**built-in, stock** `wyoming` core integration connects to it, auto-creating the
`stt.gemini_live_stt_speaker_recognition` entity. This is confirmed live: HA's
`.storage/core.config_entries` has a `wyoming` domain entry
`{"host": "127.0.0.1", "port": 10300}` pointing at `miranda-stt`.

HA core's own `homeassistant/components/wyoming/tts.py` already implements real
`async_stream_tts_audio` streaming against *any* Wyoming TTS server — it opens a
`Synthesize` request and forwards `AudioChunk` events to HA's streaming pipeline as they
arrive. **This means a Gemini-backed streaming TTS engine needs no custom Python HA
component, `manifest.json`, or `config_flow.py` at all.** We only need to speak the
Wyoming protocol from Go, and add one HA integration entry (host:port) via the UI,
exactly mirroring how `miranda-stt` is already wired in today.

## Decision

Build `miranda-tts`, a new Go microservice scaffolded from
`miranda-service-skeleton` (`/Users/alexandersamusevich/localhost/home-assistant/
miranda-service-skeleton`), with **two independent server surfaces** sharing one Gemini
TTS client:

1. **A Wyoming protocol TCP server** — what HA's stock `wyoming` integration talks to.
   This is what makes the Assist pipeline's TTS step real and streaming for
   HA-originated (`ha_assist`) turns.
2. **An MCP tool server** (the skeleton's native pattern: Streamable HTTP + bearer
   token) — what Miranda's own orchestrator calls, for voicing replies on *non*-HA
   sources (Telegram, web UI, CLI, scheduled reminders), replacing today's in-process
   `internal/tts.Dispatcher` in the main `miranda` repo.

```
Home Assistant (Assist pipeline, tts_engine)        Miranda orchestrator (MCP client)
        |  TCP :10400 (Wyoming protocol)                     |  HTTP, bearer token
        v                                                     v
  internal/wyoming (Server/Session)                 internal/httpserver (/healthz, /mcp)
        |                                                     |
        +----------------------+    both call    +-------------+
                                v
                          internal/gemini (TTS client: streaming + buffered)
                                v
                          internal/ha (CallService only)
                                v
                 HA REST API: assist_satellite.announce / media_player.play_media
```

The Wyoming surface never calls out to HA's REST API — it only streams PCM back over
the same TCP connection; HA itself plays it on whichever satellite/device started the
turn. The MCP surface does call HA's REST API, because there's no pipeline to lean on
for non-HA-originated replies.

Alongside this, Miranda's own `internal/tts` package (the direct-to-Yandex-Station
channel) is deleted, and the double-voicing bug in `internal/agent_loop/agent_loop.go`
is fixed: for `ha_assist` turns, voicing now happens exactly once, through HA's real
pipeline TTS entity.

### Non-HA-source voicing fallback (confirmed)

Rather than dropping voicing for non-HA sources or leaving the current mechanism
duplicated across two services, the existing Alice-voicing mechanism is **migrated
into `miranda-tts`**, behind its MCP `speak`/`stop` tools:

- For the two HA Voice PE satellites (`assist_satellite.*` entities) — call
  `assist_satellite.announce` with the text as `message`. The satellite's own
  configured pipeline TTS (this same service's Wyoming entity) does the actual
  synthesis; `miranda-tts` does no synthesis work in this branch at all.
- For the Yandex Station (`media_player.*`, no `assist_satellite` support) —
  synthesize via Gemini, serve the clip over a small unauthenticated HTTP route, and
  call `media_player.play_media` with that URL (`media_content_type: "music"` — never
  `"dialog"`, which would reopen the station's mic). This is the same mechanism
  Miranda's existing (currently unverified-on-real-hardware) `gemini_tts` fallback
  provider already uses, just relocated into the new service.

## Implementation plan

### 1. New `miranda-tts` service

Scaffold from `miranda-service-skeleton`'s own bootstrap checklist: module path
`github.com/archer-developer/miranda-tts`, `cmd/miranda-tts`, rename
`SERVICE_MCP_TOKEN` → `MIRANDA_TTS_MCP_TOKEN` and `SERVICE_CONFIG_DIR` →
`MIRANDA_TTS_CONFIG_DIR` everywhere, `HTTPAddr :8794` (next free port after Miranda's
own `:8787`), a new Wyoming TCP listener on `:10400` (next free slot after
`miranda-stt`'s `:10300`), update `serverName`/`serverVersion` in
`internal/mcpserver/server.go`, write this service's own `CLAUDE.md` (shared
Conventions section copied verbatim, new Architecture section), rename
`scripts/deploy.sh`'s target to `miranda-tts`.

#### Package layout

```
cmd/miranda-tts/main.go        config, two listeners (HTTP+Wyoming), graceful shutdown
internal/config/               Config struct, Default(), Load(), validate()  (skeleton pattern)
internal/envfile/              copied as-is from skeleton
internal/httpserver/           /healthz, /mcp (bearer), GET /audio/{filename} (unauthenticated)
internal/mcpserver/            "speak" + "stop" MCP tools
internal/ha/ha.go              CallService only — no entity resolution, no AliceState
internal/gemini/               SynthesizeOnce (buffered) + SynthesizeStream (chunked PCM)
internal/wyoming/protocol.go   ported verbatim from miranda-stt (Reader/Writer framing)
internal/wyoming/types.go      TTS-domain types: InfoData{TTS}, TTSProgram, TTSVoice, Synthesize*, AudioStart/Chunk/Stop
internal/wyoming/server.go     ported from miranda-stt, config fields swapped for TTS session's needs
internal/wyoming/session.go    describe -> synthesize -> audio-start -> audio-chunk* -> audio-stop loop
internal/audiocache/           content-addressed disk cache + WAV header helpers, ported from miranda/internal/tts/{cache.go,audio.go}
```

`internal/wyoming/protocol.go` (the `Reader`/`Writer` framing — one JSON header line
with `data_length`/`payload_length`, then that many data bytes, then that many payload
bytes, read via one shared `bufio.Reader` so payload bytes are never consumed by header
look-ahead) is protocol-generic and can be copied unchanged from
`miranda-stt/internal/wyoming/protocol.go`. Only `types.go` (ASR→TTS event shapes) and
`session.go` (the actual describe/synthesize state machine) are new content.

#### Config (`internal/config`)

```go
type Config struct {
    HTTPAddr     string   `yaml:"http_addr"`       // ":8794"
    AuthTokenEnv string   `yaml:"auth_token_env"`   // "MIRANDA_TTS_MCP_TOKEN"
    WyomingAddr  string   `yaml:"wyoming_addr"`     // ":10400"

    GeminiAPIKeyEnvs []string `yaml:"gemini_api_key_envs"` // ["GEMINI_API_KEY_1", "GEMINI_API_KEY_2", "GEMINI_API_KEY_3"]
    GeminiModel      string   `yaml:"gemini_model"`
    GeminiVoice      string   `yaml:"gemini_voice"`
    SampleRateHz     int      `yaml:"sample_rate_hz"`  // 24000

    ChunkMaxChars int    `yaml:"chunk_max_chars"` // sentence-chunking for the media_player speak path only
    PublicBaseURL string `yaml:"public_base_url"` // how a media_player fetches GET /audio/{key}.wav
    AudioCacheDir string `yaml:"audio_cache_dir"`

    QuotaCooldownSeconds, MaxQuotaRetryCycles, QuotaBanMinutes,
    OverloadBanMinutes, OverloadResponseTimeoutSeconds int

    Logging LoggingConfig
}
```

`HA_BASE_URL`/`HA_TOKEN` are read directly via `os.Getenv` in `main.go` with hardcoded
literal names (no config field) — this mirrors Miranda's own `buildTTSDispatcher`
exactly. `validate()` requires `AuthTokenEnv`, `WyomingAddr`, `HTTPAddr`, `GeminiModel`,
`GeminiVoice` non-empty, at least one `GeminiAPIKeyEnvs` entry, `SampleRateHz > 0`. Fail
fast (`os.Exit(1)`) if no configured Gemini key env resolves to a non-empty value; only
**warn** (don't fail) if `HA_BASE_URL`/`HA_TOKEN` are unset, since the Wyoming surface
works without them — only the MCP `speak` tool's `media_player` branch needs HA
credentials.

Deployment config on `archer@miranda` mirrors the key-array shape Miranda's own
`config/tts.yaml` already proves works:
```yaml
gemini_api_key_envs: ["GEMINI_API_KEY_1", "GEMINI_API_KEY_2", "GEMINI_API_KEY_3"]
gemini_model: "gemini-3.8-flash-lite-tts"   # fast/cheap default; flash-tts is the studio-grade alternative
gemini_voice: "Kore"
public_base_url: "http://192.168.1.94:8794"
```

#### Wyoming TTS session (`internal/wyoming/session.go`)

Mirrors `miranda-stt/internal/wyoming/session.go`'s structure, but one-directional
(server→client only — no audio ever arrives *from* HA, unlike STT):

1. `{"type":"describe"}` → reply `{"type":"info","data":{"tts":[{"name":"gemini-tts","voices":[{"name":"<GeminiVoice>","languages":[...]}],"installed":true,...}]}}`. Start by advertising just the configured default voice; don't try to enumerate all ~30 Gemini voices up front.
2. `{"type":"synthesize","data":{"text":"...","voice":{"name":"..."}}}` → call `gemini.SynthesizeStream(ctx, text, voice)`.
3. First PCM bytes available → write `{"type":"audio-start","data":{"rate":24000,"width":2,"channels":1}}`.
4. Each PCM chunk from Gemini → write `{"type":"audio-chunk","data":{...same rate/width/channels...},"payload":<raw PCM>}` — forward as it arrives, never buffer the whole utterance (this is what makes it actually streaming).
5. Stream done → `{"type":"audio-stop"}`. Loop back to step 1/2 for the next utterance on the same connection.

#### MCP tools (`internal/mcpserver`)

```go
type SpeakInput struct {
    Text     string `json:"text"`
    EntityID string `json:"entity_id"` // e.g. "media_player.alice_mini_pro" or "assist_satellite.kitchen_voice_pe"
    Voice    string `json:"voice,omitempty"`
}
type SpeakOutput struct {
    Played     bool  `json:"played"`
    DurationMS int64 `json:"duration_ms,omitempty"`
}
type StopInput struct{ EntityID string `json:"entity_id"` }
type StopOutput struct{ Stopped bool `json:"stopped"` }
```

Domain is inferred from `EntityID`'s prefix (no separate `domain` field needed — HA
entity_ids are already self-describing):
- **`assist_satellite.*`** → one `assist_satellite.announce` call with `message: text`.
  No synthesis in `miranda-tts` for this branch — the satellite's own configured
  pipeline TTS (this same service's Wyoming entity) handles it.
- **`media_player.*`** (Yandex Station) → chunk text (port the sentence-boundary
  `Chunk`/`Accumulator` from `miranda/internal/tts/chunk.go`); for each chunk:
  `gemini.SynthesizeOnce` (cache-checked, content-addressed) →
  `ha.CallService("media_player", "play_media", {entity_id, media_content_id: audioURL, media_content_type: "music"})`
  → sleep the known PCM duration before the next chunk (mirrors the existing
  `stationPlayer`/`geminiProvider` pipelined pattern).
- `Stop`: for `media_player.*`, a per-entity in-memory cancellation flag checked between
  chunks, plus best-effort `media_player.media_stop`. For `assist_satellite.*`, return
  `Stopped: false` — satellite announcements aren't interruptible through this API
  (HA's wake-word barge-in is the satellite's own concern); don't pretend to succeed.

`internal/ha/ha.go` needs only `CallService` — no `ResolveMediaPlayer`; Miranda's
orchestrator keeps owning friendly-name→entity_id resolution and passes a resolved
`entity_id` into every call.

#### Unauthenticated audio route

`internal/httpserver`'s mux gets a third route alongside the skeleton's `/healthz` +
`/mcp`: `GET /audio/{filename}`, **unauthenticated** (media players can't send bearer
tokens — same rationale as the already-working `zvukogram` custom component's
`HomeAssistantView(requires_auth=False)` on this exact HA instance, and Miranda's own
current `/tts-audio/{filename}`). Validate the filename against `^[0-9a-f]{64}\.wav$`
before touching the filesystem. Document this as a deliberate, explained deviation from
the skeleton's fixed two-route mux in `miranda-tts`'s own `CLAUDE.md`.

`internal/audiocache`: content-addressed (`sha256(model+voice+text)`), permanent disk
cache, no TTL/eviction — ported from `miranda/internal/tts/cache.go`, same
already-reasoned-through tradeoff (a cache hit skips a quota-limited Gemini call; disk
growth on a home-infra box is accepted).

### 2. Gemini TTS client (`internal/gemini`)

Two entry points sharing one `Client` + key-rotation state:

- **`SynthesizeOnce`** (buffered, for the MCP `speak` tool's `media_player` branch) —
  build this **first**; it can reuse an API shape already proven live in this exact
  codebase. Miranda's existing `gemini_tts` fallback provider
  (`internal/tts/gemini.go`) already calls `POST /v1beta/models/{model}:generateContent`
  with `generationConfig.responseModalities: ["AUDIO"]` and
  `speechConfig.voiceConfig.prebuiltVoiceConfig.voiceName`, getting back base64 PCM in
  `candidates[0].content.parts[0].inlineData.data`. Port that request/response shape and
  its key-rotation wiring (depend directly on `github.com/archer-developer/miranda-llm/
  keyrotation`, already a standalone reusable module — don't reimplement the
  cooldown/ban-cycle loop) into `internal/gemini`, trimmed of playback/caching logic
  (that stays in the MCP handler). Wrap the raw PCM in a WAV header (port
  `internal/tts/audio.go`'s writer).

- **`SynthesizeStream`** (chunked PCM, for the Wyoming session) — **the one piece of
  this plan resting on live-but-unverified API research** (Google's TTS API surface
  has moved past this project's available training-data knowledge; the exact request
  shape needs re-verification at implementation time against
  `https://ai.google.dev/gemini-api/docs/speech-generation`). Verification order:
  1. Try `streamGenerateContent?alt=sse` with the exact same body `SynthesizeOnce`
     already proves works — does Gemini emit multiple progressive `inlineData` audio
     parts over SSE, or one buffered event at the end?
  2. If not, try `POST /v1beta/interactions` with `"stream": true` (headerless
     `audio/l16` PCM chunks as base64 in `step.delta` events), per live research
     findings at design time.
  3. **Fallback that must not block shipping**: if Gemini only ever synthesizes
     atomically regardless of transport, implement `SynthesizeStream` by calling
     `SynthesizeOnce` internally and re-chunking the resulting PCM into fixed-size
     frames (e.g. 4096 bytes) written in quick succession. This still produces a
     protocol-correct, working streaming Wyoming TTS entity — true synthesis-level
     streaming can follow later without changing `session.go`'s consumer code, since
     the signature (`func(ctx, text, voice) (<-chan []byte, <-chan error)`) stays the
     same either way.

One configurable `GeminiModel` serves both paths. `GeminiVoice` is the default; both
the MCP tool's `Voice` field and Wyoming's `Synthesize.voice.name` can override it per
call.

### 3. Changes in the `miranda` repo

**Delete**: `internal/tts/` in its entirety (`provider.go`, `yandex_text.go`,
`station.go`, `gemini.go`, `cache.go`, `httpserve.go`, `audio.go`, `chunk.go`,
`dispatcher.go`, `player.go`, `factory.go` + tests, ~2700 lines) — its job moves into
`miranda-tts`. In `cmd/miranda/main.go`: `buildTTSDispatcher` (~line 1095), the
`/tts-audio/{filename}` route (~line 558), `ttsChunkMaxChars`. In `internal/ha/ha.go`:
`AliceState` (keep `CallService`; keep `ResolveMediaPlayer` since Miranda still
resolves device names before calling `miranda-tts`'s MCP tool — re-verify nothing else
still depends on the deleted pieces with `grep -rn "internal/tts\|AliceState"` right
before the deletion commit).

**Config** (`internal/config/config.go`): delete `GeminiTTSConfig`,
`YandexStationConfig`. Trim `TTSConfig` to `{DefaultDevice string; SpeakReplyTool,
StopSpeechTool bool}`. `config/tts.yaml` shrinks to just `default_device`. Add
`miranda-tts` as a new entry in the existing generic `mcp.servers` config list (same
mechanism already used for the HA MCP server) — no new config section needed.

**`internal/agent_loop/tool_catalog.go`**: keep `speak_reply`/`stop_speech`'s tool
definitions (name/description/schema/gating) unchanged — the model-visible contract
doesn't change. Add `skip["miranda-tts"] = true` in the lazy-tool-loading `skip` map
(~line 384) so `miranda-tts`'s own `speak`/`stop` MCP tools never show up directly in
the model's tool list (the model only ever sees `speak_reply`/`stop_speech`, which
dispatch to them internally). Additionally gate both tools out entirely when
`source == users.SourceHAAssist` — now that HA's pipeline handles `ha_assist` voicing
automatically, exposing `speak_reply` there would reopen double-voicing one level down.

**`internal/agent_loop/tool_dispatch.go`**: `speak_reply` handler (~line 182) resolves
the target entity via the existing `ResolveMediaPlayer` logic, then calls
`o.tools.Call(ctx, "miranda-tts_speak", ...)` **asynchronously** (`go func() { ... }()`,
reporting errors to the hub) to preserve today's non-blocking "enqueue and return"
contract. `stop_speech` handler (~line 202) calls `miranda-tts_stop` synchronously.

**`internal/agent_loop/mcp_dispatch.go`**: remove the speaker-coordination block
(~line 31, `o.tts.WaitIdle` before an "alice" HA tool call) — it existed to wait out
Miranda's own in-process TTS queue, which no longer exists after `internal/tts` is
deleted.

**`internal/agent_loop/agent_loop.go` — the actual double-voicing fix**: in
`streamOneTurn` (~line 207-238), delete `speakLive := source == users.SourceHAAssist`
and both `if speakLive { o.speakChunks(...) }` branches — every source now just calls
`publishChunks`. `ha_assist` text is voiced exactly once, by HA's own pipeline (via
`conversation.py`'s existing, unchanged `response.async_set_speech(...)` call) against
the new real Wyoming-backed `tts.*` entity. Delete `speakChunks`/`speakText`'s
`o.tts`-based bodies; repoint `speakTextChecked` (still used by `deliverReminder`,
`schedule.go`) at the same new MCP-call path `tool_dispatch.go` uses for `speak_reply`.
Delete the `o.tts.Stop()` barge-in call in `Handle` (~line 702) — no local queue left to
stop. Delete the voiced-error-message call for `ha_assist` final-call failures
(`orchestrator.go` ~line 844) — the synchronous HTTP reply already carries the error
text back through `conversation.py` → pipeline TTS; a second explicit voice call would
reintroduce exactly the bug this plan fixes.

**`cmd/miranda/main.go`**: delete `buildTTSDispatcher` and its call site; stop passing
a `*tts.Dispatcher` into `Orchestrator`'s constructor (it now only needs `o.tools` —
already passed — plus `ttsCfg.DefaultDevice`/`SpeakReplyTool`/`StopSpeechTool` and the
already-alive `ha.Client` for `ResolveMediaPlayer`). `connectMCP` needs no change — it
already iterates `cfg.MCP.Servers` generically.

**Docs**: update `README.md`'s "Home Assistant integration" section (~554-635) —
remove the "spoken back through both..." line, add a short "Home Assistant as a
Wyoming TTS server" subsection (mirroring the existing "...as an MCP server" one)
describing the `wyoming` integration add + pipeline `tts_engine` swap. Update
`CLAUDE.md`'s "Response routing" section and sequence diagram to drop the
Yandex-Station-TTS-chunks annotation for `ha_assist`, and describe
`speak_reply`/`stop_speech` as dispatching to `miranda-tts`'s MCP tools.

### 4. Deployment / HA-config changes (on `archer@miranda`)

1. Deploy `miranda-tts` via `scripts/deploy.sh` to `systemd --user`, same as
   `miranda-stt`.
2. **Settings → Devices & Services → Add Integration → "Wyoming Protocol"**, pointing
   at `127.0.0.1:10400` — a second entry alongside the existing `gemini-live-stt` one
   (`127.0.0.1:10300`). HA auto-creates a `tts.<name>` entity. No custom Python
   component.
3. **Settings → Voice Assistants → (the "Miranda" pipeline) → Text-to-speech**: change
   `tts_engine` from `tts.null_tts` to the new Wyoming TTS entity
   (`.storage/assist_pipeline.pipelines` is the field that changes).
4. **Only after step 3 is confirmed working**: delete the `null_tts` config entry and
   `custom_components/null_tts/` — it exists purely so an unconfigured pipeline doesn't
   fail validation; pulling it before the replacement is live would break the pipeline.
5. `ha-integration/miranda/` (the existing `miranda` custom_component) needs **zero
   changes** — `conversation.py`'s `async_set_speech` call already hands text to
   whatever the pipeline's `tts_engine` is; it was always correct, the bug was Miranda
   *also* voicing it directly.

## Consequences

- One voicing path for `ha_assist` turns instead of two — the documented double-voicing
  bug is fixed as a side effect of adopting the standard HA TTS pipeline, not a
  separate patch.
- `miranda-tts` becomes a new always-on dependency for any voiced reply, HA-sourced or
  not. If it's down, `ha_assist` turns fall silent (same failure mode `null_tts` masked
  before) and `speak_reply`/`stop_speech` calls fail — acceptable for a home-infra
  service with the same deployment/monitoring posture as `miranda-stt`.
- Two new open-ended risks are taken on explicitly rather than silently: the exact
  Gemini streaming TTS request shape (unverified at design time) and the Yandex
  Station URL-playback path (already unverified in Miranda today, now relocated
  rather than resolved). Both have a stated fallback/mitigation below.
- `stop_speech` becomes weaker for `assist_satellite` targets (not interruptible via
  this API at all) — accepted as a reasonable tradeoff, not treated as a regression to
  silently paper over.

## Open risks

1. **Gemini streaming TTS API shape is unverified** — biggest technical risk; budget
   real verification time before writing `SynthesizeStream` (see §2's fallback plan).
2. **Yandex Station URL-playback path remains unverified on real hardware** — this
   plan relocates, not resolves, Miranda's own existing "not yet verified" caveat. Do a
   manual `speak` MCP call against the real `media_player.*` entity early.
3. **`stop_speech` loses most of its teeth**: only interrupts a multi-chunk
   `media_player` call in flight; can't interrupt `assist_satellite.announce` at all.
   Acceptable per the confirmed design; real barge-in on the fallback path is a
   follow-up if it matters in practice.
4. **`conversation.py` streaming partial text to HA** (so an `ha_assist` turn's TTS can
   start before the LLM finishes generating) is explicit non-goal/future work — it
   currently returns one complete reply synchronously, and restructuring that is a
   materially bigger, separate project from "make the TTS entity itself stream audio."

## Suggested build order

1. `miranda-tts` MCP surface first: `SynthesizeOnce` + `speak`/`stop` tools + `/audio`
   route, verified end-to-end against the real `media_player.*` (Yandex Station) and
   `assist_satellite.*` (Voice PE) entities. `null_tts` stays in place as a safety net
   the whole time — nothing on the HA pipeline side changes yet.
2. Wire the Miranda-side changes (§3) to call the new MCP tools for
   `speak_reply`/`stop_speech`, with the double-voicing fix in `agent_loop.go` landing
   in the same pass since it's gated on the same `source == ha_assist` logic being
   touched anyway.
3. Build the Wyoming surface (`SynthesizeStream` + `session.go`), verify with a manual
   Wyoming client before touching HA config.
4. Only then do the HA deployment steps in §4, in order, keeping `null_tts` until step
   3 of §4 is confirmed.

## Verification

- `make check` in `miranda-tts` (fmt+lint+test) before first commit, per skeleton
  convention.
- Manual Wyoming protocol smoke test (describe/synthesize round-trip) against the new
  `:10400` listener before adding the HA `wyoming` integration entry.
- After the HA `wyoming` integration + pipeline `tts_engine` swap: trigger a real
  `ha_assist` voice turn (via a Voice PE satellite or the pipeline's "Try" UI) and
  confirm audio is heard exactly once, streaming (not one long pause then playback).
- Manual MCP `speak` tool call against both a `media_player.*` and an
  `assist_satellite.*` entity_id, on real hardware, before considering the non-HA
  fallback path done.
- `go test ./... -race` in `miranda` after the `internal/tts` deletion and
  `agent_loop.go` changes, paying particular attention to
  `TestOrchestrator_StopSpeechToolClearsQueue` and the `speak_reply`-related tests in
  `orchestrator_test.go`/`schedule_tools_test.go`, which will need updating to assert
  against the new MCP-call path instead of the deleted `Dispatcher`.
## Implementation notes (deviations and findings)

Recorded after building the service, because they differ from or refine the
plan above:

- **Wyoming handshake.** Reading HA core's `wyoming/tts.py` showed that its
  streaming path always sends `synthesize-start`, `synthesize-chunk`*,
  `synthesize` (the full text again, "for backwards compatibility") and
  `synthesize-stop`, and **only stops reading audio on `synthesize-stopped`**
  — `audio-stop` alone would leave the stream hanging. The session therefore
  accumulates chunks, ignores the duplicate `synthesize` inside a stream, and
  ends with `audio-stop` + `synthesize-stopped`. A bare `synthesize` (HA's
  non-streaming path) is served one-shot and ends on `audio-stop`. Errors are
  sent as Wyoming `error` events, which HA raises as TTS errors.
- **`supports_synthesize_streaming` is advertised as false.** Miranda's
  conversation entity returns a complete reply, so there is nothing to gain
  from streaming *text* in; only audio streams out.
- **No `sample_rate_hz` setting.** Gemini always returns 24 kHz / 16-bit /
  mono PCM, so it is a constant (`internal/audiocache`); a config value could
  only produce mislabelled audio.
- **Gemini streaming** uses `:streamGenerateContent?alt=sse` with the request
  body already proven for `generateContent`. When the model answers that
  endpoint with HTTP 400/404 or no audio, the client latches to buffered
  synthesis re-chunked into 4 KiB frames (the fallback this ADR required).
  Neither path was exercised against the live API during implementation
  (no key used); the `/v1beta/interactions` shape from the design research
  was deliberately not implemented. Verify with a real request and look for
  the "streaming not usable" log line.
- **Added config:** `languages` (advertised to HA), `request_timeout_seconds`.
- **Added behaviour:** a newer `speak` on the same entity interrupts the
  running one; `stop` also cancels the running `speak`; `preannounce` is
  disabled for `assist_satellite.announce` (a spoken reply should not get a
  chime).
