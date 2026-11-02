package tui

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/creack/pty"

	"github.com/frdn/eklips/internal/apperr"
	"github.com/frdn/eklips/internal/audio"
	"github.com/frdn/eklips/internal/client"
	"github.com/frdn/eklips/internal/config"
	"github.com/frdn/eklips/internal/core"
	"github.com/frdn/eklips/internal/engine"
	"github.com/frdn/eklips/internal/events"
	"github.com/frdn/eklips/internal/server"
	"github.com/frdn/eklips/internal/sink"
	"github.com/frdn/eklips/internal/store"
)

func setupDaemonForTUI(t *testing.T, authToken string) (*server.Server, *client.Client, config.Config, string) {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "eklips.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	eng, err := engine.New(engine.Options{
		Store:      st,
		Sink:       sink.KindNull,
		SampleRate: 44100,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = eng.Close() })

	srv := server.New(eng, server.Options{
		Addr:      "127.0.0.1:0",
		AuthToken: authToken,
	})
	ln, err := srv.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	})

	port := ln.Addr().(*net.TCPAddr).Port
	cfg := config.Default()
	cfg.DBPath = dbPath
	cfg.Sink = sink.KindNull
	cfg.Bind = "127.0.0.1"
	cfg.Port = port
	cfg.AuthToken = authToken

	u, _ := cfg.BaseURL()
	cl, err := client.New(u, authToken)
	if err != nil {
		t.Fatal(err)
	}
	return srv, cl, cfg, dir
}

func TestNonTTYRejectedBeforeAutoStart(t *testing.T) {
	dir := t.TempDir()
	nonTTY, err := os.CreateTemp(dir, "nontty-*")
	if err != nil {
		t.Fatal(err)
	}
	defer nonTTY.Close()

	cfg := config.Default()
	cfg.DBPath = filepath.Join(dir, "should-not-be-created.db")
	cfg.Sink = sink.KindNull
	cfg.Port = 59999

	err = Run(context.Background(), cfg, nonTTY, nonTTY)
	if !apperr.Is(err, apperr.TUIRequiresTTY) {
		t.Fatalf("Run with non-TTY: got %v, want TUI_REQUIRES_TTY", err)
	}
	if _, statErr := os.Stat(cfg.DBPath); !os.IsNotExist(statErr) {
		t.Fatal("expected non-TTY rejection before any daemon or DB creation")
	}
}

func TestPTYEndToEndKeysAndDaemonRemainsAlive(t *testing.T) {
	_, cl, cfg, dir := setupDaemonForTUI(t, "secret-tui-token")
	cfg.Remote, _ = cfg.BaseURL()

	// Load a 2-track playlist so TUI keys can toggle pause/play, volume, mute, loop, shuffle, next.
	_ = audio.WriteSineWAV(filepath.Join(dir, "01_alpha.wav"), 44100, 44100*5, 440)
	_ = audio.WriteSineWAV(filepath.Join(dir, "02_beta.wav"), 44100, 44100*5, 880)
	if _, err := cl.Play(context.Background(), dir, false); err != nil {
		t.Fatal(err)
	}

	ptmx, tty, err := pty.Open()
	if err != nil {
		t.Fatalf("pty.Open: %v", err)
	}
	defer ptmx.Close()
	defer tty.Close()
	_ = pty.Setsize(ptmx, &pty.Winsize{Rows: 30, Cols: 100})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	tuiDone := make(chan error, 1)
	go func() {
		tuiDone <- Run(ctx, cfg, tty, tty)
	}()

	// Drain PTY output continuously so the terminal never blocks.
	go func() {
		buf := make([]byte, 4096)
		for {
			if _, err := ptmx.Read(buf); err != nil {
				return
			}
		}
	}()

	// Wait for initial TUI connection.
	time.Sleep(250 * time.Millisecond)

	// Send '+' (vol up to 85), 'm' (mute), 'L' (loop), 'r' (shuffle), ' ' (pause), then 'q' (quit TUI).
	keys := []string{"+", "m", "L", "r", " "}
	for _, k := range keys {
		if _, err := ptmx.Write([]byte(k)); err != nil {
			t.Fatal(err)
		}
		time.Sleep(120 * time.Millisecond)
	}

	// Verify state changes on the daemon over HTTP!
	st, err := cl.Status(context.Background())
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if st.Volume != 85 || !st.Muted || !st.Loop || !st.Shuffle || st.State != core.Paused {
		t.Fatalf("unexpected daemon state after PTY keys: %+v", st)
	}

	// Quit TUI with 'q'.
	_, _ = ptmx.Write([]byte("q"))
	select {
	case err := <-tuiDone:
		if err != nil {
			t.Fatalf("TUI Run returned error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for TUI to exit after 'q'")
	}

	// Confirm the daemon remains alive after TUI exits!
	if err := cl.Health(context.Background()); err != nil {
		t.Fatalf("expected daemon to remain alive after TUI exit, got %v", err)
	}
}

func TestPTYQueueNavigationViewTabsAndFilter(t *testing.T) {
	_, cl, cfg, dir := setupDaemonForTUI(t, "")
	cfg.Remote, _ = cfg.BaseURL()

	for i, name := range []string{"01_one.wav", "02_two.wav", "03_three.wav"} {
		if err := audio.WriteSineWAV(filepath.Join(dir, name), 44100, 44100*4, 220+float64(i)*110); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := cl.Play(context.Background(), dir, false); err != nil {
		t.Fatal(err)
	}
	if _, err := cl.Pause(context.Background()); err != nil {
		t.Fatal(err)
	}

	ptmx, tty, err := pty.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer ptmx.Close()
	defer tty.Close()
	_ = pty.Setsize(ptmx, &pty.Winsize{Rows: 32, Cols: 110})

	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- Run(ctx, cfg, tty, tty) }()
	go func() {
		buf := make([]byte, 4096)
		for {
			if _, err := ptmx.Read(buf); err != nil {
				return
			}
		}
	}()
	time.Sleep(300 * time.Millisecond)

	send := func(keys ...string) {
		for _, k := range keys {
			if _, err := ptmx.Write([]byte(k)); err != nil {
				t.Fatal(err)
			}
			time.Sleep(120 * time.Millisecond)
		}
	}
	// Move the cursor down and press Enter to play the second track.
	send("j", "\r")
	st, err := cl.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if st.TrackIndex != 1 {
		t.Fatalf("Enter did not select track 2: index=%d", st.TrackIndex)
	}
	// Open the filter, type a non-match, apply it, then clear it.
	send("/", "z", "z", "\r")
	send("/", "\x15", "\r")
	// Switch through all views and back.
	send("2", "3", "1")
	viewSt, err := cl.Status(context.Background())
	if err != nil || viewSt.TrackIndex != 1 {
		t.Fatalf("view switching disturbed playback: %+v err=%v", viewSt, err)
	}

	send("q")
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("TUI Run returned error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for TUI exit")
	}
	if err := cl.Health(context.Background()); err != nil {
		t.Fatalf("daemon died after TUI exit: %v", err)
	}
}

func TestNoStartAndRemoteNeverAutoStarts(t *testing.T) {
	ptmx, tty, err := pty.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer ptmx.Close()
	defer tty.Close()

	cfg := config.Default()
	cfg.DBPath = filepath.Join(t.TempDir(), "eklips.db")
	cfg.Sink = sink.KindNull
	cfg.Port = 59998
	cfg.NoStart = true

	if err := Run(context.Background(), cfg, tty, tty); !apperr.Is(err, apperr.ServiceNotRunning) {
		t.Fatalf("Run with --no-start and no daemon: got %v, want SERVICE_NOT_RUNNING", err)
	}

	cfg.NoStart = false
	cfg.Remote = "http://127.0.0.1:59998"
	if err := Run(context.Background(), cfg, tty, tty); !apperr.Is(err, apperr.ServiceNotRunning) {
		t.Fatalf("Run with --remote and no daemon: got %v, want SERVICE_NOT_RUNNING", err)
	}
}

func TestModelDisconnectReconnectAndViewCache(t *testing.T) {
	m := NewModel(context.Background(), nil)
	defer m.Close()

	// Simulate connect snapshot.
	m.Update(connectedMsg{
		status: engine.Status{
			State:        core.Playing,
			SourceType:   core.Playlist,
			SourcePath:   "/music",
			CurrentTrack: "/music/song.flac",
			TrackIndex:   0,
			PositionMs:   1000,
			DurationMs:   60000,
			Volume:       80,
		},
		queue: engine.Queue{
			Tracks: []engine.QueueItem{{Index: 0, Name: "song.flac", Current: true}},
		},
		events: make(chan client.SSEEvent),
	})
	v1 := m.View()
	if !strings.Contains(v1, "PLAYING") || !strings.Contains(v1, "song.flac") {
		t.Fatalf("unexpected View: %s", v1)
	}

	// Simulate daemon disconnect -> shows SERVICE_NOT_RUNNING without exiting.
	m.Update(disconnectedMsg{err: apperr.New(apperr.ServiceNotRunning, "daemon unreachable")})
	v2 := m.View()
	if !strings.Contains(v2, "DISCONNECTED") || !strings.Contains(v2, "SERVICE_NOT_RUNNING") {
		t.Fatalf("expected DISCONNECTED and SERVICE_NOT_RUNNING in View, got:\n%s", v2)
	}
}

func keyRunes(s string) tea.KeyMsg {
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)}
}

func connectedModel(t *testing.T) *Model {
	t.Helper()
	m := NewModel(context.Background(), nil)
	t.Cleanup(m.Close)
	m.Update(connectedMsg{
		status: engine.Status{
			State:        core.Playing,
			SourceType:   core.Playlist,
			SourcePath:   "/music",
			CurrentTrack: "/music/track01.flac",
			TrackIndex:   0,
			PositionMs:   15000,
			DurationMs:   180000,
			Volume:       80,
			Metadata:     audio.Metadata{Title: "First Track", Artist: "The Band", Album: "Album One"},
		},
		queue: engine.Queue{Tracks: []engine.QueueItem{
			{Index: 0, Name: "track01.flac", Current: true, Metadata: audio.Metadata{Title: "First Track", Artist: "The Band", Album: "Album One"}},
			{Index: 1, Name: "track02.flac", Metadata: audio.Metadata{Title: "Second Song", Artist: "Other Artist"}},
			{Index: 2, Name: "track03.flac", Metadata: audio.Metadata{Title: "Third Tune", Artist: "The Band"}},
		}},
		events: make(chan client.SSEEvent),
	})
	return m
}

func TestViewModelTabsMetadataAndMeter(t *testing.T) {
	m := connectedModel(t)
	view := m.View()
	for _, want := range []string{"1: Queue (3)", "2: Files", "3: Playlists", "Now Playing", "The Band — First Track (Album One)", "L: [", "R: [", "Vol: 80%"} {
		if !strings.Contains(view, want) {
			t.Fatalf("view missing %q:\n%s", want, view)
		}
	}
}

func TestQueueCursorNavigationFilterAndViews(t *testing.T) {
	m := connectedModel(t)
	if m.cursor != 0 {
		t.Fatalf("initial cursor = %d", m.cursor)
	}
	m.Update(keyRunes("j"))
	if m.cursor != 1 {
		t.Fatalf("after j cursor = %d", m.cursor)
	}
	m.Update(keyRunes("G"))
	if m.cursor != 2 {
		t.Fatalf("after G cursor = %d", m.cursor)
	}
	m.Update(keyRunes("g"))
	if m.cursor != 0 {
		t.Fatalf("after g cursor = %d", m.cursor)
	}

	// Inline fuzzy filter.
	m.Update(keyRunes("/"))
	m.Update(keyRunes("second"))
	m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if m.queueFilter != "second" {
		t.Fatalf("filter = %q", m.queueFilter)
	}
	idx := m.queueIndices()
	if len(idx) != 1 || idx[0] != 1 {
		t.Fatalf("filtered indices = %v", idx)
	}
	if m.cursor != 1 {
		t.Fatalf("cursor not clamped to filter: %d", m.cursor)
	}
	if !strings.Contains(m.View(), "filter: second") {
		t.Fatal("view missing filter indicator")
	}

	// Filter with no matches (clear the prefilled filter first).
	m.Update(keyRunes("/"))
	m.Update(tea.KeyMsg{Type: tea.KeyCtrlU})
	m.Update(keyRunes("zzzz"))
	m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if !strings.Contains(m.View(), "(no matches)") {
		t.Fatal("view missing no-matches message")
	}
	// Esc cancels while editing and restores the previous filter.
	m.Update(keyRunes("/"))
	m.Update(keyRunes("x"))
	m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if m.queueFilter != "zzzz" {
		t.Fatalf("esc did not restore filter: %q", m.queueFilter)
	}

	// View switching via numbers and Tab.
	m.Update(keyRunes("2"))
	if m.view != viewBrowser {
		t.Fatalf("view after 2 = %v", m.view)
	}
	m.Update(tea.KeyMsg{Type: tea.KeyTab})
	if m.view != viewPlaylists {
		t.Fatalf("view after tab = %v", m.view)
	}
	m.Update(keyRunes("1"))
	if m.view != viewQueue {
		t.Fatalf("view after 1 = %v", m.view)
	}
}

func TestBrowserAndPlaylistViews(t *testing.T) {
	m := connectedModel(t)
	m.view = viewBrowser
	m.browserLoaded = true
	m.browser = engine.BrowseResult{
		Path:   "/music/rock",
		Parent: "/music",
		Root:   "/music",
		Entries: []engine.BrowseEntry{
			{Name: "albums", Path: "/music/rock/albums", IsDir: true},
			{Name: "song.flac", Path: "/music/rock/song.flac", Audio: true, Size: 4096,
				Metadata: audio.Metadata{Title: "Song", Artist: "Artist", Album: "Record", Year: "2020"}},
		},
	}
	m.moveBrowserCursor(1)
	view := m.View()
	for _, want := range []string{"File Browser", "/music/rock", "albums/", "Artist — Song (Record)", "Size: 4.0 KB"} {
		if !strings.Contains(view, want) {
			t.Fatalf("browser view missing %q:\n%s", want, view)
		}
	}

	// Browser filter.
	m.openInput(inputBrowserFilter, "/")
	m.inputText = "song"
	m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if len(m.browserIndices()) != 1 || m.browser.Entries[m.browserIndices()[0]].Name != "song.flac" {
		t.Fatalf("browser filter failed: %v", m.browserIndices())
	}

	m.view = viewPlaylists
	m.playlistsLoaded = true
	m.playlists = []playlistRow{
		{Kind: "playlist", Name: "Road Trip", Target: "/music/roadtrip.json", TrackCount: 12},
		{Kind: "alias", Name: "chill", Target: "chill"},
	}
	m.preview = engine.PlaylistView{
		PlaylistInfo: engine.PlaylistInfo{Name: "Road Trip", SourcePath: "/music/roadtrip.json", TrackCount: 2},
		Tracks: []engine.QueueItem{
			{Index: 0, Name: "a.flac", Metadata: audio.Metadata{Title: "A"}},
			{Index: 1, Name: "b.flac", Metadata: audio.Metadata{Title: "B"}},
		},
	}
	view = m.View()
	for _, want := range []string{"Playlists & Aliases", "Road Trip", "12 tracks", "alias", "Road Trip", "1. A"} {
		if !strings.Contains(view, want) {
			t.Fatalf("playlist view missing %q:\n%s", want, view)
		}
	}
}

func TestUnchangedSSEEventKeepsRenderCache(t *testing.T) {
	m := connectedModel(t)
	_ = m.View()
	m.dirty = false
	m.Update(sseEventMsg{
		event:  client.SSEEvent{Type: events.Volume, Data: []byte(`{"volume":80,"muted":false}`)},
		events: make(chan client.SSEEvent),
	})
	if m.dirty {
		t.Fatal("unchanged volume event dirtied the render cache")
	}
	m.Update(sseEventMsg{
		event:  client.SSEEvent{Type: events.Volume, Data: []byte(`{"volume":55,"muted":false}`)},
		events: make(chan client.SSEEvent),
	})
	if !m.dirty || m.status.Volume != 55 {
		t.Fatalf("changed volume event not applied: dirty=%v volume=%d", m.dirty, m.status.Volume)
	}
}

func TestThemeSwitchingAndCommandParsing(t *testing.T) {
	m := connectedModel(t)
	if !m.SetTheme("nord") || m.Theme() != "nord" {
		t.Fatalf("SetTheme(nord): theme=%s", m.Theme())
	}
	if !strings.Contains(m.View(), "theme:nord") {
		t.Fatal("view missing theme name")
	}
	if m.SetTheme("does-not-exist") {
		t.Fatal("unknown theme accepted")
	}
	if m.Theme() != "nord" {
		t.Fatalf("unknown theme changed active theme to %s", m.Theme())
	}
	if m.executeCommand("theme gruvbox"); m.Theme() != "gruvbox" {
		t.Fatalf("command theme: %s", m.Theme())
	}
	if m.executeCommand("filter hello"); m.queueFilter != "hello" {
		t.Fatalf("command filter: %q", m.queueFilter)
	}

	cases := map[string]int64{
		"+5":    5000,
		"-30s":  -30000,
		"+1.5m": 90000,
	}
	for in, want := range cases {
		got, ok := parseSeekDelta(in)
		if !ok || got != want {
			t.Fatalf("parseSeekDelta(%q) = %d,%v want %d", in, got, ok, want)
		}
	}
	if ms, ok := parseSeekPosition("1:20"); !ok || ms != 80000 {
		t.Fatalf("parseSeekPosition(1:20) = %d,%v", ms, ok)
	}
	if ms, ok := parseSeekPosition("45"); !ok || ms != 45000 {
		t.Fatalf("parseSeekPosition(45) = %d,%v", ms, ok)
	}
}

func TestMeterBarAndFormatting(t *testing.T) {
	st := newStyles(paletteFor("default"))
	bar := meterBar(10, -6, st)
	if !strings.Contains(bar, "█") || !strings.Contains(bar, "░") {
		t.Fatalf("unexpected meter bar: %q", bar)
	}
	if formatMillis(0) != "00:00" || formatMillis(65000) != "01:05" || formatMillis(3661000) != "1:01:01" {
		t.Fatalf("formatMillis: %q %q %q", formatMillis(0), formatMillis(65000), formatMillis(3661000))
	}
	if formatBytes(4096) != "4.0 KB" {
		t.Fatalf("formatBytes: %q", formatBytes(4096))
	}
}

func BenchmarkTUIUpdateAndView(b *testing.B) {
	m := NewModel(context.Background(), nil)
	defer m.Close()
	m.connected = true
	m.status = engine.Status{
		State:        core.Playing,
		SourceType:   core.Playlist,
		SourcePath:   "/music",
		CurrentTrack: "/music/track01.flac",
		TrackIndex:   0,
		PositionMs:   15000,
		DurationMs:   180000,
		Volume:       80,
	}
	m.queue = engine.Queue{
		Tracks: []engine.QueueItem{
			{Index: 0, Name: "track01.flac", Current: true},
			{Index: 1, Name: "track02.flac"},
			{Index: 2, Name: "track03.flac"},
		},
	}
	evCh := make(chan client.SSEEvent, 1)
	ev := client.SSEEvent{
		ID:   1,
		Type: events.Volume,
		Data: []byte(`{"volume":85,"muted":false}`),
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		m.Update(sseEventMsg{event: ev, events: evCh})
		_ = m.View()
	}
}

func BenchmarkTUIFullRender(b *testing.B) {
	m := NewModel(context.Background(), nil)
	defer m.Close()
	m.connected = true
	m.status = engine.Status{
		State:        core.Playing,
		SourceType:   core.Playlist,
		SourcePath:   "/music",
		CurrentTrack: "/music/track01.flac",
		TrackIndex:   0,
		PositionMs:   15000,
		DurationMs:   180000,
		Volume:       80,
		Metadata:     audio.Metadata{Title: "Track One", Artist: "Artist", Album: "Album"},
	}
	m.queue = engine.Queue{
		Tracks: []engine.QueueItem{
			{Index: 0, Name: "track01.flac", Current: true, Metadata: audio.Metadata{Title: "Track One", Artist: "Artist"}},
			{Index: 1, Name: "track02.flac", Metadata: audio.Metadata{Title: "Track Two", Artist: "Artist"}},
			{Index: 2, Name: "track03.flac", Metadata: audio.Metadata{Title: "Track Three", Artist: "Artist"}},
		},
	}
	m.levels = engine.Levels{Left: -8, Right: -10, PeakL: 0.4, PeakR: 0.3}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		m.dirty = true
		_ = m.View()
	}
}

var _ tea.Model = (*Model)(nil)
