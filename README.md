# WhatsApp MCP Server

[![CI](https://github.com/anboia/whatsapp-mcp/actions/workflows/ci.yml/badge.svg)](https://github.com/anboia/whatsapp-mcp/actions/workflows/ci.yml)
[![License: MIT](https://img.shields.io/badge/License-MIT-green.svg)](LICENSE)

A [Model Context Protocol](https://modelcontextprotocol.io) (MCP) server for your **personal WhatsApp account**. It lets an AI assistant such as Claude search and read your chats, look up contacts, send messages, voice notes and files, and download or export the media of a chat.

It connects through the WhatsApp Web multi-device API using [whatsmeow](https://github.com/tulir/whatsmeow). Your messages are stored on your machine in SQLite. They reach the LLM only when the assistant calls one of the tools, which you control.

![WhatsApp MCP](./example-use.png)

> This project started as a fork of [lharries/whatsapp-mcp](https://github.com/lharries/whatsapp-mcp) by Luke Harries. See [What's new in this fork](#whats-new-in-this-fork).

## Security and privacy

- **Prompt injection.** Like many MCP servers, this one is exposed to [the lethal trifecta](https://simonwillison.net/2025/Jun/16/the-lethal-trifecta/): it reads untrusted content (messages from anyone), has access to private data, and can send messages. A crafted message could try to get the assistant to leak data. Review what the assistant sends before you approve it.
- **Your session is a credential.** `whatsapp-bridge/store/whatsapp.db` holds the keys of your linked device. Anyone with that file can use your WhatsApp account. The whole `store/` folder (session, message history, downloaded media) is git-ignored. Never commit it, share it or back it up somewhere public.
- **Local only.** The bridge's REST API listens on `127.0.0.1` only and has no authentication. Don't expose that port to a network.
- **Unofficial API.** This is not affiliated with or endorsed by WhatsApp or Meta. Using an unofficial client may break their Terms of Service. Use it with your own account, at your own risk.

## Features

- Search and read your chats, with group messages showing the sender's name
- Search contacts, find a direct chat by phone number, get the last interaction with a contact
- Send text, files (images, videos, documents) and voice notes (converted to Opus with FFmpeg)
- Download the media of a message, verified against the SHA-256 that WhatsApp includes in the message
- Recover expired media: when the file is gone from WhatsApp's servers, the bridge asks your phone to upload it again
- Export every photo, audio, video and document of a chat to a folder, with a `manifest.csv` / `manifest.json` (date, sender, caption, size, SHA-256)
- Download new media automatically for the chats you pick
- Ask the phone to resend a chat's recent history, to fill in details older messages are missing
- Run the bridge as a background service on macOS, with a QR pairing page (`wa` helper script)

## Requirements

- [Go](https://go.dev/dl/) 1.26 or newer, with a C compiler (CGO is needed by `go-sqlite3`)
- Python 3.11 or newer
- [uv](https://docs.astral.sh/uv/): `curl -LsSf https://astral.sh/uv/install.sh | sh`
- An MCP client: Claude Desktop, Claude Code, Cursor, or similar
- FFmpeg (_optional_): converts audio to Opus `.ogg` so `send_audio_message` can send it as a voice note. Without it, send `.ogg` Opus files, or use `send_file` for other audio. `ffprobe` also fills audio and video durations in exports.

## Installation

### 1. Clone the repository

```bash
git clone https://github.com/anboia/whatsapp-mcp.git
cd whatsapp-mcp
```

### 2. Run the WhatsApp bridge

```bash
cd whatsapp-bridge
go run .
```

The first time, a QR code is printed in the terminal. On your phone open **WhatsApp → Settings → Linked devices → Link a device** and scan it. The code is also saved as `store/qr.png`, which `qr.html` displays and refreshes.

After pairing, the bridge keeps running and syncs your history. Leave it running while you use the MCP server. You may have to scan again after about 20 days.

On macOS you can run it as a background service instead. See [Running the bridge in the background (macOS)](#running-the-bridge-in-the-background-macos).

### 3. Add the MCP server to your client

Use this configuration, replacing the two placeholders:

```json
{
  "mcpServers": {
    "whatsapp": {
      "command": "{{PATH_TO_UV}}",
      "args": [
        "--directory",
        "{{PATH_TO_REPO}}/whatsapp-mcp-server",
        "run",
        "main.py"
      ]
    }
  }
}
```

- `{{PATH_TO_UV}}`: the output of `which uv`
- `{{PATH_TO_REPO}}`: the output of `pwd` in the cloned repository

Where to put it:

- **Claude Desktop**: `~/Library/Application Support/Claude/claude_desktop_config.json` (macOS) or `%APPDATA%\Claude\claude_desktop_config.json` (Windows)
- **Cursor**: `~/.cursor/mcp.json`
- **Claude Code**: `claude mcp add whatsapp -- {{PATH_TO_UV}} --directory {{PATH_TO_REPO}}/whatsapp-mcp-server run main.py`

### 4. Restart your client

WhatsApp should now show up as an available integration.

## MCP tools

| Tool | What it does |
| --- | --- |
| `search_contacts` | Search contacts by name or phone number |
| `list_messages` | Messages filtered by date, sender, chat or text, with surrounding context |
| `list_chats` | Chats with their last message, sorted by activity or name |
| `get_chat` | Metadata of one chat |
| `get_direct_chat_by_contact` | Find the direct chat with a phone number |
| `get_contact_chats` | All chats involving a contact |
| `get_last_interaction` | Most recent message with a contact |
| `get_message_context` | Messages before and after a given message |
| `send_message` | Send text to a phone number or a JID (person or group) |
| `send_file` | Send an image, video, document or raw audio file |
| `send_audio_message` | Send audio as a playable voice note (converted to Opus if needed) |
| `download_media` | Download and verify the media of a message, and return its local path |
| `download_chat_media` | Download all media of a chat, and optionally export it with a manifest |
| `request_chat_backfill` | Ask your phone to resend a chat's recent history to the bridge |

Messages that contain media show their type, message ID and chat JID, which is what `download_media` needs.

## Media

### Downloading

`download_media` saves the file under `whatsapp-bridge/store/<chat_jid>/` with a name that sorts by date and is unique per message, for example `20261001_225550_audio_3EB0A1B2C3D4E5F60718.ogg`. Documents keep their original name after the ID.

Every file is checked against the SHA-256 in the message. A file already on disk is reused only if its hash matches. A file with the right name but different content is moved aside (`.mismatch-<timestamp>`), never overwritten.

WhatsApp removes media from its servers after a while. When a download fails because of that, the bridge asks your phone to upload the file again. The phone must be online and answer within 60 seconds.

### Exporting a chat

`download_chat_media` downloads every media message of a chat, optionally filtered by type (`image`, `audio`, `video`, `document`) and date range. With `dest_dir`, it also copies the files there as read-only and writes:

- `manifest.json` and `manifest.csv`: chat, date, sender, caption, MIME type, duration, forwarded flag, original name, size and SHA-256 of each file

Running it again only copies new files. It also checks the files from earlier runs and flags any that went missing or were changed. Several chats can share one folder.

From the command line:

```bash
cd whatsapp-mcp-server
uv run export_media.py <chat_jid> ~/whatsapp-export --types image,video --after 2026-09-01T00:00:00Z
```

### Automatic download

List chat JIDs in `whatsapp-bridge/store/autodownload.txt`, one per line (`#` starts a comment, `*` means every chat). New media from those chats is downloaded as soon as it arrives. The file is read on every message, so there's no need to restart.

### Filling in older messages

Messages synced by older versions of the bridge may lack captions, sender IDs or download paths. `request_chat_backfill` (or `wa history <chat_jid>`) asks your phone to resend the chat's recent history. The bridge merges it into the database without losing what it already knows.

## Running the bridge in the background (macOS)

The `wa` script installs the bridge as a launchd service that starts at login and restarts if it stops.

```bash
ln -s "$PWD/wa" ~/.local/bin/wa   # any folder on your PATH
wa start                          # builds the bridge if needed, then starts the service
wa qr                             # opens the QR pairing page when a scan is needed
wa status                         # service, connection and database status
```

| Command | What it does |
| --- | --- |
| `wa start` / `stop` / `restart` | Control the service |
| `wa build` | Rebuild the bridge binary (then `wa restart`) |
| `wa status` | Service state, WhatsApp connection, chat and message counts |
| `wa qr` | Open the QR page when pairing is pending |
| `wa logs` | Follow `~/Library/Logs/whatsapp-bridge.log` |
| `wa media <chat_jid> [folder] [--types …] [--after …] [--before …]` | Export a chat's media |
| `wa history <chat_jid> [count]` | Ask the phone for a chat's recent history (1 to 500 messages) |
| `wa auto [list]` / `add <jid>` / `rm <jid>` | Manage automatic media download |

## Configuration

| Variable | Default | Used by |
| --- | --- | --- |
| `WHATSAPP_BRIDGE_PORT` | `8741` | Bridge, MCP server and `wa`. If you change it, set it for all three (for the MCP server, add it under `"env"` in the client config). |

## Architecture

```
MCP client (Claude) ──stdio──▶ Python MCP server ──HTTP 127.0.0.1──▶ Go bridge ──▶ WhatsApp
                                      │                                  │
                                      └──────── reads SQLite ◀───────────┘ writes
```

1. **Go bridge** (`whatsapp-bridge/`) connects to WhatsApp, handles QR pairing, stores chats and messages in `store/messages.db`, and downloads media. It exposes a small local REST API: `/api/send`, `/api/download`, `/api/download_chat`, `/api/backfill`, `/api/status`.
2. **Python MCP server** (`whatsapp-mcp-server/`) implements the MCP tools. It reads the SQLite database directly and calls the bridge to send messages and download media.

Data in `whatsapp-bridge/store/`:

| Path | Contents |
| --- | --- |
| `whatsapp.db` | whatsmeow session: device keys and contacts (**secret**) |
| `messages.db` | Chats and messages |
| `<chat_jid>/` | Downloaded media |
| `autodownload.txt` | Chats with automatic media download |
| `qr.png` | QR code while pairing is pending |

## Windows

`go-sqlite3` needs CGO, which is disabled by default on Windows, so you need a C compiler:

1. Install [MSYS2](https://www.msys2.org/) and add its `ucrt64\bin` folder to your `PATH` ([step-by-step guide](https://code.visualstudio.com/docs/cpp/config-mingw)).
2. Enable CGO and run the bridge:

   ```bash
   cd whatsapp-bridge
   go env -w CGO_ENABLED=1
   go run .
   ```

Without this, you'll see `Binary was compiled with 'CGO_ENABLED=0', go-sqlite3 requires cgo to work.` The `wa` script is macOS-only.

## Troubleshooting

- **`uv` not found by the client**: use the full path from `which uv` in the config.
- **Tools fail with a request error**: the bridge isn't running, or it's on a different port. Check with `curl http://127.0.0.1:8741/api/status`.
- **QR code not shown**: restart the bridge. If your terminal can't render it, open `whatsapp-bridge/qr.html` in a browser.
- **Device limit reached**: remove a linked device on your phone (Settings → Linked devices).
- **No messages yet**: the first history sync can take several minutes when you have many chats.
- **Media download fails with "no answer from the phone"**: the media expired on WhatsApp's servers and your phone was offline. Open WhatsApp on the phone and try again.
- **Out of sync**: stop the bridge, delete `whatsapp-bridge/store/messages.db` and `whatsapp-bridge/store/whatsapp.db`, start it again and scan the QR code.

For Claude Desktop integration issues, see the [MCP documentation](https://modelcontextprotocol.io/quickstart/server#claude-for-desktop-integration-issues).

## What's new in this fork

Compared with [lharries/whatsapp-mcp](https://github.com/lharries/whatsapp-mcp):

- Media downloads verified by SHA-256, with recovery of expired media through the phone
- `download_chat_media`: bulk download and export with a manifest
- `request_chat_backfill`: on-demand history from the phone
- Automatic media download per chat
- Captions, sender JIDs, durations, forwarded flags and quoted replies recorded for each message
- History sync unwraps ephemeral and view-once messages, and doesn't overwrite data learned earlier
- Bridge API bound to `127.0.0.1` on port `8741` (configurable) instead of all interfaces on `8080`
- QR code saved as an image, and a macOS background service (`wa`)
- Up-to-date whatsmeow and dependencies, plus tests and CI

## Development

```bash
cd whatsapp-bridge && go vet ./... && go test ./...
cd whatsapp-mcp-server && uv run python -c "import main"
```

Issues and pull requests are welcome. Please never include anything from `store/`, real phone numbers or message contents in issues, logs or test fixtures.

## License

[MIT](LICENSE). Originally created by Luke Harries.
