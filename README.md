# voicechat

[![Build](https://github.com/hangxie/voicechat/actions/workflows/pr.yml/badge.svg)](https://github.com/hangxie/voicechat/actions/workflows/pr.yml)

Spoken conversation with an LLM in a cloned voice. A browser page records what you say; this service sends it to a speech-to-text backend for a transcript, streams a reply from an OpenAI-compatible LLM, and has a text-to-speech backend speak it one sentence at a time, so the first sentence plays while the rest is still being generated.

```text
browser ──WSS /ws──► reverse proxy ──► voicechat (127.0.0.1:5006) ──► STT_URL  /audio/transcriptions
   ▲                                          │                  ──► LLM_URL  /chat/completions (streamed)
   └──── JSON events + binary audio ◄─────────┘                  ──► TTS_URL  /api/tts, one chunk at a time
```

The service is one static binary with the page built in. It holds no models and needs no GPU; every backend is a URL, so each stage can run wherever there is hardware for it. The browser keeps the conversation and sends it with every turn, so the service keeps no state between turns and a restart or reconnect loses nothing.

Browsers allow microphone access only on HTTPS pages and on `localhost`, so outside of local development serve it behind an HTTPS reverse proxy that passes WebSocket upgrades through.

## Backends

- **STT**: OpenAI's `POST /audio/transcriptions` with `response_format=verbose_json`, whose reply carries `text` and `language` (a lowercase English name such as `chinese`). Any Whisper server with that API works.
- **LLM**: OpenAI's streamed `POST /chat/completions`, such as llama.cpp's `llama-server`, vLLM, or LiteLLM. Every request sets `chat_template_kwargs.enable_thinking` to false, since thinking would be a long silent pause before the first word.
- **TTS**: `GET /voices` (a JSON list of voice names), `GET /health` (with `languages`, the languages it accepts), and `POST /api/tts` taking `{"text", "voice", "language", "stream": true}`. A streaming backend answers `application/x-ndjson` with `{"type": "audio", "pcm": <base64 16-bit mono PCM>, "sample_rate": n}` events and a final `{"type": "done"}` or `{"type": "error", "error": "..."}`; any other content type is taken as one whole WAV.

[hangxie/ray-images](https://github.com/hangxie/ray-images) serves both speech APIs on CUDA under Ray Serve: Qwen3-TTS voice cloning ([TTS](https://github.com/hangxie/ray-images#tts)) and Whisper ([STT](https://github.com/hangxie/ray-images#stt)).

## How a turn works

The page uses push-to-talk by default: hold the button or Space while speaking. On release it sends the recording as 16 kHz mono WAV, along with the conversation so far and the selected voice. A typed message goes the same way without audio and skips STT.

The model gets up to 80 messages of the conversation; past that the oldest are dropped 40 at a time, so it always sees between 41 and 80. Dropping in blocks keeps the start of the prompt unchanged for 20 turns at a time, so an LLM server with prefix caching, such as llama.cpp, processes only the newest messages. A window that slides by one message every turn changes the start of the prompt every time and makes every turn reprocess all of it.

The LLM text is cut into chunks at sentence punctuation as it streams, and each chunk goes to TTS as soon as it is complete. To start playback sooner, the first chunk may also end at a comma once it is 12 characters long. ASCII stops only end a chunk when followed by a space, so `3.14` stays whole. Markdown, links, and emoji are stripped before synthesis because the system prompt does not always stop the model from writing them.

Each chunk is requested with `"stream": true`, and every piece of PCM is forwarded to the page as soon as it is generated; the page schedules each piece right after the previous one, so playback starts well before the whole chunk is synthesized. Pieces travel as raw PCM rather than small WAVs because the browser resamples each decoded WAV on its own, which can click at the joins. Chunks are synthesized one at a time and in order. A busy TTS (409, 429, or 503) is retried for up to 20 seconds before the turn reports a TTS error; the reply text still arrives.

Each turn names its reply language in a note appended to the latest user message rather than in the system prompt, because a change there would invalidate the whole cached prefix and chat templates usually allow a system message only first. The language is Chinese, Japanese, or Korean when the message contains that script, otherwise the STT-detected language, and for typed Latin text just "the language of this message". Without the note, once the history holds a couple of replies in another language (for example after STT mistook speech for English), a small model copies that pattern. The same language goes to TTS when TTS lists it in `/health`, otherwise `auto`.

**Hands-free** keeps the microphone open and finds utterances by level: speech starts when the input stays above three times an adaptive noise floor for 150 ms and ends after 800 ms of quiet, with 300 ms of audio before the trigger kept so the first syllable is not clipped. Utterances with under 300 ms of speech are dropped, and any utterance is cut at 30 seconds. While the bot is speaking the bar is six times the floor for 350 ms, because echo cancellation leaves some of the bot's voice in the microphone; speech that clears it interrupts the bot just as pressing talk does. A level detector was chosen over a speech model such as Silero VAD to keep the page a single file with no downloaded runtime. Hands-free also holds a screen wake lock, since mobile browsers stop the microphone when the screen locks.

Pressing talk while the bot is speaking interrupts it: the page stops playback and cancels the turn, and the service aborts that turn's LLM and TTS requests, so a TTS that watches for disconnects frees its GPU at once. The part of the reply received before the interruption stays in the history.

The page reports the STT time and detected language for each utterance, and the time to the first LLM token, the first audio, and the end of generation for each reply, all measured from when the service received the turn.

## Protocol

The page keeps one WebSocket open to `/ws` and reconnects on its own after a restart or network drop. It sends JSON text frames:

- `{"type": "turn", "id": 7, "text": "...", "audio": false, "history": [{"role": "user", "content": "..."}], "voice": "alice"}` starts a turn. With `"audio": true` the next frame is binary and holds the recording as WAV, up to 10 MB. A new turn cancels the one in progress.
- `{"type": "cancel"}` stops the turn in progress and aborts its LLM and TTS requests.

The server answers with JSON text frames that each carry the `turn` id, so the page drops late events from a cancelled turn: `transcript`, `text` (with `delta`), `audio`, `error` (with `stage`: `request`, `stt`, `llm`, or `tts`), and `done` (with `reply` and `timings`). Every turn that is not cancelled ends with `done`, even after an error. An `audio` event is followed by one binary frame: raw 16-bit mono PCM at `sample_rate` when `format` is `pcm`, or a whole WAV when `format` is `wav`. It also carries `index` (chunk), `part` (piece within the chunk), and on the first piece the chunk's `text`.

The server pings every 25 seconds, which keeps an idle connection under a proxy's idle timeout and finds dead peers; when a connection drops mid-turn, the turn is cancelled as if interrupted.

Other endpoints: `GET /` (the page), `GET /health` (the configured backends, without contacting them), and `GET /api/voices` (the TTS voices and the default).

## Configuration

Settings are environment variables.

| Setting | Default | Meaning |
|---|---|---|
| `STT_URL` | `http://localhost:5005/v1` | OpenAI-compatible base URL for `/audio/transcriptions` |
| `LLM_URL` | `http://localhost:8080/v1` | OpenAI-compatible base URL for `/chat/completions` |
| `LLM_MODEL` | `qwen3.5-9b-q8-uncensored` | Model ID sent in each LLM request |
| `LLM_MAX_TOKENS` | `512` | Cap on each reply |
| `TTS_URL` | `http://localhost:5004` | Base URL of a TTS with the API above |
| `VOICE` | Unset | Voice for turns that name none; the page defaults to it, else to the first TTS voice, and remembers its own choice |
| `SYSTEM_PROMPT` | Short, spoken, same-language replies | Replaces the built-in system prompt |
| `STT_TOKEN`, `LLM_TOKEN`, `TTS_TOKEN` | Unset | Sent as `Authorization: Bearer <token>` to that backend |
| `VOICE_CHAT_HOST` | `127.0.0.1` | Listen address |
| `VOICE_CHAT_PORT` | `5006` | Listen port |
| `TRUSTED_NETWORKS` | `127.0.0.0/8,::1/128` | Comma-separated CIDR prefixes clients may connect from; others get 403 |

The service has no login of its own. Behind a reverse proxy on the same host every request comes from loopback, so who can reach it is decided by the proxy; widen `TRUSTED_NETWORKS` only to serve a LAN directly. Recordings are held only in memory for the turn, and the log records connections and one line of timings per turn but never what was said.

## Develop

```bash
make test     # unit tests with -race; coverage report in build/test/coverage.{txt,html}
make format   # gofumpt and goimports
make lint     # golangci-lint, with gocognit
make run      # build and serve http://localhost:5006/ against STT_URL, LLM_URL, and TTS_URL
make all      # everything but run, as CI does
```

`make run` takes the settings above from the command line or the environment, for example `make run LLM_URL=http://gpu-box:8080/v1 VOICE=alice`. The tests run the backend clients, the WebSocket session, and whole turns against fake STT, LLM, and TTS servers. [AGENTS.md](AGENTS.md) holds the coding, testing, and commit standards.

## Deploy

Build a Linux binary and run it under a service manager with the settings in its environment:

```bash
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -buildvcs=false -ldflags='-s -w' -o voicechat .
```

The build is reproducible: without a VCS stamp the binary is byte-identical until the code changes, so configuration management can copy it on every run without reporting a change. Put an HTTPS reverse proxy in front that passes the WebSocket upgrade through and allows idle connections of at least a minute. For nginx:

```nginx
location / {
    proxy_pass http://127.0.0.1:5006;
    proxy_http_version 1.1;
    proxy_set_header Upgrade $http_upgrade;
    proxy_set_header Connection $connection_upgrade;  # from a map on $http_upgrade
    proxy_set_header Host $host;
    proxy_set_header X-Real-IP $remote_addr;
    proxy_read_timeout 300s;
}
```

The WebSocket accepts only pages from its own origin, so the proxy must pass the `Host` header through unchanged.

## License

[BSD 3-Clause](LICENSE).
