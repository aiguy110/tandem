# Spoken agent responses

Tandem can render any completed assistant message as audio. Click **Listen** beneath the
message; the browser asks the daemon to produce audio and then displays native playback
controls. In-progress messages are disabled until their text is stable.

The per-thread speaker button in the chat header is an opt-in background version of the
same action: when an agent finishes, the daemon renders and retains the same clip. Opening
the chat then loads the usual player without another provider request. It never starts
playback automatically. The preference, render state, and generated clips survive browser
refreshes and daemon restarts, sync across clients, and every generated clip is retained
until the agent is deleted. The header reports rendering, ready, or error status.

## Pipeline and privacy

The daemon reconstructs the selected message from its durable event log, then performs two
requests:

1. `POST` the message and speech-preparation instructions to Tandem's shared
   OpenAI-compatible Chat Completions endpoint. This removes Markdown and turns code/tables
   into language that sounds natural. The same configuration is intended for conversation
   summaries and automatic title generation.
2. `POST` the cleaned text to an OpenAI-compatible `v1/audio/speech` endpoint.

Both requests originate from the Tandem daemon. Provider API keys remain in the owner-only
`$TANDEM_HOME/config.yml` (or daemon environment), are redacted by `tandem debug config`, and
are never sent to the browser. Generated audio is retained in Tandem's local SQLite database
until its agent is deleted; the browser receives it only from Tandem's authenticated API.
The original message is sent to the shared language-model provider and its rewritten form to
the speech provider, so choose local endpoints when the transcript must stay on the host.

## Setup

Run `tandem setup`, configure the “Shared language model,” then opt into “Spoken agent
responses.” The wizard explains each field and preserves existing settings when rerun. For
OpenAI, use:

- Shared language-model endpoint: `https://api.openai.com/v1/chat/completions`
- Speech endpoint: `https://api.openai.com/v1/audio/speech`
- Speech model: `tts-1` (or another model supported by your account/provider)
- Voice: `alloy` (or another voice supported by the selected model)
- Format: `mp3`

See the [OpenAI speech guide](https://developers.openai.com/api/docs/guides/text-to-speech)
and [Audio API reference](https://platform.openai.com/docs/api-reference/audio/createSpeech).

Open-weight/local choices include:

- [Ollama OpenAI compatibility](https://docs.ollama.com/api/openai-compatibility) for the
  shared `v1/chat/completions` endpoint.
- [Kokoro-FastAPI](https://github.com/remsky/Kokoro-FastAPI), which exposes an
  OpenAI-compatible `v1/audio/speech` endpoint and has CPU/GPU images.
- [LocalAI](https://localai.io/), which can provide both OpenAI-compatible language and TTS
  models from one local service.

Endpoint fields are complete URLs, not base URLs. Tandem sends the standard Chat Completions
body (`model`, `messages`) and Speech body (`model`, `input`, `voice`, `response_format`).

## Environment overrides

Environment settings take precedence over the setup file:

| Variable | Meaning |
|---|---|
| `TANDEM_VOICE_ENABLED` | `1`, `true`, or `on` enables the feature; other explicit values disable it |
| `TANDEM_LANGUAGE_MODEL_ENDPOINT` | Complete shared Chat Completions URL |
| `TANDEM_LANGUAGE_MODEL_API_KEY` | Optional shared language-model bearer token |
| `TANDEM_LANGUAGE_MODEL_MODEL` | Shared language-model identifier |
| `TANDEM_VOICE_INSTRUCTIONS` | System instruction used to prepare spoken text |
| `TANDEM_VOICE_TTS_ENDPOINT` | Complete Speech URL |
| `TANDEM_VOICE_TTS_API_KEY` | Optional speech bearer token |
| `TANDEM_VOICE_TTS_MODEL` | TTS model identifier |
| `TANDEM_VOICE_TTS_VOICE` | Provider-supported voice identifier |
| `TANDEM_VOICE_TTS_FORMAT` | Audio response format, such as `mp3`, `wav`, `opus`, `flac`, or `aac` |

Provider failures are shown inline beneath the message. Tandem limits language-model responses to
2 MiB and audio responses to 32 MiB, and applies a two-minute request timeout.

The original `TANDEM_VOICE_CLEANUP_*` names and `settings.voice.cleanup*` keys remain
supported as deprecated compatibility aliases. Rerunning `tandem setup` migrates file-based
settings to `settings.languageModel`; new configurations should use the shared names above.
