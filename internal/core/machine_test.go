package core

import (
	"testing"

	"github.com/frdn/eklips/internal/apperr"
)

func TestMachineUnloadedErrors(t *testing.T) {
	m := NewMachine(42)
	if m.Loaded() {
		t.Fatal("expected machine to start unloaded")
	}
	if m.State() != Stopped {
		t.Fatalf("expected Stopped, got %s", m.State())
	}

	ops := []struct {
		name string
		fn   func() (Effect, error)
	}{
		{"Play", m.Play},
		{"Pause", m.Pause},
		{"Stop", m.Stop},
		{"Next", m.Next},
		{"Previous", m.Previous},
		{"VolumeUp", m.VolumeUp},
		{"VolumeDown", m.VolumeDown},
		{"ToggleMute", m.ToggleMute},
		{"ToggleShuffle", m.ToggleShuffle},
		{"ToggleLoop", m.ToggleLoop},
	}
	for _, op := range ops {
		if _, err := op.fn(); !apperr.Is(err, apperr.NoMediaLoaded) {
			t.Errorf("%s on unloaded machine: got err=%v, want NO_MEDIA_LOADED", op.name, err)
		}
	}
}

func TestSingleFileTransitionsAndErrors(t *testing.T) {
	m := NewMachine(42)
	eff, err := m.Load(Source{
		Type:   Single,
		Path:   "/music/song.mp3",
		Tracks: []string{"/music/song.mp3"},
	}, false)
	if err != nil || eff != EffectStart {
		t.Fatalf("Load: eff=%v err=%v", eff, err)
	}
	if m.State() != Playing || m.CurrentTrack() != "/music/song.mp3" {
		t.Fatalf("unexpected state after Load: %+v", m.Snapshot())
	}

	// Play when already playing is idempotent.
	if eff, err := m.Play(); err != nil || eff != EffectNone {
		t.Fatalf("Play while playing: eff=%v err=%v", eff, err)
	}

	// Playlist-only commands fail with PLAYLIST_REQUIRED on single-file source.
	for name, fn := range map[string]func() (Effect, error){
		"Next":          m.Next,
		"Previous":      m.Previous,
		"ToggleShuffle": m.ToggleShuffle,
		"ToggleLoop":    m.ToggleLoop,
	} {
		if _, err := fn(); !apperr.Is(err, apperr.PlaylistRequired) {
			t.Errorf("%s on single source: got %v, want PLAYLIST_REQUIRED", name, err)
		}
	}

	// Pause -> Paused; second Pause -> INVALID_STATE.
	if eff, err := m.Pause(); err != nil || eff != EffectPause || m.State() != Paused {
		t.Fatalf("Pause: eff=%v err=%v state=%s", eff, err, m.State())
	}
	if _, err := m.Pause(); !apperr.Is(err, apperr.InvalidState) {
		t.Fatalf("second Pause: got %v, want INVALID_STATE", err)
	}

	// Play from Paused -> EffectResume.
	if eff, err := m.Play(); err != nil || eff != EffectResume || m.State() != Playing {
		t.Fatalf("Play from Paused: eff=%v err=%v state=%s", eff, err, m.State())
	}

	// Stop -> Stopped; queue retained; second Stop -> INVALID_STATE.
	if eff, err := m.Stop(); err != nil || eff != EffectStop || m.State() != Stopped {
		t.Fatalf("Stop: eff=%v err=%v state=%s", eff, err, m.State())
	}
	if !m.Loaded() {
		t.Fatal("expected source to remain loaded after Stop")
	}
	if _, err := m.Stop(); !apperr.Is(err, apperr.InvalidState) {
		t.Fatalf("second Stop: got %v, want INVALID_STATE", err)
	}
	if _, err := m.Pause(); !apperr.Is(err, apperr.InvalidState) {
		t.Fatalf("Pause while Stopped: got %v, want INVALID_STATE", err)
	}

	// Play from Stopped -> EffectStart.
	if eff, err := m.Play(); err != nil || eff != EffectStart || m.State() != Playing {
		t.Fatalf("Play from Stopped: eff=%v err=%v state=%s", eff, err, m.State())
	}

	// Natural end of single file without loop stops playback.
	if m.Advance() {
		t.Fatal("expected Advance at end of non-looped single file to return false")
	}
	if m.State() != Stopped {
		t.Fatalf("expected Stopped after end of single file, got %s", m.State())
	}
}

func TestSelectRemoveMoveClearAndAppend(t *testing.T) {
	m := NewMachine(1)
	src := Source{
		Type:   Playlist,
		Path:   "/music",
		Name:   "music",
		Tracks: []string{"/a", "/b", "/c", "/d"},
	}
	if _, err := m.Load(src, false); err != nil {
		t.Fatal(err)
	}
	if eff, err := m.SelectIndex(2); err != nil || eff != EffectStart {
		t.Fatalf("SelectIndex: eff=%v err=%v", eff, err)
	}
	if m.TrackIndex() != 2 || m.CurrentTrack() != "/c" {
		t.Fatalf("after select: index=%d track=%s", m.TrackIndex(), m.CurrentTrack())
	}
	// Removing a non-current track keeps the current selection.
	if eff, err := m.RemoveTrack(0); err != nil || eff != EffectNone {
		t.Fatalf("RemoveTrack non-current: eff=%v err=%v", eff, err)
	}
	if m.TrackIndex() != 1 || m.CurrentTrack() != "/c" {
		t.Fatalf("after remove non-current: index=%d track=%s", m.TrackIndex(), m.CurrentTrack())
	}
	// Moving the current track preserves its identity.
	if _, err := m.MoveTrack(1, 2); err != nil {
		t.Fatal(err)
	}
	if m.CurrentTrack() != "/c" {
		t.Fatalf("after move: current=%s tracks=%v", m.CurrentTrack(), m.Snapshot().Source.Tracks)
	}
	tracks := m.Snapshot().Source.Tracks
	if len(tracks) != 3 || tracks[2] != "/c" {
		t.Fatalf("unexpected order after move: %v", tracks)
	}
	// Removing the current track advances to the next.
	if eff, err := m.RemoveTrack(m.TrackIndex()); err != nil || eff != EffectStart {
		t.Fatalf("RemoveTrack current: eff=%v err=%v", eff, err)
	}
	if m.TrackIndex() < 0 || m.TrackIndex() >= len(m.Snapshot().Source.Tracks) {
		t.Fatalf("bad index after removing current: %d", m.TrackIndex())
	}
	// Append converts to a playlist queue and adds tracks.
	if _, err := m.AppendTracks([]string{"/e", "/f"}); err != nil {
		t.Fatal(err)
	}
	if got := len(m.Snapshot().Source.Tracks); got != 4 {
		t.Fatalf("after append: %d tracks", got)
	}
	// Clear empties and stops.
	if eff := m.Clear(); eff != EffectStop {
		t.Fatalf("Clear effect: %v", eff)
	}
	if m.Loaded() || m.State() != Stopped {
		t.Fatalf("expected empty stopped machine, loaded=%v state=%s", m.Loaded(), m.State())
	}
}

func TestAppendToEmptyQueueStartsPlaying(t *testing.T) {
	m := NewMachine(1)
	eff, err := m.AppendTracks([]string{"/a", "/b"})
	if err != nil || eff != EffectStart {
		t.Fatalf("AppendTracks: eff=%v err=%v", eff, err)
	}
	if m.State() != Playing || m.CurrentTrack() != "/a" || len(m.Snapshot().Source.Tracks) != 2 {
		t.Fatalf("state=%s current=%s tracks=%d", m.State(), m.CurrentTrack(), len(m.Snapshot().Source.Tracks))
	}
}

func TestSelectIndexOutOfRange(t *testing.T) {
	m := NewMachine(1)
	if _, err := m.SelectIndex(0); !apperr.Is(err, apperr.NoMediaLoaded) {
		t.Fatalf("got %v, want NO_MEDIA_LOADED", err)
	}
	if _, err := m.Load(Source{Type: Playlist, Tracks: []string{"/a"}}, false); err != nil {
		t.Fatal(err)
	}
	if _, err := m.SelectIndex(2); !apperr.Is(err, apperr.InvalidCommand) {
		t.Fatalf("got %v, want INVALID_COMMAND", err)
	}
}

func TestPlaylistNavigationShuffleLoopAndVolume(t *testing.T) {
	m := NewMachine(99)
	tracks := []string{"/a/1.wav", "/a/2.wav", "/a/3.wav", "/a/4.wav"}
	if _, err := m.Load(Source{
		Type:   Playlist,
		Path:   "/a",
		Tracks: tracks,
	}, false); err != nil {
		t.Fatal(err)
	}

	// Previous at track 0 without loop restarts track 0.
	if eff, err := m.Previous(); err != nil || eff != EffectStart || m.TrackIndex() != 0 {
		t.Fatalf("Previous at start: eff=%v err=%v idx=%d", eff, err, m.TrackIndex())
	}

	// Advance through tracks 0 -> 1 -> 2 -> 3.
	for want := 1; want < len(tracks); want++ {
		if _, err := m.Next(); err != nil || m.TrackIndex() != want {
			t.Fatalf("Next to %d: err=%v idx=%d", want, err, m.TrackIndex())
		}
	}
	// Next at end without loop -> INVALID_STATE.
	if _, err := m.Next(); !apperr.Is(err, apperr.InvalidState) {
		t.Fatalf("Next at end without loop: got %v, want INVALID_STATE", err)
	}

	// Enable loop -> Next wraps to 0, Previous from 0 wraps to last.
	if _, err := m.ToggleLoop(); err != nil || !m.Snapshot().Loop {
		t.Fatalf("ToggleLoop: %v", err)
	}
	if _, err := m.Next(); err != nil || m.TrackIndex() != 0 {
		t.Fatalf("Next with loop wrap: err=%v idx=%d", err, m.TrackIndex())
	}
	if _, err := m.Previous(); err != nil || m.TrackIndex() != 3 {
		t.Fatalf("Previous with loop wrap: err=%v idx=%d", err, m.TrackIndex())
	}

	// Shuffle keeps current track (index 3) at cursor 0 without restarting.
	genBefore := m.Gen()
	eff, err := m.ToggleShuffle()
	if err != nil || eff != EffectNone {
		t.Fatalf("ToggleShuffle: eff=%v err=%v", eff, err)
	}
	if m.TrackIndex() != 3 || m.Gen() == genBefore {
		t.Fatalf("expected current track 3 to stay active and gen to bump; idx=%d", m.TrackIndex())
	}
	// Disable shuffle -> restores identity order with cursor on current track.
	if _, err := m.ToggleShuffle(); err != nil || m.TrackIndex() != 3 {
		t.Fatalf("ToggleShuffle off: err=%v idx=%d", err, m.TrackIndex())
	}

	// Next/Previous when stopped -> INVALID_STATE.
	_, _ = m.Stop()
	if _, err := m.Next(); !apperr.Is(err, apperr.InvalidState) {
		t.Fatalf("Next when stopped: got %v, want INVALID_STATE", err)
	}

	// Volume bounds and mute retention.
	for m.Volume() < 100 {
		if _, err := m.VolumeUp(); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := m.VolumeUp(); !apperr.Is(err, apperr.VolumeMax) {
		t.Fatalf("VolumeUp at 100: got %v, want VOLUME_MAX", err)
	}
	if _, err := m.ToggleMute(); err != nil || !m.Muted() || m.Volume() != 100 {
		t.Fatalf("ToggleMute: err=%v muted=%v vol=%d", err, m.Muted(), m.Volume())
	}
	for m.Volume() > 0 {
		if _, err := m.VolumeDown(); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := m.VolumeDown(); !apperr.Is(err, apperr.VolumeMin) {
		t.Fatalf("VolumeDown at 0: got %v, want VOLUME_MIN", err)
	}
}
