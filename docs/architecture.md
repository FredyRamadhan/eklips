# eklips — Architecture

This document is the contract for the implementation. Where `specsheet.md` was
ambiguous, the resolution chosen here is recorded under
[Resolved ambiguities](#resolved-ambiguities).

## 1. Decisions

| Area | Decision |
| --- | --- |
| Process model | A long-running daemon owns the playback engine, state, and database. CLI commands and the TUI are thin HTTP clients. One-shot CLI processes never hold state. |
| UI | Headless by default. `eklips`, `eklips serve`, and `eklips start` never initialise a UI. `eklips tui` is an opt-in client compiled into the same binary. |
| TUI | Attaches over HTTP, owns no playback state, never embeds an engine. If no local daemon answers `/api/v1/health`, it re-executes the same binary detached (`serve` + forwarded flags), waits ≤5 s for health, then attaches. `--no-start` disables this, `--remote` never auto-starts, `--stop-on-exit` stops only a daemon this TUI started. |
| Control plane | One HTTP API under `/api/v1`, bound to `127.0.0.1:8080` by default. The same handlers serve the CLI, TUI, external HTTP clients, and tests. |
| Events | `GET /api/v1/events` (SSE). Full snapshot on connect, then typed incremental events. Heartbeat comment every 15 s. `Last-Event-ID` replays from a 256-event ring; if the id is too old, a fresh snapshot is sent. |
| Sinks | `device`, `http`, `both`, `null` (tests). Default `device`; Docker defaults to `http`. |
| Docker | No sound card assumed. Stream-first (`--sink=http --bind=0.0.0.0`). `/dev/snd` passthrough is an opt-in for device mode. |
| Database | SQLite through `modernc.org/sqlite` (pure Go). WAL, `foreign_keys=ON`, `busy_timeout=5000`, embedded versioned migrations, prepared statements, a single writer goroutine. Default path `$XDG_DATA_HOME/eklips/eklips.db` → `~/.local/share/eklips/eklips.db`; `--db` overrides; Docker uses `/data/eklips.db`. |
| State model | Playback `stopped | playing | paused`; source `single | playlist`. The loaded queue persists across `stop` and restarts. On boot, queue and volume are restored as `stopped` unless `--resume-on-start` (or the `resume_on_start` setting) is enabled. |
| Formats | Native: WAV (PCM 8/16/24/32, float32), MP3 (`hajimehoshi/go-mp3`), OGG/Vorbis (`jfreymuth/oggvorbis`), FLAC (`mewkiz/flac`). Optional FFmpeg subprocess decoder for M4A/AAC/Opus/etc. when `ffmpeg` is on `PATH`. Otherwise `UNSUPPORTED_FORMAT`. |
| Platform | Linux first (ALSA output via `ebitengine/oto/v3`, PipeWire/PulseAudio device-change detection via `pactl`). macOS/Windows best-effort. |
| cgo | Only the device sink needs cgo (ALSA on Linux). It is isolated behind the `cgo` build tag; `CGO_ENABLED=0` builds return `SINK_UNAVAILABLE` for `device`/`both`. |
| Security | Loopback bind; bearer token mandatory on non-loopback binds unless `--allow-remote`; paths normalised and optionally restricted with `--media-root`; no shell interpolation (direct `exec`); 64 KiB request body limit; header/read timeouts; non-root container; token never logged or persisted. |
| Playlist schema | Versioned JSON: `{ "version": 1, "name": "...", "tracks": [{ "path": "..." }] }`. Relative paths resolve against the playlist file's directory. |
| Terminal player (Phase 12) | The HTTP API remains the internal transport and stays backward compatible. Seeking, queue mutation, browse, playlist inspection, and metering are exposed as additive `/api/v1` endpoints so the TUI can run locally or remotely. The TUI owns no audio state. |
| Metadata | Native tag readers in `internal/audio/tags.go`: ID3v2.2/2.3/2.4 for MP3, Vorbis Comments for FLAC/OGG, RIFF `INFO`/ID3 chunks for WAV. Filename heuristics (`Artist - Title`, `01. Title`) are the fallback. Tags are cached per path; queues over 500 tracks use filename metadata except for the current track so a load never stalls the audio actor. |
| Themes | Five built-in palettes (`default`, `nord`, `gruvbox`, `tokyonight`, `catppuccin`) compiled into the TUI. Selection is `--theme` or `:theme <name>`; unknown names are rejected without changing the active palette. |

## 2. Package layout

```
cmd/eklips            entry point; dispatches to internal/cli
internal/apperr       error codes, HTTP status mapping, JSON envelope
internal/config       daemon/client options, defaults, path resolution
internal/media        supported extensions, folder scan with natural sort
internal/playlist     JSON parse/validate/generate
internal/core         pure state machine (no I/O)
internal/audio        decoders, resampler, gapless trimming, tag readers, PCM helpers
internal/sink         Sink interface; null, device (cgo), HTTP broadcaster
internal/engine       playback actor: core + audio + sinks + events + persistence
internal/events       event bus with replay ring
internal/store        SQLite persistence and migrations
internal/server       HTTP API v1
internal/client       HTTP client shared by CLI and TUI
internal/daemon       lifecycle: serve, background start, single-instance lock
internal/devwatch     output-device change detection
internal/tui          Bubble Tea client
```

## 3. State machine

```mermaid
stateDiagram-v2
    [*] --> stopped
    stopped --> playing: play (source loaded / given)
    playing --> paused: pause / play (toggle via TUI) / device change
    paused --> playing: play
    playing --> stopped: stop / end of queue
    paused --> stopped: stop
    playing --> playing: next / prev / gapless splice
```

Precondition checks run in this order: `NO_MEDIA_LOADED` → `PLAYLIST_REQUIRED` → `INVALID_STATE` → bounds (`VOLUME_MAX`/`VOLUME_MIN`).

| Command | Requires | Errors |
| --- | --- | --- |
| `play <path>` | — | `FILE_NOT_FOUND`, `UNSUPPORTED_FORMAT`, `DECODE_ERROR`, `INVALID_PLAYLIST` |
| `play` | source | `NO_MEDIA_LOADED`; idempotent when already playing |
| `pause` | source, `playing` | `NO_MEDIA_LOADED`, `INVALID_STATE` |
| `stop` | source, `playing`/`paused` | `NO_MEDIA_LOADED`, `INVALID_STATE` |
| `next` / `previous` | playlist, `playing`/`paused` | `NO_MEDIA_LOADED`, `PLAYLIST_REQUIRED`, `INVALID_STATE` |
| `louder` / `quieter` | source | `NO_MEDIA_LOADED`, `VOLUME_MAX`, `VOLUME_MIN` |
| `mute` | source | `NO_MEDIA_LOADED` |
| `shuffle` / `loop` | playlist | `NO_MEDIA_LOADED`, `PLAYLIST_REQUIRED` |
| `status` | — | never fails (reports empty source) |

Queue semantics:

- The queue is the list of tracks plus a play *order* (identity, or a permutation when shuffled) and a cursor into that order.
- `select(index)` jumps to a track (starting playback if stopped) without rebuilding the source. `remove(index)` drops a track: removing a non-current track preserves the active decoder; removing the current track starts the next one or clears an empty queue. `move(from,to)` reorders tracks while preserving the current track identity. `clear()` empties the queue and stops. `append(paths)` expands directories, converts a single-file source into a playlist, and starts playback if nothing was loaded.
- Every queue mutation bumps the generation counter, invalidating any in-flight prefetch.
- Enabling shuffle builds a permutation whose first element is the current track, so the current track is not restarted. Disabling shuffle restores identity order with the cursor on the current track.
- `next` at the last track: wraps if loop is on, otherwise `INVALID_STATE` ("end of playlist").
- `previous` at the first track: wraps if loop is on, otherwise restarts the first track.
- Natural end of queue without loop → `stopped`, cursor reset to the first track.
- Single-file sources may be loaded with `--loop`; the file then repeats gaplessly.
- Volume steps are 5 within `0..100`; mute is independent of volume (volume changes do not unmute).

## 4. Playback pipeline

```mermaid
flowchart LR
    D["Decoder (native rate/channels)"] --> T["Gapless trim (delay/padding)"]
    T --> R["Resampler → output rate, stereo float32"]
    R --> A["Actor loop: gain + soft-knee limiter → int16"]
    A --> DS["Device sink ring (1000 ms) + 100 ms HW buffer"]
    A --> B["Broadcaster"]
    B --> L1["Listener 1 (bounded chan)"]
    B --> L2["Listener N (bounded chan)"]
```

- **Actor model.** One goroutine (the *engine loop*) owns the `core.Machine`, the current decoder, and the prefetched decoder. Commands arrive over a channel and get a reply; readers get lock-free snapshots via `atomic.Pointer`. There is no lock in the audio path.
- **Fixed output format.** 44 100 Hz (configurable via `--sample-rate`), stereo, signed 16-bit little-endian. Tracks at other rates are resampled (linear) per track so splices are sample-accurate in the output domain.
- **Chunking.** 20 ms chunks (882 frames at 44.1 kHz). Buffers are preallocated; stream chunks come from a `sync.Pool` and are reference-counted across listeners.
- **Clock & Pre-roll.** The device sink paces the loop (blocking ring write into a 1000 ms SPSC ring feeding a 100 ms `oto` hardware buffer, with ~160 ms pre-roll priming before unpausing). Without a device, a wall-clock pacer paces the loop at 1× so HTTP listeners receive real time audio.
- **Gapless.** When less than 10 s of the current track remains, a helper goroutine opens the next track (chosen with current shuffle/loop state) and pre-decodes ~0.5 s. On EOF the remainder of the current chunk is filled from the next track — a sample-accurate splice with no output reset. Index/metadata events are emitted at the splice. Prefetch is one track at most; any command that changes queue, shuffle, or loop bumps a generation counter that invalidates the prefetch. Undecodable tracks are skipped with an `error` event; if none remain, playback stops.
- **Encoder delay/padding.** MP3: LAME/Xing tag delay+padding (plus the 529-sample decoder delay) trimmed. FLAC/WAV are exact. Vorbis end-trim is handled by the decoder's granule positions. AAC via FFmpeg relies on FFmpeg's iTunSMPB/edit-list handling.
- **Slow consumers.** Each HTTP listener has a bounded channel (2 s of audio). If full, the listener is evicted; the decoder never blocks on a listener.
- **Paused/stopped streams.** While not playing, the loop emits 20 ms silence chunks only if HTTP listeners are connected, so connections stay open; with no listeners the loop blocks on its command channel (zero idle CPU).
- **Stream format.** `audio/wav` with a streaming RIFF header (sizes set to max), followed by raw PCM. Live streams do not support byte ranges: `Accept-Ranges: none`, Range headers are ignored and a 200 is returned (RFC 9110 permits this).

### Seeking

- `POST /api/v1/seek` accepts either `position_ms` (absolute) or `delta_ms` (relative), matching the TUI's `h`/`l` (∓5 s) and `[`/`]` (∓30 s) keys.
- **Forward seek:** discard frames with `Track.Skip` in the output sample domain. No decoder reopen, no sink flush.
- **Backward seek:** reopen the decoder, skip to the target frame, flush the device ring, and re-prime ~160 ms before resuming. The sink and HTTP stream stay open.
- **Past the end:** behaves like a natural transition — `Advance` selects the next track (or stops, or repeats for a looping single file). Seeking is a no-op error (`INVALID_STATE`) when nothing is playing.
- Seeking publishes a `state` event containing the updated status/position and persists immediately.

### Stereo metering

The actor loop computes per-chunk post-gain, post-limiter peak amplitudes for left and right and stores them in atomics. `GET /api/v1/levels` converts them to dBFS with an exponential decay (~30 ms / 0.72 factor) so the TUI meter falls smoothly. The TUI polls it at 8 Hz only while playing; it is not part of the SSE stream.

### Metadata

`audio.ReadMetadata` reads only tag headers (never whole files; capped at 4 MiB). Results are cached in the engine. `Status` and every `QueueItem` carry `metadata` (`title`, `artist`, `album`, `year`, `track_number`), and the TUI renders `Artist — Title (Album)`. For queues larger than 500 tracks only the current track and the browser's visible directory are parsed; other rows use filename heuristics.

## 5. Database schema

```sql
schema_migrations(version INTEGER PRIMARY KEY, applied_at TEXT)
settings(key TEXT PRIMARY KEY, value TEXT)
aliases(name TEXT PRIMARY KEY, playlist_path TEXT, created_at TEXT, updated_at TEXT)
playlists(id INTEGER PRIMARY KEY, name TEXT, source_path TEXT UNIQUE, created_at TEXT, updated_at TEXT)
playlist_tracks(playlist_id INTEGER REFERENCES playlists(id) ON DELETE CASCADE, position INTEGER, path TEXT, PRIMARY KEY(playlist_id, position))
playback_state(id INTEGER PRIMARY KEY CHECK(id = 1), source_type TEXT, source_path TEXT, current_index INTEGER, position_ms INTEGER, state TEXT, volume INTEGER, muted INTEGER, shuffle INTEGER, loop INTEGER, updated_at TEXT)
```

- Loading a playlist (folder or JSON) upserts it into `playlists`/`playlist_tracks` in one transaction (import). On restore, the source is re-resolved; if the source path no longer resolves, the stored tracks are used.
- A single persister goroutine writes `playback_state`. Transitions are written immediately (coalesced); position-only updates at most every 5 s; final write on shutdown.
- `PRAGMA user_version` above the binary's latest migration → `DB_ERROR` ("database schema is newer"). Corrupt files (`SQLITE_NOTADB`) and locked files fail with `DB_ERROR`, never silently reset.
- Single-instance: an exclusive `flock` on `<db>.lock`.

## 6. Event stream

```
id: 42
event: volume
data: {"volume":55,"muted":false}
```

Event types: `snapshot`, `state`, `track`, `queue`, `volume`, `mute`, `shuffle`, `loop`, `error`, `device`, `shutdown`. `snapshot`/`track`/`state` carry the full status object; others carry the changed fields. Queue selection/reorder/remove/append emit `queue` (and `track` when the current track changes); seeks emit `state` with the new position. Heartbeat: `: ping` every 15 s. Subscribers have a 64-event buffer; overflow evicts the subscriber (client reconnects with `Last-Event-ID`).

## 7. TUI architecture

The TUI is a pure Bubble Tea client (`internal/tui`) with three views selected by
`1`/`2`/`3`/`Tab`:

| View | State | Data source |
| --- | --- | --- |
| Queue & Now Playing | cursor (distinct from the playing index), scroll offset, filter | `status`/`queue` snapshots + `state`/`track`/`queue` SSE events |
| File Browser | current directory, entries, cursor, filter | `GET /api/v1/browse?path=` (daemon-side path validation and metadata) |
| Playlists & Aliases | saved playlist/alias rows, preview | `GET /api/v1/playlists`, `GET /api/v1/playlists/tracks`, `GET /api/v1/aliases` |

- **Rendering.** The header (connection badge + theme), tab bar, view body, banners,
  and footer are rendered into one cached string. `View()` rebuilds only when a
  model field changes (`dirty`); the renderer is capped at 30 FPS. Styles are
  precomputed per theme, so switching themes is an O(1) style swap.
- **Progress vs. metering.** Progress is interpolated locally at 4 Hz from the
  last `state`/`track` snapshot. Meter levels are fetched at 8 Hz only while
  playing. Neither poll runs while stopped or paused.
- **Queue cursor.** `cursorIndex` is an absolute index into the queue, independent
  of `status.TrackIndex`. Fuzzy filtering produces a visible-index list; `j`/`k`
  move within that list and `Enter` sends the absolute index.
- **Input modes.** `/` opens an inline filter, `:` opens the command bar. Input
  mode captures all keys except `Esc`/`Enter`; a cancelled filter restores the
  previous value.
- **Confirmation.** `Q`, `C`, and playlist deletion open a `y/N` prompt; the
  default action is cancel.
- **Terminal safety.** The terminal is restored on normal exit, panic (deferred
  `ReleaseTerminal`), and context cancellation. Resize is handled by
  `WindowSizeMsg` without restarting the program.
- **Auth token.** Held only in the HTTP client; never rendered, logged, or
  persisted.

## 8. Performance budgets

| Metric | Budget |
| --- | --- |
| Stream listeners at 1× without dropouts | ≥ 10 (default max 32) |
| Idle CPU (stopped, no listeners, no TUI) | ≈ 0 — no timers running |
| Memory | bounded: device ring 1000 ms (~176 KiB), 2 s per listener, ≤1 prefetched track (0.5 s prebuffer + decoder state), queue ≤ 10 000 tracks |
| DB writes during steady playback | ≤ 1 per 5 s |
| Decode throughput (WAV/FLAC) | ≥ 50× real time on one core |
| Gapless splice | 0 inserted samples; < 1 chunk of extra work |
| Fan-out publish (10 listeners, 20 ms chunk) | < 20 µs |
| Status/command HTTP latency (loopback) | < 5 ms p99 excluding decoder open |
| TUI | 4 Hz progress + 8 Hz meter only while playing; ≤ 30 FPS; View() < 200 µs |
| Metadata | header-only reads, cached; ≤ 1 tag read per path; huge queues use filename fallback |
| Seek (backward) | one decoder reopen + flush + ≤160 ms pre-roll; no sink/stream restart |

Benchmarks in `internal/audio`, `internal/sink`, `internal/engine`, `internal/store`, and `internal/tui` are the acceptance gates; `scripts/bench.sh` compares against `bench/baseline.txt` with `benchstat`.

## 9. Resolved ambiguities

1. **`next`/`previous` when stopped.** The spec lists both `PLAYLIST_REQUIRED` and "requires active playback". Resolution: single-file → `PLAYLIST_REQUIRED`; playlist but stopped → `INVALID_STATE`.
2. **`status` with nothing loaded.** The state model says status requires a source, but `eklips status` must report daemon status. Resolution: status always succeeds and reports `source_type: ""`.
3. **`loop` for single files.** The `loop` toggle requires a playlist, but `play --loop` may target a single file. Resolution: `play <file> --loop` repeats the file; the `loop` toggle still requires a playlist.
4. **Shutdown endpoint.** `eklips quit` and TUI `Q` need an API: `POST /api/v1/shutdown` (authenticated).
5. **Byte ranges on a live stream.** Not feasible; documented as `Accept-Ranges: none`.
6. **Health endpoint** is unauthenticated (Docker healthcheck, TUI probe) and reveals only `{"status":"ok","version":...}`.
