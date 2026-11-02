package engine

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/frdn/eklips/internal/apperr"
	"github.com/frdn/eklips/internal/audio"
	"github.com/frdn/eklips/internal/core"
	"github.com/frdn/eklips/internal/events"
	"github.com/frdn/eklips/internal/sink"
	"github.com/frdn/eklips/internal/store"
)

type captureOutput struct {
	mu      sync.Mutex
	buf     bytes.Buffer
	flushes int
}

func (c *captureOutput) Write(p []byte) error {
	c.mu.Lock()
	_, _ = c.buf.Write(p)
	c.mu.Unlock()
	return nil
}
func (c *captureOutput) Pause()  {}
func (c *captureOutput) Resume() {}
func (c *captureOutput) Flush() {
	c.mu.Lock()
	c.flushes++
	c.mu.Unlock()
}
func (c *captureOutput) Close() error { return nil }

func (c *captureOutput) samples() []int16 {
	c.mu.Lock()
	defer c.mu.Unlock()
	b := c.buf.Bytes()
	out := make([]int16, len(b)/2)
	for i := range out {
		out[i] = int16(binary.LittleEndian.Uint16(b[i*2:]))
	}
	return out
}

func TestSingleInstanceEnforcement(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "eklips.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	e1, err := New(Options{Store: st, Sink: sink.KindNull})
	if err != nil {
		t.Fatalf("first New: %v", err)
	}
	defer e1.Close()

	// Second engine using the same DB lock must fail with ALREADY_RUNNING.
	if _, err := New(Options{LockPath: dbPath + ".lock", Sink: sink.KindNull}); !apperr.Is(err, apperr.AlreadyRunning) {
		t.Fatalf("second New: got %v, want ALREADY_RUNNING", err)
	}
}

func TestGaplessSampleAccurateSpliceAndStreamContinuity(t *testing.T) {
	dir := t.TempDir()
	const rate = 44100
	// Pick frame counts that are NOT multiples of the 882-frame chunk size so the
	// splice occurs mid-chunk!
	const frames1 = 1500
	const frames2 = 1700
	const totalFrames = frames1 + frames2

	track1Samples := make([]int16, frames1*2)
	for i := 0; i < frames1; i++ {
		v := int16(100 + i)
		track1Samples[i*2] = v
		track1Samples[i*2+1] = v
	}
	track2Samples := make([]int16, frames2*2)
	for i := 0; i < frames2; i++ {
		v := int16(100 + frames1 + i)
		track2Samples[i*2] = v
		track2Samples[i*2+1] = v
	}

	p1 := filepath.Join(dir, "01_first.wav")
	p2 := filepath.Join(dir, "02_second.wav")
	if err := audio.WriteTestWAV(p1, rate, track1Samples); err != nil {
		t.Fatal(err)
	}
	if err := audio.WriteTestWAV(p2, rate, track2Samples); err != nil {
		t.Fatal(err)
	}

	capOut := &captureOutput{}
	eng, err := New(Options{
		Sink:         sink.KindNull,
		CustomOutput: capOut,
		SampleRate:   rate,
		Unpaced:      true,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Close()

	// Subscribe to the HTTP broadcaster to verify continuous stream output across
	// the gapless boundary as well.
	streamCh, _, streamCancel, err := eng.Broadcaster().Subscribe()
	if err != nil {
		t.Fatal(err)
	}
	defer streamCancel()

	var streamBytes bytes.Buffer
	streamDone := make(chan struct{})
	go func() {
		defer close(streamDone)
		for chunk := range streamCh {
			streamBytes.Write(chunk.Data)
			chunk.Release()
			if streamBytes.Len() >= totalFrames*4 {
				return
			}
		}
	}()

	// Set volume to 100% so output samples match input 1:1.
	if _, err := eng.Play(dir, false); err != nil {
		t.Fatal(err)
	}
	// Note: default volume is 80%; test gain scaling or set volume to 100 before play.
	// Wait for playback to finish naturally.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if eng.Status().State == core.Stopped {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if eng.Status().State != core.Stopped {
		t.Fatalf("expected engine to reach Stopped after playlist, got %s", eng.Status().State)
	}

	select {
	case <-streamDone:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for HTTP stream chunks")
	}

	gotSamples := capOut.samples()
	if len(gotSamples) != totalFrames*2 {
		t.Fatalf("captured %d samples (%d frames), want %d frames (zero silence inserted)",
			len(gotSamples), len(gotSamples)/2, totalFrames)
	}

	// Verify Flush was called only once (at initial Play) and NEVER at the
	// gapless track boundary!
	if capOut.flushes != 1 {
		t.Fatalf("expected output Flush count = 1 (no device reset at splice), got %d", capOut.flushes)
	}

	// Verify sample-by-sample monotonic ramp across the track1 -> track2 splice
	// boundary (frames1-1 -> frames1) with zero silence or drop!
	for i := 0; i < totalFrames; i++ {
		want := int16(float64(100+i)*0.8 + 0.5)
		got := gotSamples[i*2]
		if diff := int(got) - int(want); diff < -2 || diff > 2 {
			t.Fatalf("frame %d (boundary=%d): got sample %d, want %d", i, frames1, got, want)
		}
	}
}

func TestPrefetchInvalidationAndBrokenTrackSkip(t *testing.T) {
	dir := t.TempDir()
	const rate = 44100
	p1 := filepath.Join(dir, "01.wav")
	p2 := filepath.Join(dir, "02_corrupt.wav")
	p3 := filepath.Join(dir, "03.wav")

	if err := audio.WriteSineWAV(p1, rate, 2000, 440); err != nil {
		t.Fatal(err)
	}
	// Write corrupt WAV data for track 2.
	if err := os.WriteFile(p2, []byte("RIFF0000WAVEcorrupt"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := audio.WriteSineWAV(p3, rate, 2000, 880); err != nil {
		t.Fatal(err)
	}

	eng, err := New(Options{
		Sink:       sink.KindNull,
		SampleRate: rate,
		Unpaced:    true,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Close()

	sub, _, _ := eng.Events().Subscribe(0)
	defer eng.Events().Unsubscribe(sub)

	if _, err := eng.Play(dir, false); err != nil {
		t.Fatal(err)
	}

	sawError := false
	sawTrack3 := false
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case ev := <-sub.C:
			if ev.Type == events.Error {
				sawError = true
			}
			if ev.Type == events.Track && bytes.Contains(ev.Data, []byte("03.wav")) {
				sawTrack3 = true
			}
		default:
			if eng.Status().State == core.Stopped && sawError && sawTrack3 {
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
	if !sawError || !sawTrack3 {
		t.Fatalf("expected corrupt track 2 to emit error (%v) and skip to track 3 (%v)", sawError, sawTrack3)
	}
}

func TestCoalescedPersistenceAndDeviceChangePause(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "eklips.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	wavPath := filepath.Join(dir, "long.wav")
	// 2 seconds of audio
	if err := audio.WriteSineWAV(wavPath, 44100, 44100*2, 440); err != nil {
		t.Fatal(err)
	}

	eng, err := New(Options{
		Store:           st,
		Sink:            sink.KindNull,
		PersistInterval: 5 * time.Second,
		PacerSpeed:      4.0,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Close()

	if _, err := eng.Play(wavPath, true); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	writesAfterStart := st.Writes()

	// Let steady-state playback run for 250ms (< 5s PersistInterval); verify no
	// steady-state position writes occurred.
	time.Sleep(250 * time.Millisecond)
	if got := st.Writes() - writesAfterStart; got > 1 {
		t.Fatalf("expected at most 1 coalesced write during 250ms of playback, got %d", got)
	}

	// Simulate output device change -> must pause and never auto-resume.
	eng.OnDeviceChange("headphones unplugged")
	if eng.Status().State != core.Paused {
		t.Fatalf("expected Paused after OnDeviceChange, got %s", eng.Status().State)
	}
}

func TestSeekingForwardBackwardAndPastEnd(t *testing.T) {
	dir := t.TempDir()
	const rate = 44100
	p1 := filepath.Join(dir, "01_a.wav")
	p2 := filepath.Join(dir, "02_b.wav")
	if err := audio.WriteSineWAV(p1, rate, rate*2, 440); err != nil {
		t.Fatal(err)
	}
	if err := audio.WriteSineWAV(p2, rate, rate*3, 660); err != nil {
		t.Fatal(err)
	}
	eng, err := New(Options{Sink: sink.KindNull, CustomOutput: &captureOutput{}, SampleRate: rate, Unpaced: true})
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Close()

	if _, err := eng.Play(dir, false); err != nil {
		t.Fatal(err)
	}
	if _, err := eng.Pause(); err != nil {
		t.Fatal(err)
	}

	st, err := eng.SeekTo(1200)
	if err != nil {
		t.Fatal(err)
	}
	if st.PositionMs < 1150 || st.PositionMs > 1260 {
		t.Fatalf("SeekTo(1200): position=%d", st.PositionMs)
	}
	st, err = eng.SeekBy(-500)
	if err != nil {
		t.Fatal(err)
	}
	if st.PositionMs < 650 || st.PositionMs > 760 {
		t.Fatalf("Seek(-500): position=%d", st.PositionMs)
	}
	// Seeking past the end advances to the next track.
	st, err = eng.SeekBy(10000)
	if err != nil {
		t.Fatal(err)
	}
	if st.TrackIndex != 1 || st.PositionMs > 100 {
		t.Fatalf("seek past end: index=%d pos=%d duration=%d", st.TrackIndex, st.PositionMs, st.DurationMs)
	}
}

func TestQueueMutationsAndAppend(t *testing.T) {
	dir := t.TempDir()
	const rate = 22050
	var paths []string
	for i, name := range []string{"01_a.wav", "02_b.wav", "03_c.wav"} {
		p := filepath.Join(dir, name)
		if err := audio.WriteSineWAV(p, rate, rate, 200+float64(i)*100); err != nil {
			t.Fatal(err)
		}
		paths = append(paths, p)
	}
	eng, err := New(Options{Sink: sink.KindNull, CustomOutput: &captureOutput{}, SampleRate: rate, Unpaced: true})
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Close()

	if _, err := eng.Play(dir, false); err != nil {
		t.Fatal(err)
	}
	if _, err := eng.Pause(); err != nil {
		t.Fatal(err)
	}
	if st, err := eng.PlayIndex(2); err != nil || st.TrackIndex != 2 {
		t.Fatalf("PlayIndex: st=%+v err=%v", st, err)
	}
	// Removing a non-current track keeps the current track loaded.
	st, err := eng.QueueRemove(0)
	if err != nil {
		t.Fatal(err)
	}
	if st.TrackIndex != 1 || len(eng.Queue().Tracks) != 2 {
		t.Fatalf("after remove: index=%d tracks=%d", st.TrackIndex, len(eng.Queue().Tracks))
	}
	// Reordering preserves the current track.
	st, err = eng.QueueMove(1, 0)
	if err != nil {
		t.Fatal(err)
	}
	if st.TrackIndex != 0 || len(eng.Queue().Tracks) != 2 {
		t.Fatalf("after move: index=%d", st.TrackIndex)
	}
	// Append a new file to the active queue.
	st, err = eng.QueueAppend([]string{paths[0]})
	if err != nil {
		t.Fatal(err)
	}
	if len(eng.Queue().Tracks) != 3 {
		t.Fatalf("after append: %d tracks", len(eng.Queue().Tracks))
	}
	// Clear stops and empties.
	st, err = eng.QueueClear()
	if err != nil {
		t.Fatal(err)
	}
	if len(eng.Queue().Tracks) != 0 || st.State != core.Stopped {
		t.Fatalf("after clear: tracks=%d state=%s", len(eng.Queue().Tracks), st.State)
	}
}

func TestBrowseAndMetadata(t *testing.T) {
	dir := t.TempDir()
	if err := audio.WriteSineWAV(filepath.Join(dir, "01. Artist - Title.wav"), 22050, 22050, 440); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	eng, err := New(Options{Sink: sink.KindNull, SampleRate: 22050, MediaRoot: dir})
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Close()

	res, err := eng.Browse("")
	if err != nil {
		t.Fatal(err)
	}
	if res.Path != dir || len(res.Entries) != 2 {
		t.Fatalf("browse: path=%s entries=%d %+v", res.Path, len(res.Entries), res.Entries)
	}
	if !res.Entries[0].IsDir || res.Entries[0].Name != "sub" {
		t.Fatalf("expected directory first: %+v", res.Entries[0])
	}
	file := res.Entries[1]
	if !file.Audio || file.Metadata.Artist != "Artist" || file.Metadata.Title != "Title" {
		t.Fatalf("unexpected file metadata: %+v", file)
	}
}

func TestSoftLimitPeakLimiter(t *testing.T) {
	// 1. Signals within [-0.90, 0.90] must be 100% bit-exact linear.
	for _, v := range []float32{-0.90, -0.50, -0.10, 0.0, 0.10, 0.50, 0.90} {
		if got := softLimit(v); got != v {
			t.Fatalf("softLimit(%v) = %v, want exact %v", v, got, v)
		}
	}

	// 2. Signals above the knee (0.90) must be smoothly compressed, monotonic,
	// and strictly bounded within (-1.0, 1.0) without brick-wall hard clipping.
	prev := softLimit(0.90)
	for _, v := range []float32{0.95, 1.00, 1.10, 1.25, 1.50, 2.00} {
		pos := softLimit(v)
		neg := softLimit(-v)
		if pos <= prev || pos >= 1.0 {
			t.Fatalf("softLimit(%v) = %v, want strictly in (%v, 1.0)", v, pos, prev)
		}
		if neg != -pos {
			t.Fatalf("softLimit(-%v) = %v, want symmetric -%v", v, neg, pos)
		}
		prev = pos
	}
}

func BenchmarkGaplessSplice(b *testing.B) {
	dir := b.TempDir()
	const rate = 44100
	p1 := filepath.Join(dir, "01.wav")
	p2 := filepath.Join(dir, "02.wav")
	if err := audio.WriteSineWAV(p1, rate, 1200, 440); err != nil {
		b.Fatal(err)
	}
	if err := audio.WriteSineWAV(p2, rate, 1200, 880); err != nil {
		b.Fatal(err)
	}

	eng, err := New(Options{
		Sink:       sink.KindNull,
		SampleRate: rate,
		Unpaced:    true,
	})
	if err != nil {
		b.Fatal(err)
	}
	defer eng.Close()

	if _, err := eng.Play(dir, true); err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := eng.Next(); err != nil {
			b.Fatal(err)
		}
	}
}
