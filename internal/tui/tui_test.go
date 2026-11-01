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

	// Send '+' (vol up to 85), 'm' (mute), 'l' (loop), 'r' (shuffle), ' ' (pause), then 'q' (quit TUI).
	keys := []string{"+", "m", "l", "r", " "}
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

var _ tea.Model = (*Model)(nil)
