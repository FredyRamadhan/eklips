# eklips

`eklips` is a lightweight, headless-first audio player microservice and daemon with an optional built-in terminal UI (`eklips tui`).

A single long-running daemon owns the playback engine, gapless decoder pipeline, audio sinks (`device`, `http`, `both`, `null`), and SQLite persistence (`modernc.org/sqlite`). All CLI commands, the TUI, and external integrations communicate with the daemon over its `/api/v1` HTTP and Server-Sent Events (SSE) API.

---

## Features

- **Headless by default:** `eklips`, `eklips serve`, and `eklips start` never initialize a UI.
- **Built-in TUI client (`eklips tui`):** Compiled into the same binary; attaches over HTTP/SSE, auto-starts a local daemon when needed, and exits without stopping a pre-existing daemon.
- **Gapless playback:** Sample-accurate PCM splicing across consecutive playlist tracks with 1-track prefetch and LAME/Xing encoder delay & padding trimming. Both the audio device and HTTP stream stay open continuously across boundaries without resets or silence insertion.
- **Single decode, bounded fan-out:** Audio is decoded once and broadcast to the host audio device and up to 32 concurrent HTTP WAV stream listeners via pooled, reference-counted ring buffers. Slow listeners are evicted without stalling playback.
- **Format support:**
  - Native Go decoders: **WAV** (8/16/24/32-bit PCM, float32/64), **MP3**, **OGG/Vorbis**, **FLAC**.
  - Optional FFmpeg backend: **M4A/AAC**, **Opus**, **AIFF**, **ALAC**, and other formats when `ffmpeg` is on `PATH`.
- **SQLite service database:** WAL mode, embedded migrations, prepared statements, aliases, playlist import/export, and coalesced state persistence (at most one position write per 5 seconds during steady-state playback).
- **Device-change auto-pause:** Subscribes to Linux PipeWire/PulseAudio sink and port events (`pactl subscribe`, with configurable polling fallback) and automatically pauses playback when headphones or output devices are plugged/unplugged.

---

## Quick Start

### Build

```bash
go build -o eklips ./cmd/eklips
```

### Start the daemon and play media

```bash
# Start the daemon in the background (defaults to 127.0.0.1:8080, --sink=device)
./eklips start

# Play a single audio file
./eklips play /path/to/song.flac

# Play all audio files in a folder in natural sort order
./eklips play /path/to/album

# Play the current directory with loop enabled
./eklips play . --loop

# Query status and queue
./eklips status
./eklips queue

# Launch the interactive terminal UI
./eklips tui

# Stop the daemon
./eklips quit
```

---

## CLI Reference

### Service Lifecycle

| Command | Description |
| --- | --- |
| `eklips` / `eklips serve` | Start the daemon in the foreground (`Ctrl+C` for graceful shutdown) |
| `eklips start` | Start the daemon in the background and wait for `/api/v1/health` |
| `eklips status` | Report daemon and playback status |
| `eklips queue` | List tracks in the loaded queue |
| `eklips quit` | Gracefully stop the running daemon |
| `eklips help` | Show command and flag reference |

### Common Options

| Option | Env | Default | Description |
| --- | --- | --- | --- |
| `--db <path>` | `EKLIPS_DB` | `$XDG_DATA_HOME/eklips/eklips.db` | SQLite database file path |
| `--sink <mode>` | `EKLIPS_SINK` | `device` | Output sink: `device`, `http`, `both`, or `null` |
| `--bind <addr>` | `EKLIPS_BIND` | `127.0.0.1` | HTTP control API bind address |
| `--port <port>` | `EKLIPS_PORT` | `8080` | HTTP control API port |
| `--auth-token <tok>` | `EKLIPS_TOKEN` | *(empty)* | Bearer token required for HTTP/CLI/TUI clients |
| `--allow-remote` | — | `false` | Allow binding to non-loopback address without `--auth-token` |
| `--resume-on-start` | — | `false` | Restore playback as `playing` instead of `stopped` on startup |
| `--media-root <dir>` | — | *(empty)* | Restrict playable files, playlists, and TUI browsing to `<dir>` |
| `--theme <name>` | — | `default` | TUI palette: `default`, `nord`, `gruvbox`, `tokyonight`, `catppuccin` |

### Playback Controls

| Command | Description |
| --- | --- |
| `eklips play [<file\|dir\|.>] [--loop]` | Load and play a file or directory, or resume the loaded queue |
| `eklips pause` | Pause active playback |
| `eklips stop` | Stop playback while retaining the loaded queue |
| `eklips next` | Advance to the next track in a playlist |
| `eklips prev` | Return to the previous track in a playlist |
| `eklips louder` | Increase volume by 5 (`0..100`) |
| `eklips quieter` | Decrease volume by 5 (`0..100`) |
| `eklips mute` | Toggle mute (retains previous volume) |
| `eklips shuffle` | Toggle playlist shuffle without restarting the current track |
| `eklips loop` | Toggle playlist loop |

### Playlists & Aliases

```bash
# Write a starter playlist.json template in the current directory
eklips playlist create [--force]

# Scan a folder and write <directory>/playlist.json (or --output <path>)
eklips playlist create /path/to/album [--output /path/to/playlist.json] [--force]

# Play a JSON playlist by path
eklips playlist play /path/to/playlist.json [--loop]

# Manage and play named playlist aliases stored in SQLite
eklips playlist alias set chill /path/to/playlist.json
eklips playlist alias list
eklips playlist play chill
eklips playlist alias remove chill
```

---

## Terminal UI (`eklips tui`)

```
 eklips  •  http://127.0.0.1:8080   PLAYING   theme:nord
 [1: Queue (3)]  [2: Files]  [3: Playlists (2)]

 Now Playing
   The Cinematic Orchestra — To Build a Home (Ma Fleur)
   playlist: /music/symphony
   ███████████████░░░░░░░░░░░░░░░░░░░░░░░░  01:42 / 03:24
   L: [██████████████░░░░░░]   -8.2 dB   R: [█████████████░░░░░░░]   -9.7 dB

    Vol: 80%    Shuffle: OFF    Loop: ON

 Queue (3)  filter: home
 ❯▶  1. The Cinematic Orchestra — To Build a Home (Ma Fleur)
     2. Ólafur Arnalds — Near Light (For Now I Am Winter)
     3. Nils Frahm — Says (Spaces)

 space play/pause · enter play · j/k move · h/l seek ±5s · [/] ±30s · x/d remove · J/K reorder · C clear · / filter · 1/2/3 view · ? help · q quit
```

The TUI is a full terminal music player with three views:

1. **Queue & Now Playing** — metadata-rich queue, cursor selection, reorder/remove,
   inline fuzzy filter, progress bar, stereo VU meter.
2. **File Browser** — navigate the daemon's media root (or cwd), preview ID3/Vorbis
   metadata, play a file/folder directly (`Enter`), or append with `a`.
3. **Playlists & Aliases** — load saved database playlists/aliases, inspect their
   tracks, and delete them with confirmation.

### TUI Options

- `eklips tui`: Attach to local daemon (auto-starts one in the background if not running).
- `eklips tui --no-start`: Fail with `SERVICE_NOT_RUNNING` instead of auto-starting.
- `eklips tui --stop-on-exit`: Stop the daemon when exiting the TUI, **only** if this TUI invocation auto-started it.
- `eklips tui --remote http://host:8080 --auth-token <token>`: Connect to a remote or containerized daemon (never auto-starts). For plain HTTP over untrusted networks, use an SSH tunnel (`ssh -L 8080:127.0.0.1:8080 user@host`).
- `eklips tui --theme <name>`: Select `default`, `nord`, `gruvbox`, `tokyonight`, or `catppuccin`.
  Use `:theme <name>` at runtime to switch without restarting.

### TUI Keymap

| Key | Action |
| --- | --- |
| `1` / `2` / `3`, `Tab` | Switch Queue / Files / Playlists view |
| `space` | Play / Pause |
| `s` | Stop |
| `n` / `p` | Next / Previous track |
| `+` / `-` | Volume up / down (step 5) |
| `m` | Toggle mute |
| `r` | Toggle shuffle |
| `L` | Toggle loop |
| `↑` / `k`, `↓` / `j` | Move cursor |
| `g` / `G` | Jump to first / last item |
| `Enter` | Play highlighted queue track, open folder, or load playlist |
| `h` / `l`, `←` / `→` | Seek −5s / +5s |
| `[` / `]` | Seek −30s / +30s |
| `x` / `d` | Remove highlighted queue track (or delete playlist) |
| `J` / `K` | Move queue track down / up |
| `C` | Clear the queue (confirmation) |
| `/` | Inline fuzzy filter (queue or file browser) |
| `a` | Append highlighted file/folder to the queue |
| `p` | Replace queue with highlighted file/folder and play |
| `⌫` / `h` | Parent directory (Files view) |
| `:` | Command line (`:theme`, `:seek`, `:open`, `:clear`, `:filter`, …) |
| `?` | Toggle full help |
| `q` / `Esc` | Quit TUI only (daemon keeps running) |
| `Q` | Quit running daemon (prompts `y/N` confirmation) |

Metadata is read natively from ID3v2 (MP3), Vorbis Comments (FLAC/OGG), and RIFF
`INFO` chunks (WAV), with a filename fallback for untagged or FFmpeg-only files.

---

## HTTP API v1 Reference

Full OpenAPI 3.0 specification: [`docs/openapi.yaml`](file:///home/frdn/Documents/coding/services/eklips/docs/openapi.yaml).

| Method & Path | Description |
| --- | --- |
| `GET /api/v1/health` | Unauthenticated liveness check (`{"status":"ok","version":"1.0.0"}`) |
| `GET /api/v1/status` | Current playback and daemon status |
| `GET /api/v1/queue` | Current queue tracks and play order |
| `GET /api/v1/events` | Server-Sent Events (`snapshot`, `state`, `track`, `queue`, `volume`, `mute`, `shuffle`, `loop`, `error`, `device`, `shutdown`; supports `Last-Event-ID`) |
| `GET /api/v1/stream` | Continuous gapless `audio/wav` PCM stream (supports multiple concurrent listeners) |
| `POST /api/v1/play` | Play (`{"path":"/optional/path","loop":false}`) or resume |
| `POST /api/v1/pause` | Pause playback |
| `POST /api/v1/stop` | Stop playback |
| `POST /api/v1/next` | Next playlist track |
| `POST /api/v1/previous` | Previous playlist track |
| `POST /api/v1/volume/up` | Increase volume by 5 |
| `POST /api/v1/volume/down` | Decrease volume by 5 |
| `POST /api/v1/mute` | Toggle mute |
| `POST /api/v1/shuffle` | Toggle shuffle |
| `POST /api/v1/loop` | Toggle loop |
| `POST /api/v1/shutdown` | Gracefully shut down the daemon |
| `POST /api/v1/seek` | Seek (`{"position_ms":120000}` or `{"delta_ms":-5000}`) |
| `GET /api/v1/levels` | Real-time stereo peak meter in dBFS |
| `GET /api/v1/browse?path=...` | Directory listing with folders, audio files, and metadata |
| `POST /api/v1/queue/play` | Jump to a queue track (`{"index":2}`) |
| `POST /api/v1/queue/remove` | Remove a queue track (`{"index":2}`) |
| `POST /api/v1/queue/move` | Reorder a queue track (`{"from":2,"to":0}`) |
| `POST /api/v1/queue/clear` | Clear the queue and stop |
| `POST /api/v1/queue/append` | Append files/directories (`{"paths":["/music/album"]}`) |
| `GET /api/v1/playlists` | List saved playlists with track counts |
| `GET /api/v1/playlists/tracks?target=...` | Inspect a saved playlist and its tracks |
| `DELETE /api/v1/playlists?target=...` | Delete a saved playlist or alias |
| `POST /api/v1/playlists/create` | Generate playlist JSON (`{"dir":"...","output":"...","force":false}`) |
| `POST /api/v1/playlists/play` | Play playlist JSON or alias (`{"target":"...","loop":false}`) |
| `GET /api/v1/aliases` | List saved aliases |
| `PUT /api/v1/aliases/{name}` | Create or update alias (`{"path":"/path/to/playlist.json"}`) |
| `DELETE /api/v1/aliases/{name}` | Delete alias |

All error responses use a consistent JSON envelope:

```json
{
  "error": {
    "code": "FILE_NOT_FOUND",
    "message": "/music/missing.mp3 does not exist"
  }
}
```

---

## Docker Guide

By default, the Docker image runs as non-root user `eklips` (`10001:10001`) in stream-first mode (`--sink=http --bind=0.0.0.0 --port=8080 --db=/data/eklips.db`) requiring no sound card.

```bash
# Build image
docker build -t eklips:latest .

# Run container with persistent database volume and read-only media mount
docker run -d --name eklips \
  -p 8080:8080 \
  -e EKLIPS_TOKEN=my-secret-token \
  -v eklips_data:/data \
  -v /path/to/music:/music:ro \
  eklips:latest

# Control the container from the host TUI or CLI
eklips tui --remote http://127.0.0.1:8080 --auth-token my-secret-token
eklips play /music --remote http://127.0.0.1:8080 --auth-token my-secret-token

# Listen to the live HTTP stream
mpv http://127.0.0.1:8080/api/v1/stream --http-header-fields="Authorization: Bearer my-secret-token"
```

### Optional Host Audio Device Passthrough (`/dev/snd`)

To output directly to a Linux host's ALSA sound card from inside Docker, pass `/dev/snd` and set `--sink=device` or `--sink=both`:

```bash
docker run -d --name eklips \
  --device /dev/snd:/dev/snd \
  -p 8080:8080 \
  -v eklips_data:/data \
  -v /path/to/music:/music:ro \
  eklips:latest serve --sink=both --bind=0.0.0.0 --port=8080 --db=/data/eklips.db --allow-remote
```

> **Note:** Running `eklips tui` inside the container is not supported; run `eklips tui --remote <url>` on the host.

---

## Maintenance, Upgrades & Troubleshooting

- **Database migrations:** Schema migrations are embedded in the binary and applied automatically inside a transaction on startup (`schema_migrations` table and `PRAGMA user_version`).
- **Downgrade protection:** If a database was migrated by a newer version of `eklips`, an older binary refuses to open it (`DB_ERROR: database schema version X is newer than this binary supports`) rather than risking corruption. Back up `~/.local/share/eklips/eklips.db` before major upgrades.
- **Single-instance lock:** The daemon acquires an exclusive `flock` on `<db>.lock`. If a second daemon fails with `ALREADY_RUNNING`, verify no other `eklips` process owns the same `--db` path.
- **Platform notes:** Linux is the primary platform (PulseAudio/PipeWire/ALSA output and `pactl` device-change detection). macOS and Windows are supported on a best-effort basis.
