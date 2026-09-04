# ⚡ Verdent 2API

OpenAI & Anthropic-compatible proxy for Verdent.ai — single Go binary.

[![Version](https://img.shields.io/badge/version-1.0.1-blue)](https://github.com/D3-vin/Verdent2Api/releases)
[![Go](https://img.shields.io/badge/Go-1.26+-00ADD8?logo=go)](https://go.dev/)
[![License](https://img.shields.io/badge/license-AS_IS-green)](#license)

[![Telegram](https://img.shields.io/badge/Telegram-@D3_vin-blue?logo=telegram)](https://t.me/D3_vin)
[![Author](https://img.shields.io/badge/Author-@D3vin_dev-blue?logo=telegram)](https://t.me/D3vin_dev)
[![GitHub](https://img.shields.io/badge/GitHub-Repository-black?logo=github)](https://github.com/D3-vin/Verdent2Api)

[Features](#features) • [Quick Start](#quick-start) • [Usage](#usage) • [Configuration](#configuration) • [Troubleshooting](#troubleshooting) • [Contact](#contact)

[English](#) | [Русский](README_RU.md)

---

## Features

- 🚀 **Single binary** — HTTP server, Verdent client, and dashboard in one Go binary; zero external dependencies
- 🔓 **Multi-protocol** — OpenAI (`/v1/chat/completions`), Anthropic (`/v1/messages`), and Responses API (`/v1/responses`)
- 🧩 **Tool calling** — full function calling support for IDE integration; works out of the box with text-based contract
- 🔐 **Account pooling** — multi-JWT rotation with automatic failover and quota tracking
- 📊 **Embedded dashboard** — live HTML UI for account management, model selection, and OAuth login
- 🌐 **Cross-platform** — Windows, macOS, Linux
- 📦 **Standalone** — pure Go, no CGO dependencies

> 💡 **Works with IDE out of the box!** Perfect for AI-assisted development with Verdent models.

---

## Quick Start

### 1. Download

Download the latest release for your platform:
- [Windows (64-bit)](https://github.com/D3-vin/Verdent2Api/releases)
- [Linux (64-bit)](https://github.com/D3-vin/Verdent2Api/releases)
- [macOS Intel](https://github.com/D3-vin/Verdent2Api/releases)
- [macOS Apple Silicon](https://github.com/D3-vin/Verdent2Api/releases)

Or build from source:

```bash
git clone https://github.com/D3-vin/Verdent2Api.git
cd Verdent2Api
go build -trimpath -ldflags="-s -w" -o verdent ./cmd/server
```

### 2. Configure

```bash
cp .env.example .env
# Edit .env — set VERDENT_TOKEN (JWT from Verdent desktop app)
# Get token from DevTools → Network → authorization header
```

### 3. Run

```bash
./verdent
```

Dashboard: http://localhost:5084

### 4. Test

```bash
curl -X POST http://localhost:5084/v1/chat/completions \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer change-me" \
  -d '{"model":"deepseek-v4-flash-free","messages":[{"role":"user","content":"Hello!"}],"stream":false}'
```

---

## Usage

### Streaming

```bash
curl -N -X POST http://localhost:5084/v1/chat/completions \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer change-me" \
  -d '{"model":"deepseek-v4-flash-free","stream":true,"messages":[{"role":"user","content":"Say hi"}]}'
```

### Connect from an IDE

| Setting | Value |
|---|---|
| Base URL | `http://localhost:5084/v1` |
| API Key | `change-me` (the `AUTH_TOKEN` from .env) |
| Model | `deepseek-v4-flash-free`, `glm-5.3-flash-free`, etc. |

> 💡 **IDE Integration**: This API works perfectly with AI coding assistants! Just add it as a custom OpenAI endpoint.

### Models

Free models available without account limits:
- `deepseek-v4-flash-free` — DeepSeek V4 Flash (fast, efficient)
- `glm-5.3-flash-free` — GLM-5.3 Flash (balanced)
- And more via `/v1/models`

Premium models require Verdent JWT:
- `deepseek-v4` — DeepSeek V4 (full version)
- `glm-4-plus` — GLM-4 Plus
- `claude-3-5-sonnet-20241022` — Claude 3.5 Sonnet
- Full list at http://localhost:5084/ or `GET /v1/models`

### API Endpoints

| Method | Path | Auth | Description |
|---|---|---|---|
| `POST` | `/v1/chat/completions` | yes | OpenAI-compatible chat (stream/non-stream, tools) |
| `POST` | `/v1/messages` | yes | Anthropic-compatible chat (Claude Desktop) |
| `POST` | `/v1/responses` | yes | Responses API (extended format) |
| `GET` | `/v1/models` | yes | List available models |
| `POST` | `/prompt` | yes | Legacy: single text prompt |
| `GET` | `/status` | no | Server status |

**Session headers:**

| Header | Purpose |
|---|---|
| `X-Session-Id` | Conversation ID (default `default`) |
| `X-Fresh-Session` | `true` — new conversation, empty history |

**Extra request fields:** `deepThink`, `search`/`webSearch`, `reasoning`.

### Tool Calling

Verdent has no native function calling. The proxy implements a text-based contract adapter:

- `tools` → rewritten as structured prompt → model emits JSON → parsed into `tool_calls`
- Multiple parsing strategies: JSON code blocks, agent markers, direct JSON
- Streaming support: live tool call interception
- Compatible with OpenAI and Anthropic formats

### Account Management

Web dashboard at http://localhost:5084/:
- Add/remove JWT accounts
- View quota usage per account
- Login via OAuth (PKCE flow)
- Select default model
- View cooldown status

---

## Configuration

Settings in `.env` (auto-loaded) or environment variables:

| Variable | Default | Description |
|---|---|---|
| `VERDENT_TOKEN` | *(empty)* | Verdent JWT (seed for first run) |
| `VERDENT_TOKENS_FILE` | `verdent_tokens.txt` | Multi-account seed file (one JWT per line) |
| `PORT` | `5084` | Server port |
| `HOST` | `0.0.0.0` | Bind address |
| `AUTH_TOKEN` | `change-me` | Bearer token for API access |
| `LOG_LEVEL` | `info` | `debug`, `info`, `warn`, `error` |
| `TIMEOUT` | `300000` | Request timeout (milliseconds) |
| `VERDENT_PROXY` | *(empty)* | HTTP proxy for Verdent traffic |
| `PERSIST_HISTORY` | `false` | Persist conversation history |
| `CLAUDE_DESKTOP` | `false` | Enable Anthropic-compatible mode |

**Getting `VERDENT_TOKEN`:**
1. Open Verdent desktop app
2. DevTools (F12) → Network tab
3. Look for requests with `authorization` header
4. Copy the JWT (starts with `eyJ...`)
5. Add to `.env`: `VERDENT_TOKEN=eyJ...`

Optional CLI flags:

```bash
./verdent  # All config from .env
```

---

## Troubleshooting

| Symptom | Cause | Fix |
|---|---|---|
| `No accounts available` | Missing or invalid JWT | Add valid `VERDENT_TOKEN` in `.env` or via dashboard |
| `401 Unauthorized` | Wrong `AUTH_TOKEN` | Check Bearer token matches `AUTH_TOKEN` in `.env` |
| Empty response | Network/timeout error | Set `LOG_LEVEL=debug`, check logs |
| `Model not available` | Model not in catalog | Check `/v1/models` for available models |
| Quota exhausted | All accounts depleted | Wait for reset or add more accounts |

---

## Project Structure

```
verdent/
├── cmd/server/          # Entry point (calls app.Main)
├── internal/app/        # Core implementation
│   ├── verdent.go       # Verdent client (AES-GCM, SSE streaming)
│   ├── verdentmeta.go   # Model catalog + quota tracking
│   ├── verdentoauth.go  # OAuth PKCE login
│   ├── verdentpool.go   # JWT account pool + config.json
│   ├── models.go        # Model catalog + aliases + cooldowns
│   ├── main.go          # HTTP server, OpenAI routes
│   ├── anthropic.go     # Anthropic API compatibility
│   ├── responses.go     # Responses API
│   ├── tools.go         # Tool calling adapter
│   ├── toolstream.go    # Streaming tool call parser
│   └── dashboard.go     # Embedded dashboard + API
├── .env.example         # Configuration template
├── go.mod               # Go module (no external deps)
└── BUILD.md             # Build instructions
```

---

## Contact

- **GitHub**: https://github.com/D3-vin/Verdent2Api
- **Telegram**: [@D3_vin](https://t.me/D3_vin)
- **Author**: [@D3vin_dev](https://t.me/D3vin_dev)

---

## License

Provided as-is for educational and interoperability purposes. Use responsibly and in accordance with Verdent's terms of service.
