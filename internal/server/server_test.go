package server_test

import (
	"context"
	"io"
	"net/http"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/frdn/eklips/internal/apperr"
	"github.com/frdn/eklips/internal/audio"
	"github.com/frdn/eklips/internal/client"
	"github.com/frdn/eklips/internal/core"
	"github.com/frdn/eklips/internal/engine"
	"github.com/frdn/eklips/internal/events"
	"github.com/frdn/eklips/internal/server"
	"github.com/frdn/eklips/internal/sink"
	"github.com/frdn/eklips/internal/store"
)

func setupTestServer(t *testing.T, authToken string) (*server.Server, *client.Client, string) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "eklips.db"))
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
		Addr:              "127.0.0.1:0",
		AuthToken:         authToken,
		HeartbeatInterval: 40 * time.Millisecond,
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

	baseURL := "http://" + ln.Addr().String()
	cl, err := client.New(baseURL, authToken)
	if err != nil {
		t.Fatal(err)
	}
	return srv, cl, dir
}

func TestHTTPControlsPlaylistsAliasesAndErrors(t *testing.T) {
	_, cl, dir := setupTestServer(t, "s3cr3t-token")
	ctx := context.Background()

	// Health is unauthenticated; other endpoints require bearer token.
	unauthClient, _ := client.New(cl.BaseURL(), "")
	if err := unauthClient.Health(ctx); err != nil {
		t.Fatalf("unauthenticated Health failed: %v", err)
	}
	if _, err := unauthClient.Status(ctx); !apperr.Is(err, apperr.AuthRequired) {
		t.Fatalf("unauthenticated Status: got %v, want AUTH_REQUIRED", err)
	}
	wrongClient, _ := client.New(cl.BaseURL(), "wrong-token")
	if _, err := wrongClient.Status(ctx); !apperr.Is(err, apperr.Unauthorized) {
		t.Fatalf("wrong token Status: got %v, want UNAUTHORIZED", err)
	}

	// Unloaded play fails with NO_MEDIA_LOADED (409).
	if _, err := cl.Play(ctx, "", false); !apperr.Is(err, apperr.NoMediaLoaded) {
		t.Fatalf("Play unloaded: got %v, want NO_MEDIA_LOADED", err)
	}

	// Create 2 test WAV files in a music folder.
	musicDir := filepath.Join(dir, "music")
	_ = audio.WriteSineWAV(filepath.Join(dir, "single.wav"), 44100, 44100, 440)
	_ = audio.WriteSineWAV(filepath.Join(dir, "t1.wav"), 44100, 44100, 440)
	_ = audio.WriteSineWAV(filepath.Join(dir, "t2.wav"), 44100, 44100, 880)
	_ = musicDir

	// Play single file -> Next fails with PLAYLIST_REQUIRED.
	st, err := cl.Play(ctx, filepath.Join(dir, "single.wav"), false)
	if err != nil || st.State != core.Playing || st.SourceType != core.Single {
		t.Fatalf("Play single: st=%+v err=%v", st, err)
	}
	if _, err := cl.Next(ctx); !apperr.Is(err, apperr.PlaylistRequired) {
		t.Fatalf("Next on single: got %v, want PLAYLIST_REQUIRED", err)
	}

	// Create a playlist from dir, set an alias, and play by alias.
	plOut := filepath.Join(dir, "playlist.json")
	created, err := cl.PlaylistCreate(ctx, dir, plOut, false)
	if err != nil || created != plOut {
		t.Fatalf("PlaylistCreate: path=%q err=%v", created, err)
	}
	alias, err := cl.AliasSet(ctx, "favs", plOut)
	if err != nil || alias.Name != "favs" {
		t.Fatalf("AliasSet: %+v err=%v", alias, err)
	}
	aliases, err := cl.AliasList(ctx)
	if err != nil || len(aliases) != 1 {
		t.Fatalf("AliasList: %+v err=%v", aliases, err)
	}

	st, err = cl.PlaylistPlay(ctx, "favs", false)
	if err != nil || st.SourceType != core.Playlist || st.State != core.Playing {
		t.Fatalf("PlaylistPlay(favs): st=%+v err=%v", st, err)
	}

	q, err := cl.Queue(ctx)
	if err != nil || len(q.Tracks) != 3 {
		t.Fatalf("Queue: %+v err=%v", q, err)
	}

	// Next, Previous, Shuffle, Loop, Volume, Mute, Pause, Stop, Resume.
	if st, err = cl.Next(ctx); err != nil || st.TrackIndex != 1 {
		t.Fatalf("Next: idx=%d err=%v", st.TrackIndex, err)
	}
	if st, err = cl.Previous(ctx); err != nil || st.TrackIndex != 0 {
		t.Fatalf("Previous: idx=%d err=%v", st.TrackIndex, err)
	}
	if st, err = cl.Shuffle(ctx); err != nil || !st.Shuffle {
		t.Fatalf("Shuffle: %+v err=%v", st, err)
	}
	if st, err = cl.Loop(ctx); err != nil || !st.Loop {
		t.Fatalf("Loop: %+v err=%v", st, err)
	}
	if st, err = cl.VolumeUp(ctx); err != nil || st.Volume != 85 {
		t.Fatalf("VolumeUp: vol=%d err=%v", st.Volume, err)
	}
	if st, err = cl.VolumeDown(ctx); err != nil || st.Volume != 80 {
		t.Fatalf("VolumeDown: vol=%d err=%v", st.Volume, err)
	}
	if st, err = cl.Mute(ctx); err != nil || !st.Muted {
		t.Fatalf("Mute: %+v err=%v", st, err)
	}
	if st, err = cl.Pause(ctx); err != nil || st.State != core.Paused {
		t.Fatalf("Pause: %+v err=%v", st, err)
	}
	if st, err = cl.Stop(ctx); err != nil || st.State != core.Stopped {
		t.Fatalf("Stop: %+v err=%v", st, err)
	}
	if st, err = cl.Play(ctx, "", false); err != nil || st.State != core.Playing {
		t.Fatalf("Play resume after stop: %+v err=%v", st, err)
	}

	if err := cl.AliasRemove(ctx, "favs"); err != nil {
		t.Fatalf("AliasRemove: %v", err)
	}
}

func TestSSESnapshotIncrementalHeartbeatAndReconnect(t *testing.T) {
	_, cl, dir := setupTestServer(t, "")
	wav := filepath.Join(dir, "a.wav")
	_ = audio.WriteSineWAV(wav, 44100, 44100*2, 440)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	evCh, err := cl.SubscribeEvents(ctx, 0)
	if err != nil {
		t.Fatalf("SubscribeEvents: %v", err)
	}

	// First event on connect must be a snapshot.
	first := <-evCh
	if first.Type != events.Snapshot {
		t.Fatalf("first SSE event = %s, want snapshot", first.Type)
	}

	if _, err := cl.Play(context.Background(), wav, false); err != nil {
		t.Fatal(err)
	}
	if _, err := cl.VolumeUp(context.Background()); err != nil {
		t.Fatal(err)
	}

	var lastID uint64
	sawVol := false
	deadline := time.After(time.Second)
	for !sawVol {
		select {
		case ev := <-evCh:
			if ev.ID > 0 {
				lastID = ev.ID
			}
			if ev.Type == events.Volume {
				sawVol = true
			}
		case <-deadline:
			t.Fatal("timed out waiting for volume SSE event")
		}
	}
	cancel()

	// Trigger another change while disconnected, then reconnect with Last-Event-ID.
	if _, err := cl.Mute(context.Background()); err != nil {
		t.Fatal(err)
	}

	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	evCh2, err := cl.SubscribeEvents(ctx2, lastID)
	if err != nil {
		t.Fatalf("reconnect SubscribeEvents: %v", err)
	}
	replayed := <-evCh2
	if replayed.Type != events.Mute {
		t.Fatalf("replayed event = %s, want mute", replayed.Type)
	}
}

func TestHTTPSeekQueueBrowserAndLevels(t *testing.T) {
	_, cl, dir := setupTestServer(t, "")
	ctx := context.Background()
	for i, name := range []string{"01_a.wav", "02_b.wav", "03_c.wav"} {
		if err := audio.WriteSineWAV(filepath.Join(dir, name), 44100, 44100*2, 220+float64(i)*110); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := cl.Play(ctx, dir, false); err != nil {
		t.Fatal(err)
	}
	if _, err := cl.Pause(ctx); err != nil {
		t.Fatal(err)
	}
	st, err := cl.SeekTo(ctx, 1500)
	if err != nil {
		t.Fatal(err)
	}
	if st.PositionMs < 1400 || st.PositionMs > 1600 {
		t.Fatalf("SeekTo: position=%d", st.PositionMs)
	}
	st, err = cl.SeekBy(ctx, -1000)
	if err != nil {
		t.Fatal(err)
	}
	if st.PositionMs < 400 || st.PositionMs > 600 {
		t.Fatalf("Seek: position=%d", st.PositionMs)
	}

	// Queue index selection and reordering.
	st, err = cl.PlayIndex(ctx, 2)
	if err != nil || st.TrackIndex != 2 {
		t.Fatalf("PlayIndex: st=%+v err=%v", st, err)
	}
	st, err = cl.QueueMove(ctx, 2, 0)
	if err != nil {
		t.Fatal(err)
	}
	if st.TrackIndex != 0 {
		t.Fatalf("QueueMove index=%d", st.TrackIndex)
	}
	q, err := cl.Queue(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(q.Tracks) != 3 || !q.Tracks[0].Current {
		t.Fatalf("unexpected queue after move: %+v", q)
	}
	if _, err := cl.QueueRemove(ctx, 2); err != nil {
		t.Fatal(err)
	}
	if q, _ := cl.Queue(ctx); len(q.Tracks) != 2 {
		t.Fatalf("after remove: %d tracks", len(q.Tracks))
	}
	if _, err := cl.QueueAppend(ctx, []string{filepath.Join(dir, "01_a.wav")}); err != nil {
		t.Fatal(err)
	}
	if q, _ := cl.Queue(ctx); len(q.Tracks) != 3 {
		t.Fatalf("after append: %d tracks", len(q.Tracks))
	}

	// File browser listing.
	res, err := cl.Browse(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	if res.Path != dir || len(res.Entries) < 3 {
		t.Fatalf("browse: %+v", res)
	}
	// Levels endpoint always answers.
	if _, err := cl.Levels(ctx); err != nil {
		t.Fatalf("Levels: %v", err)
	}
	// The directory played above is persisted as a saved playlist.
	list, err := cl.PlaylistList(ctx)
	if err != nil || len(list) != 1 {
		t.Fatalf("PlaylistList: list=%v err=%v", list, err)
	}
	view, err := cl.PlaylistInspect(ctx, list[0].SourcePath)
	if err != nil || len(view.Tracks) != 3 {
		t.Fatalf("PlaylistInspect: view=%+v err=%v", view, err)
	}
	if err := cl.PlaylistRemove(ctx, list[0].SourcePath); err != nil {
		t.Fatalf("PlaylistRemove: %v", err)
	}
	if list, _ := cl.PlaylistList(ctx); len(list) != 0 {
		t.Fatalf("playlist not removed: %v", list)
	}
	if st, err := cl.QueueClear(ctx); err != nil || len(st.CurrentTrack) != 0 {
		t.Fatalf("QueueClear: st=%+v err=%v", st, err)
	}
}

func TestHTTPStream10ConcurrentListenersAt1x(t *testing.T) {
	_, cl, dir := setupTestServer(t, "")
	wav := filepath.Join(dir, "stream.wav")
	if err := audio.WriteSineWAV(wav, 44100, 44100*2, 440); err != nil {
		t.Fatal(err)
	}
	if _, err := cl.Play(context.Background(), wav, true); err != nil {
		t.Fatal(err)
	}

	const listeners = 10
	const readBytes = 44 + 882*4*5 // WAV header + 5 chunks (~100 ms of 1x audio)
	var wg sync.WaitGroup
	errs := make(chan error, listeners)

	for i := 0; i < listeners; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, err := http.Get(cl.BaseURL() + "/api/v1/stream")
			if err != nil {
				errs <- err
				return
			}
			defer resp.Body.Close()
			if ct := resp.Header.Get("Content-Type"); ct != "audio/wav" {
				errs <- apperr.New(apperr.Internal, "unexpected Content-Type %q", ct)
				return
			}
			buf := make([]byte, readBytes)
			if _, err := io.ReadFull(resp.Body, buf); err != nil {
				errs <- err
				return
			}
			if string(buf[0:4]) != "RIFF" || string(buf[8:12]) != "WAVE" {
				errs <- apperr.New(apperr.Internal, "missing RIFF/WAVE header")
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent stream listener failed: %v", err)
	}
}
