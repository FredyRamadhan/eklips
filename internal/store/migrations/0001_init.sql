-- 0001: initial schema
CREATE TABLE IF NOT EXISTS settings (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS aliases (
    name          TEXT PRIMARY KEY,
    playlist_path TEXT NOT NULL,
    created_at    TEXT NOT NULL,
    updated_at    TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS playlists (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    name        TEXT NOT NULL,
    source_path TEXT NOT NULL UNIQUE,
    created_at  TEXT NOT NULL,
    updated_at  TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS playlist_tracks (
    playlist_id INTEGER NOT NULL REFERENCES playlists(id) ON DELETE CASCADE,
    position    INTEGER NOT NULL,
    path        TEXT NOT NULL,
    PRIMARY KEY (playlist_id, position)
);

CREATE TABLE IF NOT EXISTS playback_state (
    id            INTEGER PRIMARY KEY CHECK (id = 1),
    source_type   TEXT NOT NULL DEFAULT '',
    source_path   TEXT NOT NULL DEFAULT '',
    current_index INTEGER NOT NULL DEFAULT 0,
    position_ms   INTEGER NOT NULL DEFAULT 0,
    state         TEXT NOT NULL DEFAULT 'stopped',
    volume        INTEGER NOT NULL DEFAULT 80,
    muted         INTEGER NOT NULL DEFAULT 0,
    shuffle       INTEGER NOT NULL DEFAULT 0,
    loop          INTEGER NOT NULL DEFAULT 0,
    updated_at    TEXT NOT NULL
);
