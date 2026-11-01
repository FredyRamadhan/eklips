package store

import (
	"database/sql"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/frdn/eklips/internal/apperr"
)

func TestOpenMigrateAliasesPlaylistsAndPlayback(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "eklips.db")
	s, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	v, err := s.SchemaVersion()
	if err != nil || v != LatestVersion() {
		t.Fatalf("SchemaVersion = %d, err = %v, want %d", v, err, LatestVersion())
	}

	// Settings CRUD
	if err := s.SetSetting("resume_on_start", "true"); err != nil {
		t.Fatal(err)
	}
	val, ok, err := s.GetSetting("resume_on_start")
	if err != nil || !ok || val != "true" {
		t.Fatalf("GetSetting = %q, ok=%v, err=%v", val, ok, err)
	}

	// Alias CRUD
	a, err := s.SetAlias("chill", "/music/chill.json")
	if err != nil || a.Name != "chill" || a.PlaylistPath != "/music/chill.json" {
		t.Fatalf("SetAlias: %+v, err=%v", a, err)
	}
	if _, err := s.SetAlias("bad/name", "/music/a.json"); !apperr.Is(err, apperr.InvalidCommand) {
		t.Fatalf("expected INVALID_COMMAND on bad alias name, got %v", err)
	}
	list, err := s.ListAliases()
	if err != nil || len(list) != 1 {
		t.Fatalf("ListAliases: %+v, err=%v", list, err)
	}
	if _, err := s.GetAlias("missing"); !apperr.Is(err, apperr.AliasNotFound) {
		t.Fatalf("GetAlias(missing): got %v, want ALIAS_NOT_FOUND", err)
	}

	// Playlist import/export
	tracks := []string{"/music/1.flac", "/music/2.flac"}
	if _, err := s.SavePlaylist("MyList", "/music/list.json", tracks); err != nil {
		t.Fatalf("SavePlaylist: %v", err)
	}
	name, gotTracks, err := s.PlaylistTracks("/music/list.json")
	if err != nil || name != "MyList" || len(gotTracks) != 2 {
		t.Fatalf("PlaylistTracks: name=%q tracks=%v err=%v", name, gotTracks, err)
	}

	// Playback state save & survive restart
	wantPB := Playback{
		SourceType:   "playlist",
		SourcePath:   "/music/list.json",
		CurrentIndex: 1,
		PositionMs:   12345,
		State:        "playing",
		Volume:       75,
		Muted:        true,
		Shuffle:      true,
		Loop:         true,
	}
	if err := s.SavePlayback(wantPB); err != nil {
		t.Fatalf("SavePlayback: %v", err)
	}
	_ = s.Close()

	// Reopen database and verify state survived restart.
	s2, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Reopen: %v", err)
	}
	defer s2.Close()

	gotAlias, err := s2.GetAlias("chill")
	if err != nil || gotAlias.PlaylistPath != "/music/chill.json" {
		t.Fatalf("GetAlias after restart: %+v, err=%v", gotAlias, err)
	}
	gotPB, ok, err := s2.LoadPlayback()
	if err != nil || !ok {
		t.Fatalf("LoadPlayback after restart: ok=%v err=%v", ok, err)
	}
	if gotPB.SourcePath != wantPB.SourcePath || gotPB.CurrentIndex != 1 || gotPB.Volume != 75 || !gotPB.Muted || !gotPB.Shuffle || !gotPB.Loop {
		t.Fatalf("LoadPlayback mismatch: %+v", gotPB)
	}
	if err := s2.RemoveAlias("chill"); err != nil {
		t.Fatalf("RemoveAlias: %v", err)
	}
	if err := s2.RemoveAlias("chill"); !apperr.Is(err, apperr.AliasNotFound) {
		t.Fatalf("second RemoveAlias: got %v, want ALIAS_NOT_FOUND", err)
	}
}

func TestCorruptAndNewerSchemaDatabaseErrors(t *testing.T) {
	dir := t.TempDir()
	corruptPath := filepath.Join(dir, "corrupt.db")
	if err := os.WriteFile(corruptPath, []byte("not a sqlite database file at all!"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(corruptPath); !apperr.Is(err, apperr.DBError) {
		t.Fatalf("Open corrupt DB: got %v, want DB_ERROR", err)
	}

	newerPath := filepath.Join(dir, "newer.db")
	s, err := Open(newerPath)
	if err != nil {
		t.Fatal(err)
	}
	_ = s.Close()

	rawDB, err := sql.Open("sqlite", "file:"+newerPath)
	if err != nil {
		t.Fatal(err)
	}
	_, err = rawDB.Exec("INSERT INTO schema_migrations(version, applied_at) VALUES(999, '2026-01-01T00:00:00Z')")
	_ = rawDB.Close()
	if err != nil {
		t.Fatal(err)
	}

	if _, err := Open(newerPath); !apperr.Is(err, apperr.DBError) {
		t.Fatalf("Open newer schema DB: got %v, want DB_ERROR", err)
	}
}

func TestConcurrentStoreAccess(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "concurrent.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for i := 0; i < 25; i++ {
				_ = s.SavePlayback(Playback{
					SourceType:   "single",
					SourcePath:   "/music/test.wav",
					CurrentIndex: 0,
					PositionMs:   int64(i * 100),
					State:        "playing",
					Volume:       80,
				})
				_, _, _ = s.LoadPlayback()
				_, _ = s.ListAliases()
			}
		}(g)
	}
	wg.Wait()
}

func BenchmarkStoreSavePlayback(b *testing.B) {
	s, err := Open(filepath.Join(b.TempDir(), "bench.db"))
	if err != nil {
		b.Fatal(err)
	}
	defer s.Close()
	pb := Playback{
		SourceType:   "playlist",
		SourcePath:   "/music/playlist.json",
		CurrentIndex: 2,
		PositionMs:   45000,
		State:        "playing",
		Volume:       80,
		UpdatedAt:    time.Now(),
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		pb.PositionMs = int64(i * 5000)
		if err := s.SavePlayback(pb); err != nil {
			b.Fatal(err)
		}
	}
}
