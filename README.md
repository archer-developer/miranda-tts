# miranda-tts

Gemini text-to-speech for Home Assistant and the Miranda assistant.

- **Home Assistant voice replies.** Appears in Home Assistant as a text-to-speech engine for your
  Assist pipeline. Audio starts playing while Gemini is still generating it.
- **Reading answers aloud elsewhere.** Lets Miranda speak on a Home Assistant speaker
  (a Voice PE satellite or a media player such as a Yandex Station) when a request did not come
  from Home Assistant, for example from Telegram or a reminder.

## What you need

- A Gemini API key (more than one is fine; they are used in turn when one runs out of quota).
- Home Assistant with the built-in **Wyoming Protocol** integration.
- A Linux host with `systemd --user` to run the service, and Go 1.25+ to build it.
- For Yandex Station playback: the [`yandex_station`](https://github.com/AlexxIT/YandexStation)
  integration **version 3.22.0 or newer** (see Troubleshooting).

## Install

```bash
git clone git@github.com:archer-developer/miranda-tts.git
cd miranda-tts

cp .env.example .env              # then fill it in (see below)
cp config/config.yaml.dist config/config.yaml
make run                          # build and start locally
```

To install on a server:

```bash
MIRANDA_DEPLOY_HOST=user@host ./scripts/deploy.sh
```

The script builds the binary, installs a `systemd --user` service called `miranda-tts` and checks
that it is healthy. It never uploads `.env` or `config/*.yaml`; create those on the server once.

## Configure

**`.env`** holds the secrets:

| Variable | What it is |
| --- | --- |
| `MIRANDA_TTS_MCP_TOKEN` | Password Miranda uses to call this service. Make one with `openssl rand -hex 32`. |
| `GEMINI_API_KEY_1`, ... | Your Gemini API keys. Any names work, as long as they are listed in `config.yaml`. |
| `HA_BASE_URL`, `HA_TOKEN` | Home Assistant address and a long-lived access token. Only needed for speaking on speakers. |

**`config/config.yaml`** holds everything else. The commonly changed settings:

```yaml
gemini_api_key_envs: ["GEMINI_API_KEY_1", "GEMINI_API_KEY_2"]   # names of the variables above
gemini_model: "gemini-3.8-flash-lite-tts"
gemini_voice: "Achernar"
public_base_url: "http://192.168.1.50:8794"   # how your speakers reach this service
```

Every option is described in [`config/config.yaml.dist`](config/config.yaml.dist).

### Choosing a model and voice

- **Models:** `gemini-3.8-flash-lite-tts` (fastest and cheapest, the default),
  `gemini-3.8-flash-tts` (higher quality), `gemini-3.1-flash-tts-preview` (older). Check Google's
  pricing page for current prices.
- **Voices:** Gemini's built-in voices, for example `Zephyr`, `Kore`, `Puck`, `Sulafat`,
  `Achernar`, `Vindemiatrix`, `Aoede`. Listen to a few and pick one.
- The service also speaks Russian, Belarusian and many other languages; the language is detected
  from the text.

## Connect to Home Assistant

1. **Settings -> Devices & services -> Add integration -> Wyoming Protocol.** Enter the host
   running this service and port `10400`.
2. **Settings -> Voice assistants** -> your pipeline -> **Text-to-speech** -> pick the new
   engine and a voice. The voice must match `gemini_voice` or be one this service advertises.

Test it with the **Try** button in the pipeline settings.

If you change `gemini_voice` later, reload the Wyoming integration entry in Home Assistant so it
sees the new voice.

## Use with Miranda

Add this service to Miranda's `mcp.servers`:

```yaml
- name: miranda-tts
  url: "http://127.0.0.1:8794/mcp"
  token_env: "MIRANDA_TTS_MCP_TOKEN"
  enabled: true
```

Miranda then reads answers aloud on request ("say it out loud") through the `speak_reply` tool and
stops with `stop_speech`. Voice replies to questions asked through Home Assistant do not use this;
they go through the pipeline above.

## Logs

```bash
journalctl --user -u miranda-tts -f
```

Every synthesis is one line with the model, voice, time and audio length:

```
gemini: synthesized mode=stream model=gemini-3.8-flash-lite-tts voice=Achernar chars=36 synth_ms=1081 audio_ms=3200 first_audio_ms=649
```

On the speaker path each piece also reports `cache=hit` or `cache=miss`: a hit means a clip made
earlier was reused and no request was sent to Gemini. Errors are logged at `ERROR` level. Set
`logging.level: debug` for more detail in `logs/debug.log`.

## Troubleshooting

- **A Yandex Station plays random music instead of the voice.** Newer station firmware needs
  `yandex_station` 3.22.0 or newer. Update it and restart Home Assistant.
- **Home Assistant says it cannot connect.** Check the service is running
  (`systemctl --user status miranda-tts`) and that port `10400` is reachable from Home Assistant.
- **No sound from a media player.** `public_base_url` must be an address the player can open in
  a browser. Open `http://<that address>/healthz`; it should answer `ok`.
- **Errors about quota.** Add another key to `.env` and to `gemini_api_key_envs`.
- **Speaking on speakers fails but Home Assistant voice works.** Check `HA_BASE_URL` and
  `HA_TOKEN` in `.env`.

## For developers

```bash
make check   # format, lint and tests; run before every commit
```

Architecture and conventions: [`CLAUDE.md`](CLAUDE.md). Design decisions:
[`docs/adr/`](docs/adr/gemini-tts-ha-streaming-integration.md).
