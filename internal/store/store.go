// Package store is the SQLite persistence layer.
package store

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/frdn/eklips/internal/apperr"

	_ "modernc.org/sqlite" // pure-Go driver
)

//go:embed migrations/*.sql
var migrationFS embed.FS

// Store wraps the database. All access goes through a single connection,
// which makes it the single writer path and serialises transactions.
type Store struct {
	db     *sql.DB
	path   string
	writes atomic.Int64

	stmtGetSetting  *sql.Stmt
	stmtSetSetting  *sql.Stmt
	stmtSetAlias    *sql.Stmt
	stmtGetAlias    *sql.Stmt
	stmtDelAlias    *sql.Stmt
	stmtSavePlay    *sql.Stmt
	stmtLoadPlay    *sql.Stmt
	stmtListAliases *sql.Stmt
}

// Alias maps a name to a playlist file.
type Alias struct {
	Name         string    `json:"name"`
	PlaylistPath string    `json:"playlist_path"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

// Playback is the persisted playback state row.
type Playback struct {
	SourceType   string
	SourcePath   string
	CurrentIndex int
	PositionMs   int64
	State        string
	Volume       int
	Muted        bool
	Shuffle      bool
	Loop         bool
	UpdatedAt    time.Time
}

const timeFmt = time.RFC3339Nano

// DefaultPath returns $XDG_DATA_HOME/eklips/eklips.db or the
// ~/.local/share fallback.
func DefaultPath() string {
	if d := os.Getenv("XDG_DATA_HOME"); d != "" && filepath.IsAbs(d) {
		return filepath.Join(d, "eklips", "eklips.db")
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return filepath.Join(os.TempDir(), "eklips", "eklips.db")
	}
	return filepath.Join(home, ".local", "share", "eklips", "eklips.db")
}

// Open opens (creating if needed) and migrates the database at path.
func Open(path string) (*Store, error) {
	if path == "" {
		path = DefaultPath()
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, apperr.Wrap(apperr.DBError, err, "invalid database path")
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o700); err != nil {
		return nil, apperr.Wrap(apperr.DBError, err, "create database directory")
	}
	if err := checkHeader(abs); err != nil {
		return nil, err
	}
	q := url.Values{}
	q.Add("_pragma", "busy_timeout(5000)")
	q.Add("_pragma", "foreign_keys(1)")
	q.Add("_pragma", "journal_mode(WAL)")
	q.Add("_pragma", "synchronous(NORMAL)")
	q.Set("_txlock", "immediate")
	dsn := "file:" + abs + "?" + q.Encode()
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, apperr.Wrap(apperr.DBError, err, "open database")
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(0)
	s := &Store{db: db, path: abs}
	if err := s.ping(); err != nil {
		db.Close()
		return nil, err
	}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	if err := s.prepare(); err != nil {
		db.Close()
		return nil, err
	}
	_ = os.Chmod(abs, 0o600)
	return s, nil
}

// checkHeader rejects non-empty files that are not SQLite databases, so a
// corrupt file is never silently replaced.
func checkHeader(path string) error {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return apperr.Wrap(apperr.DBError, err, "open database")
	}
	defer f.Close()
	hdr := make([]byte, 16)
	n, _ := f.Read(hdr)
	if n == 0 {
		return nil
	}
	if n < 16 || string(hdr) != "SQLite format 3\x00" {
		return apperr.New(apperr.DBError, "%s is not a SQLite database (corrupt or foreign file); refusing to overwrite", path)
	}
	return nil
}

func classify(err error, what string) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	switch {
	case strings.Contains(msg, "SQLITE_BUSY") || strings.Contains(msg, "database is locked"):
		return apperr.Wrap(apperr.DBError, err, "%s: database is locked by another process", what)
	case strings.Contains(msg, "not a database") || strings.Contains(msg, "malformed") || strings.Contains(msg, "SQLITE_CORRUPT") || strings.Contains(msg, "SQLITE_NOTADB"):
		return apperr.Wrap(apperr.DBError, err, "%s: database is corrupt", what)
	}
	return apperr.Wrap(apperr.DBError, err, "%s", what)
}

func (s *Store) ping() error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var mode string
	if err := s.db.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&mode); err != nil {
		return classify(err, "open database")
	}
	var res string
	if err := s.db.QueryRowContext(ctx, "PRAGMA quick_check").Scan(&res); err != nil {
		return classify(err, "check database")
	}
	if res != "ok" {
		return apperr.New(apperr.DBError, "database integrity check failed: %s", res)
	}
	return nil
}

type migration struct {
	version int
	name    string
	sql     string
}

func loadMigrations() ([]migration, error) {
	entries, err := migrationFS.ReadDir("migrations")
	if err != nil {
		return nil, err
	}
	var out []migration
	for _, e := range entries {
		name := e.Name()
		num, _, ok := strings.Cut(name, "_")
		if !ok {
			continue
		}
		v, err := strconv.Atoi(num)
		if err != nil {
			return nil, fmt.Errorf("bad migration name %q", name)
		}
		b, err := migrationFS.ReadFile("migrations/" + name)
		if err != nil {
			return nil, err
		}
		out = append(out, migration{version: v, name: name, sql: string(b)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].version < out[j].version })
	return out, nil
}

// LatestVersion is the newest schema version embedded in the binary.
func LatestVersion() int {
	ms, _ := loadMigrations()
	if len(ms) == 0 {
		return 0
	}
	return ms[len(ms)-1].version
}

func (s *Store) migrate() error {
	ms, err := loadMigrations()
	if err != nil {
		return apperr.Wrap(apperr.DBError, err, "load migrations")
	}
	ctx := context.Background()
	if _, err := s.db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version INTEGER PRIMARY KEY,
		applied_at TEXT NOT NULL)`); err != nil {
		return classify(err, "migrate")
	}
	var current int
	if err := s.db.QueryRowContext(ctx, "SELECT COALESCE(MAX(version), 0) FROM schema_migrations").Scan(&current); err != nil {
		return classify(err, "migrate")
	}
	latest := 0
	if len(ms) > 0 {
		latest = ms[len(ms)-1].version
	}
	if current > latest {
		return apperr.New(apperr.DBError, "database schema version %d is newer than this binary supports (%d); upgrade eklips", current, latest)
	}
	for _, m := range ms {
		if m.version <= current {
			continue
		}
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return classify(err, "migrate")
		}
		if _, err := tx.ExecContext(ctx, m.sql); err != nil {
			tx.Rollback()
			return classify(err, "apply migration "+m.name)
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO schema_migrations(version, applied_at) VALUES(?, ?)",
			m.version, time.Now().UTC().Format(timeFmt)); err != nil {
			tx.Rollback()
			return classify(err, "record migration "+m.name)
		}
		if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", m.version)); err != nil {
			tx.Rollback()
			return classify(err, "record migration "+m.name)
		}
		if err := tx.Commit(); err != nil {
			return classify(err, "commit migration "+m.name)
		}
	}
	return nil
}

// SchemaVersion returns the applied schema version.
func (s *Store) SchemaVersion() (int, error) {
	var v int
	err := s.db.QueryRow("SELECT COALESCE(MAX(version), 0) FROM schema_migrations").Scan(&v)
	return v, classify(err, "read schema version")
}

func (s *Store) prepare() error {
	var err error
	p := func(q string) *sql.Stmt {
		if err != nil {
			return nil
		}
		var st *sql.Stmt
		st, err = s.db.Prepare(q)
		return st
	}
	s.stmtGetSetting = p("SELECT value FROM settings WHERE key = ?")
	s.stmtSetSetting = p("INSERT INTO settings(key, value) VALUES(?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value")
	s.stmtSetAlias = p(`INSERT INTO aliases(name, playlist_path, created_at, updated_at) VALUES(?, ?, ?, ?)
		ON CONFLICT(name) DO UPDATE SET playlist_path = excluded.playlist_path, updated_at = excluded.updated_at`)
	s.stmtGetAlias = p("SELECT name, playlist_path, created_at, updated_at FROM aliases WHERE name = ?")
	s.stmtDelAlias = p("DELETE FROM aliases WHERE name = ?")
	s.stmtListAliases = p("SELECT name, playlist_path, created_at, updated_at FROM aliases ORDER BY name")
	s.stmtSavePlay = p(`INSERT INTO playback_state(id, source_type, source_path, current_index, position_ms, state, volume, muted, shuffle, loop, updated_at)
		VALUES(1, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET source_type = excluded.source_type, source_path = excluded.source_path,
		current_index = excluded.current_index, position_ms = excluded.position_ms, state = excluded.state,
		volume = excluded.volume, muted = excluded.muted, shuffle = excluded.shuffle, loop = excluded.loop,
		updated_at = excluded.updated_at`)
	s.stmtLoadPlay = p(`SELECT source_type, source_path, current_index, position_ms, state, volume, muted, shuffle, loop, updated_at
		FROM playback_state WHERE id = 1`)
	if err != nil {
		return classify(err, "prepare statements")
	}
	return nil
}

// Close closes the database.
func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

// Path returns the absolute database path.
func (s *Store) Path() string { return s.path }

// Writes returns the number of write statements executed (for tests and
// benchmarks verifying the write budget).
func (s *Store) Writes() int64 { return s.writes.Load() }

// GetSetting returns a setting value.
func (s *Store) GetSetting(key string) (string, bool, error) {
	var v string
	err := s.stmtGetSetting.QueryRow(key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, classify(err, "read setting")
	}
	return v, true, nil
}

// SetSetting stores a setting value.
func (s *Store) SetSetting(key, value string) error {
	s.writes.Add(1)
	_, err := s.stmtSetSetting.Exec(key, value)
	return classify(err, "write setting")
}

// ValidAliasName reports whether name is an acceptable alias.
func ValidAliasName(name string) bool {
	if name == "" || len(name) > 64 {
		return false
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
		default:
			return false
		}
	}
	return name != "." && name != ".."
}

// SetAlias creates or updates an alias.
func (s *Store) SetAlias(name, playlistPath string) (Alias, error) {
	if !ValidAliasName(name) {
		return Alias{}, apperr.New(apperr.InvalidCommand, "invalid alias name %q (use letters, digits, '-', '_', '.')", name)
	}
	now := time.Now().UTC()
	s.writes.Add(1)
	if _, err := s.stmtSetAlias.Exec(name, playlistPath, now.Format(timeFmt), now.Format(timeFmt)); err != nil {
		return Alias{}, classify(err, "save alias")
	}
	return s.GetAlias(name)
}

func scanAlias(sc interface{ Scan(...any) error }) (Alias, error) {
	var a Alias
	var c, u string
	if err := sc.Scan(&a.Name, &a.PlaylistPath, &c, &u); err != nil {
		return a, err
	}
	a.CreatedAt, _ = time.Parse(timeFmt, c)
	a.UpdatedAt, _ = time.Parse(timeFmt, u)
	return a, nil
}

// GetAlias returns an alias or ALIAS_NOT_FOUND.
func (s *Store) GetAlias(name string) (Alias, error) {
	a, err := scanAlias(s.stmtGetAlias.QueryRow(name))
	if errors.Is(err, sql.ErrNoRows) {
		return Alias{}, apperr.New(apperr.AliasNotFound, "alias %q is not defined", name)
	}
	if err != nil {
		return Alias{}, classify(err, "read alias")
	}
	return a, nil
}

// ListAliases returns all aliases sorted by name.
func (s *Store) ListAliases() ([]Alias, error) {
	rows, err := s.stmtListAliases.Query()
	if err != nil {
		return nil, classify(err, "list aliases")
	}
	defer rows.Close()
	out := []Alias{}
	for rows.Next() {
		a, err := scanAlias(rows)
		if err != nil {
			return nil, classify(err, "list aliases")
		}
		out = append(out, a)
	}
	return out, classify(rows.Err(), "list aliases")
}

// RemoveAlias deletes an alias or returns ALIAS_NOT_FOUND.
func (s *Store) RemoveAlias(name string) error {
	s.writes.Add(1)
	res, err := s.stmtDelAlias.Exec(name)
	if err != nil {
		return classify(err, "remove alias")
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return apperr.New(apperr.AliasNotFound, "alias %q is not defined", name)
	}
	return nil
}

// SavePlaylist imports a playlist (upsert by source path) and replaces its
// tracks in a single transaction.
func (s *Store) SavePlaylist(name, sourcePath string, tracks []string) (int64, error) {
	ctx := context.Background()
	s.writes.Add(1)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, classify(err, "save playlist")
	}
	defer tx.Rollback()
	now := time.Now().UTC().Format(timeFmt)
	var id int64
	err = tx.QueryRowContext(ctx, `INSERT INTO playlists(name, source_path, created_at, updated_at) VALUES(?, ?, ?, ?)
		ON CONFLICT(source_path) DO UPDATE SET name = excluded.name, updated_at = excluded.updated_at
		RETURNING id`, name, sourcePath, now, now).Scan(&id)
	if err != nil {
		return 0, classify(err, "save playlist")
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM playlist_tracks WHERE playlist_id = ?", id); err != nil {
		return 0, classify(err, "save playlist")
	}
	ins, err := tx.PrepareContext(ctx, "INSERT INTO playlist_tracks(playlist_id, position, path) VALUES(?, ?, ?)")
	if err != nil {
		return 0, classify(err, "save playlist")
	}
	defer ins.Close()
	for i, t := range tracks {
		if _, err := ins.ExecContext(ctx, id, i, t); err != nil {
			return 0, classify(err, "save playlist")
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, classify(err, "save playlist")
	}
	return id, nil
}

// PlaylistTracks returns the stored tracks for a playlist source path.
func (s *Store) PlaylistTracks(sourcePath string) (name string, tracks []string, err error) {
	var id int64
	err = s.db.QueryRow("SELECT id, name FROM playlists WHERE source_path = ?", sourcePath).Scan(&id, &name)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil, nil
	}
	if err != nil {
		return "", nil, classify(err, "read playlist")
	}
	rows, err := s.db.Query("SELECT path FROM playlist_tracks WHERE playlist_id = ? ORDER BY position", id)
	if err != nil {
		return "", nil, classify(err, "read playlist")
	}
	defer rows.Close()
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return "", nil, classify(err, "read playlist")
		}
		tracks = append(tracks, p)
	}
	return name, tracks, classify(rows.Err(), "read playlist")
}

// SavePlayback writes the playback state row.
func (s *Store) SavePlayback(p Playback) error {
	if p.UpdatedAt.IsZero() {
		p.UpdatedAt = time.Now()
	}
	s.writes.Add(1)
	_, err := s.stmtSavePlay.Exec(p.SourceType, p.SourcePath, p.CurrentIndex, p.PositionMs, p.State,
		p.Volume, b2i(p.Muted), b2i(p.Shuffle), b2i(p.Loop), p.UpdatedAt.UTC().Format(timeFmt))
	return classify(err, "save playback state")
}

// LoadPlayback reads the playback state row.
func (s *Store) LoadPlayback() (Playback, bool, error) {
	var p Playback
	var muted, shuffle, loop int
	var u string
	err := s.stmtLoadPlay.QueryRow().Scan(&p.SourceType, &p.SourcePath, &p.CurrentIndex, &p.PositionMs,
		&p.State, &p.Volume, &muted, &shuffle, &loop, &u)
	if errors.Is(err, sql.ErrNoRows) {
		return Playback{}, false, nil
	}
	if err != nil {
		return Playback{}, false, classify(err, "load playback state")
	}
	p.Muted, p.Shuffle, p.Loop = muted != 0, shuffle != 0, loop != 0
	p.UpdatedAt, _ = time.Parse(timeFmt, u)
	return p, true, nil
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}
