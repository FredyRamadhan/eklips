// Package engine coordinates the playback state machine, audio decoders,
// device/HTTP sinks, SSE event bus, and coalesced SQLite persistence inside a
// single lock-free actor loop.
package engine

import (
	"encoding/binary"
	"errors"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/frdn/eklips/internal/apperr"
	"github.com/frdn/eklips/internal/audio"
	"github.com/frdn/eklips/internal/config"
	"github.com/frdn/eklips/internal/core"
	"github.com/frdn/eklips/internal/events"
	"github.com/frdn/eklips/internal/media"
	"github.com/frdn/eklips/internal/playlist"
	"github.com/frdn/eklips/internal/sink"
	"github.com/frdn/eklips/internal/store"
)

// Status is the public daemon and playback status snapshot.
type Status struct {
	State        core.State      `json:"state"`
	SourceType   core.SourceType `json:"source_type"`
	SourcePath   string          `json:"source_path"`
	SourceName   string          `json:"source_name,omitempty"`
	CurrentTrack string          `json:"current_track"`
	TrackIndex   int             `json:"index"`
	PositionMs   int64           `json:"position_ms"`
	DurationMs   int64           `json:"duration_ms"`
	Volume       int             `json:"volume"`
	Muted        bool            `json:"muted"`
	Shuffle      bool            `json:"shuffle"`
	Loop         bool            `json:"loop"`
	Sink         sink.Kind       `json:"sink"`
	Listeners    int             `json:"listeners"`
	LastError    string          `json:"last_error,omitempty"`
	Metadata     audio.Metadata  `json:"metadata,omitempty"`
}

// QueueItem represents one track in the loaded queue.
type QueueItem struct {
	Index    int            `json:"index"`
	Path     string         `json:"path"`
	Name     string         `json:"name"`
	Current  bool           `json:"current"`
	Metadata audio.Metadata `json:"metadata,omitempty"`
}

// Levels is a real-time stereo peak-meter snapshot in dBFS (-60..0).
type Levels struct {
	Left  float64 `json:"left"`
	Right float64 `json:"right"`
	PeakL float64 `json:"peak_left"`
	PeakR float64 `json:"peak_right"`
}

// BrowseEntry is one directory or audio file returned by Browse.
type BrowseEntry struct {
	Name     string         `json:"name"`
	Path     string         `json:"path"`
	IsDir    bool           `json:"is_dir"`
	Audio    bool           `json:"audio"`
	Size     int64          `json:"size,omitempty"`
	Metadata audio.Metadata `json:"metadata,omitempty"`
}

// BrowseResult is a directory listing for the TUI file browser.
type BrowseResult struct {
	Path    string        `json:"path"`
	Parent  string        `json:"parent,omitempty"`
	Root    string        `json:"root,omitempty"`
	Entries []BrowseEntry `json:"entries"`
}

// PlaylistInfo describes a saved playlist.
type PlaylistInfo struct {
	Name       string    `json:"name"`
	SourcePath string    `json:"source_path"`
	TrackCount int       `json:"track_count"`
	UpdatedAt  time.Time `json:"updated_at"`
}

// PlaylistView is an inspectable playlist with its tracks.
type PlaylistView struct {
	PlaylistInfo
	Tracks []QueueItem `json:"tracks"`
}

// Queue is the public queue snapshot returned by GET /api/v1/queue.
type Queue struct {
	SourceType   core.SourceType `json:"source_type"`
	SourcePath   string          `json:"source_path"`
	SourceName   string          `json:"source_name,omitempty"`
	CurrentIndex int             `json:"current_index"`
	Cursor       int             `json:"cursor"`
	Shuffle      bool            `json:"shuffle"`
	Loop         bool            `json:"loop"`
	Tracks       []QueueItem     `json:"tracks"`
	PlayOrder    []int           `json:"play_order"`
}

// Player is the control interface implemented by Engine.
type Player interface {
	Play(path string, loop bool) (Status, error)
	Pause() (Status, error)
	Stop() (Status, error)
	Next() (Status, error)
	Previous() (Status, error)
	VolumeUp() (Status, error)
	VolumeDown() (Status, error)
	ToggleMute() (Status, error)
	ToggleShuffle() (Status, error)
	ToggleLoop() (Status, error)
	SeekTo(posMs int64) (Status, error)
	SeekBy(deltaMs int64) (Status, error)
	PlayIndex(index int) (Status, error)
	QueueRemove(index int) (Status, error)
	QueueMove(fromIndex, toIndex int) (Status, error)
	QueueClear() (Status, error)
	QueueAppend(paths []string) (Status, error)
	Status() Status
	Queue() Queue
	Levels() Levels
	Browse(path string) (BrowseResult, error)
	PlaylistCreate(dir, output string, force bool) (string, error)
	PlaylistPlay(target string, loop bool) (Status, error)
	PlaylistList() ([]PlaylistInfo, error)
	PlaylistInspect(target string) (PlaylistView, error)
	PlaylistRemove(target string) error
	AliasSet(name, playlistPath string) (store.Alias, error)
	AliasList() ([]store.Alias, error)
	AliasRemove(name string) error
	OnDeviceChange(reason string)
	Events() *events.Bus
	Broadcaster() *sink.Broadcaster
	SinkKind() sink.Kind
	Close() error
}

// Options configures an Engine instance.
type Options struct {
	Store           *store.Store
	LockPath        string
	DisableLock     bool
	Sink            sink.Kind
	CustomOutput    sink.Output
	SampleRate      int
	MaxListeners    int
	MaxQueue        int
	MediaRoot       string
	ResumeOnStart   bool
	PersistInterval time.Duration
	PacerSpeed      float64
	Unpaced         bool
	Seed            uint64
}

type cmdReq struct {
	fn    func() (Status, error)
	reply chan cmdResp
}

type cmdResp struct {
	status Status
	err    error
}

type prefetchResult struct {
	gen   uint64
	index int
	path  string
	track *audio.Track
	err   error
}

// Engine owns the playback state machine, audio pipeline, and sinks.
type Engine struct {
	opts            Options
	store           *store.Store
	lock            *InstanceLock
	sinkKind        sink.Kind
	format          sink.Format
	sampleRate      int
	chunkFrames     int
	prebufferFrames int
	prefetchWindow  int64 // frames before EOF at which prefetch triggers
	persistInterval time.Duration

	out      sink.Output
	hasClock bool
	pacer    *sink.Pacer
	bcast    *sink.Broadcaster
	bus      *events.Bus

	// Owned exclusively by the actor loop goroutine:
	machine     *core.Machine
	cur         *audio.Track
	pref        *audio.Track
	prefGen     uint64
	prefErr     error
	prefetching bool
	prefetchCh  chan prefetchResult
	fbuf        []float32
	pcm         []byte
	silence     []byte
	lastErr     string
	lastPersist time.Time

	// Lock-free atomic state for readers:
	posFrames  atomic.Int64
	durFrames  atomic.Int64
	statusSnap atomic.Pointer[Status]
	queueSnap  atomic.Pointer[Queue]

	// Stereo peak metering (updated from the audio loop, read lock-free).
	levelL    atomic.Uint32 // linear peak * 10000
	levelR    atomic.Uint32
	levelTime atomic.Int64 // unix nanos of the last update

	// Tag cache shared by queue snapshots and the file browser.
	metaMu    sync.RWMutex
	metaCache map[string]audio.Metadata

	cmdCh       chan cmdReq
	persistImm  chan store.Playback
	persistPos  chan store.Playback
	stopCh      chan struct{}
	loopDone    chan struct{}
	persistDone chan struct{}
	closeOnce   sync.Once
}

// New creates and starts an Engine.
func New(opts Options) (*Engine, error) {
	if opts.Sink == "" {
		opts.Sink = sink.KindDevice
	}
	if _, ok := sink.ParseKind(string(opts.Sink)); !ok {
		return nil, apperr.New(apperr.InvalidCommand, "invalid sink %q", opts.Sink)
	}
	if opts.SampleRate <= 0 {
		opts.SampleRate = config.DefaultSampleRate
	}
	if opts.MaxListeners <= 0 {
		opts.MaxListeners = config.DefaultMaxListeners
	}
	if opts.MaxQueue <= 0 {
		opts.MaxQueue = config.DefaultMaxQueue
	}
	if opts.PersistInterval <= 0 {
		opts.PersistInterval = 5 * time.Second
	}
	if opts.Seed == 0 {
		opts.Seed = uint64(time.Now().UnixNano())
	}

	lockPath := opts.LockPath
	if lockPath == "" && opts.Store != nil && !opts.DisableLock {
		lockPath = opts.Store.Path() + ".lock"
	}
	var lock *InstanceLock
	if lockPath != "" && !opts.DisableLock {
		var err error
		lock, err = AcquireLock(lockPath)
		if err != nil {
			return nil, err
		}
	}

	f := sink.Format{SampleRate: opts.SampleRate, Channels: audio.Channels}
	chunkFrames := opts.SampleRate / 50 // 20 ms (882 frames at 44.1 kHz)
	if chunkFrames < 64 {
		chunkFrames = 64
	}
	chunkBytes := chunkFrames * f.BytesPerFrame()

	var out sink.Output = sink.Null{}
	hasClock := false
	if opts.CustomOutput != nil {
		out = opts.CustomOutput
		hasClock = opts.Sink.HasDevice()
	} else if opts.Sink.HasDevice() {
		dev, err := sink.OpenDevice(f, sink.DefaultDeviceBuffer)
		if err != nil {
			_ = lock.Release()
			return nil, err
		}
		out = dev
		hasClock = true
	}

	e := &Engine{
		opts:            opts,
		store:           opts.Store,
		lock:            lock,
		sinkKind:        opts.Sink,
		format:          f,
		sampleRate:      opts.SampleRate,
		chunkFrames:     chunkFrames,
		prebufferFrames: opts.SampleRate / 2, // 0.5 s prebuffer
		prefetchWindow:  int64(opts.SampleRate * 10),
		persistInterval: opts.PersistInterval,
		out:             out,
		hasClock:        hasClock,
		pacer:           sink.NewPacer(opts.SampleRate, opts.PacerSpeed),
		bcast:           sink.NewBroadcaster(f, chunkBytes, 2*time.Second, opts.MaxListeners),
		bus:             events.NewBus(256, 64),
		machine:         core.NewMachine(opts.Seed),
		metaCache:       make(map[string]audio.Metadata),
		prefetchCh:      make(chan prefetchResult, 4),
		fbuf:            make([]float32, chunkFrames*audio.Channels),
		pcm:             make([]byte, chunkBytes),
		silence:         make([]byte, chunkBytes),
		cmdCh:           make(chan cmdReq, 32),
		persistImm:      make(chan store.Playback, 1),
		persistPos:      make(chan store.Playback, 1),
		stopCh:          make(chan struct{}),
		loopDone:        make(chan struct{}),
		persistDone:     make(chan struct{}),
	}

	e.restoreFromStore()
	e.updateSnapshots()

	go e.runPersister()
	go e.runLoop()
	return e, nil
}

func (e *Engine) restoreFromStore() {
	if e.store == nil {
		return
	}
	resumeOnStart := e.opts.ResumeOnStart
	if !resumeOnStart {
		if v, ok, err := e.store.GetSetting("resume_on_start"); err == nil && ok {
			resumeOnStart = v == "true" || v == "1"
		}
	}
	pb, ok, err := e.store.LoadPlayback()
	if err != nil || !ok {
		return
	}
	var src core.Source
	switch core.SourceType(pb.SourceType) {
	case core.Single:
		if media.CheckAudioFile(pb.SourcePath) == nil {
			src = core.Source{
				Type:   core.Single,
				Path:   pb.SourcePath,
				Name:   filepath.Base(pb.SourcePath),
				Tracks: []string{pb.SourcePath},
			}
		}
	case core.Playlist:
		src = e.resolvePersistedPlaylist(pb.SourcePath)
	}
	targetState := core.Stopped
	if resumeOnStart && src.Type != core.None && pb.State == string(core.Playing) {
		targetState = core.Playing
	}
	e.machine.Restore(core.RestoreParams{
		Source:  src,
		Index:   pb.CurrentIndex,
		State:   targetState,
		Volume:  pb.Volume,
		Muted:   pb.Muted,
		Shuffle: pb.Shuffle,
		Loop:    pb.Loop,
	})
	if targetState == core.Playing {
		tr, err := audio.Open(e.machine.CurrentTrack(), e.sampleRate)
		if err != nil {
			e.machine.ForceStop()
			return
		}
		if pb.PositionMs > 0 {
			skipFrames := pb.PositionMs * int64(e.sampleRate) / 1000
			_ = tr.Skip(skipFrames)
			e.posFrames.Store(skipFrames)
		}
		e.cur = tr
		e.durFrames.Store(max(0, tr.TotalFrames()))
		e.primeDevice()
		e.out.Resume()
	}
}

func (e *Engine) resolvePersistedPlaylist(sourcePath string) core.Source {
	if sourcePath == "" {
		return core.Source{}
	}
	if st, err := os.Stat(sourcePath); err == nil {
		if st.IsDir() {
			if files, err := media.Scan(sourcePath, e.opts.MaxQueue); err == nil && len(files) > 0 {
				return core.Source{
					Type:   core.Playlist,
					Path:   sourcePath,
					Name:   filepath.Base(sourcePath),
					Tracks: files,
				}
			}
		} else if media.IsPlaylist(sourcePath) {
			if pl, err := playlist.Load(sourcePath, e.opts.MaxQueue); err == nil {
				return core.Source{
					Type:   core.Playlist,
					Path:   sourcePath,
					Name:   pl.Name,
					Tracks: pl.Tracks,
				}
			}
		}
	}
	if name, tracks, err := e.store.PlaylistTracks(sourcePath); err == nil && len(tracks) > 0 {
		valid := make([]string, 0, len(tracks))
		for _, t := range tracks {
			if media.CheckAudioFile(t) == nil {
				valid = append(valid, t)
			}
		}
		if len(valid) > 0 {
			return core.Source{
				Type:   core.Playlist,
				Path:   sourcePath,
				Name:   name,
				Tracks: valid,
			}
		}
	}
	return core.Source{}
}

func (e *Engine) runPersister() {
	defer close(e.persistDone)
	if e.store == nil {
		return
	}
	for {
		select {
		case pb := <-e.persistImm:
			// Drain any older queued position update since immediate is newer.
			select {
			case <-e.persistPos:
			default:
			}
			_ = e.store.SavePlayback(pb)
		case pb := <-e.persistPos:
			_ = e.store.SavePlayback(pb)
		case <-e.stopCh:
			for {
				select {
				case pb := <-e.persistImm:
					_ = e.store.SavePlayback(pb)
				case pb := <-e.persistPos:
					_ = e.store.SavePlayback(pb)
				default:
					return
				}
			}
		}
	}
}

func (e *Engine) playbackRow() store.Playback {
	snap := e.machine.Snapshot()
	posMs := e.posFrames.Load() * 1000 / int64(e.sampleRate)
	idx := snap.TrackIndex
	if idx < 0 {
		idx = 0
	}
	return store.Playback{
		SourceType:   string(snap.Source.Type),
		SourcePath:   snap.Source.Path,
		CurrentIndex: idx,
		PositionMs:   posMs,
		State:        string(snap.State),
		Volume:       snap.Volume,
		Muted:        snap.Muted,
		Shuffle:      snap.Shuffle,
		Loop:         snap.Loop,
		UpdatedAt:    time.Now(),
	}
}

func (e *Engine) schedulePersist(immediate bool) {
	if e.store == nil {
		return
	}
	now := time.Now()
	if immediate {
		e.lastPersist = now
		pb := e.playbackRow()
		select {
		case e.persistImm <- pb:
		default:
			select {
			case <-e.persistImm:
			default:
			}
			select {
			case e.persistImm <- pb:
			default:
			}
		}
		return
	}
	if now.Sub(e.lastPersist) < e.persistInterval {
		return
	}
	e.lastPersist = now
	pb := e.playbackRow()
	select {
	case e.persistPos <- pb:
	default:
	}
}

func (e *Engine) updateSnapshots() {
	ms := e.machine.Snapshot()
	curTrack := ""
	var curMeta audio.Metadata
	if ms.TrackIndex >= 0 && ms.TrackIndex < len(ms.Source.Tracks) {
		curTrack = ms.Source.Tracks[ms.TrackIndex]
		curMeta = e.metadataFor(curTrack)
	}
	st := &Status{
		State:        ms.State,
		SourceType:   ms.Source.Type,
		SourcePath:   ms.Source.Path,
		SourceName:   ms.Source.Name,
		CurrentTrack: curTrack,
		TrackIndex:   ms.TrackIndex,
		PositionMs:   e.posFrames.Load() * 1000 / int64(e.sampleRate),
		DurationMs:   e.durFrames.Load() * 1000 / int64(e.sampleRate),
		Volume:       ms.Volume,
		Muted:        ms.Muted,
		Shuffle:      ms.Shuffle,
		Loop:         ms.Loop,
		Sink:         e.sinkKind,
		Listeners:    e.bcast.Count(),
		LastError:    e.lastErr,
		Metadata:     curMeta,
	}
	e.statusSnap.Store(st)

	// Tags are read lazily for the current track and for reasonably sized
	// queues. Huge queues use filename heuristics so a queue load never stalls
	// the audio actor on thousands of file reads.
	const tagQueueLimit = 500
	useTags := len(ms.Source.Tracks) <= tagQueueLimit
	items := make([]QueueItem, len(ms.Source.Tracks))
	for i, p := range ms.Source.Tracks {
		md := audio.Metadata{}
		if useTags || i == ms.TrackIndex {
			md = e.metadataFor(p)
		} else {
			md = audio.FilenameMetadata(p)
		}
		items[i] = QueueItem{
			Index:    i,
			Path:     p,
			Name:     filepath.Base(p),
			Current:  i == ms.TrackIndex,
			Metadata: md,
		}
	}
	orderCopy := make([]int, len(ms.Order))
	copy(orderCopy, ms.Order)
	q := &Queue{
		SourceType:   ms.Source.Type,
		SourcePath:   ms.Source.Path,
		SourceName:   ms.Source.Name,
		CurrentIndex: ms.TrackIndex,
		Cursor:       ms.Cursor,
		Shuffle:      ms.Shuffle,
		Loop:         ms.Loop,
		Tracks:       items,
		PlayOrder:    orderCopy,
	}
	e.queueSnap.Store(q)
}

// Status returns a lock-free snapshot of the current status.
func (e *Engine) Status() Status {
	st := *e.statusSnap.Load()
	st.PositionMs = e.posFrames.Load() * 1000 / int64(e.sampleRate)
	st.DurationMs = e.durFrames.Load() * 1000 / int64(e.sampleRate)
	st.Listeners = e.bcast.Count()
	return st
}

// Queue returns a lock-free snapshot of the current queue.
func (e *Engine) Queue() Queue {
	return *e.queueSnap.Load()
}

func (e *Engine) metadataFor(path string) audio.Metadata {
	if path == "" {
		return audio.Metadata{}
	}
	e.metaMu.RLock()
	md, ok := e.metaCache[path]
	e.metaMu.RUnlock()
	if ok {
		return md
	}
	md = audio.ReadMetadata(path)
	e.metaMu.Lock()
	if len(e.metaCache) > 50000 {
		clear(e.metaCache)
	}
	e.metaCache[path] = md
	e.metaMu.Unlock()
	return md
}

// Levels returns the current stereo peak meter with an exponential decay so
// the visualizer falls smoothly to silence after playback stops.
func (e *Engine) Levels() Levels {
	l := float64(e.levelL.Load()) / 10000
	r := float64(e.levelR.Load()) / 10000
	last := e.levelTime.Load()
	if last > 0 {
		elapsed := time.Since(time.Unix(0, last))
		if elapsed > 30*time.Millisecond {
			decay := math.Pow(0.72, elapsed.Seconds()*10)
			l *= decay
			r *= decay
			if l < 0.0005 {
				l = 0
			}
			if r < 0.0005 {
				r = 0
			}
		}
	}
	return Levels{Left: toDB(l), Right: toDB(r), PeakL: l, PeakR: r}
}

func toDB(peak float64) float64 {
	if peak <= 0.00001 {
		return -60
	}
	db := 20 * math.Log10(peak)
	if db < -60 {
		return -60
	}
	if db > 0 {
		return 0
	}
	return db
}

func abs32(v float32) float32 {
	if v < 0 {
		return -v
	}
	return v
}

func min32(v, max float32) float32 {
	if v > max {
		return max
	}
	return v
}

// Events returns the SSE event bus.
func (e *Engine) Events() *events.Bus { return e.bus }

// Broadcaster returns the HTTP stream broadcaster.
func (e *Engine) Broadcaster() *sink.Broadcaster { return e.bcast }

// SinkKind returns the active sink mode.
func (e *Engine) SinkKind() sink.Kind { return e.sinkKind }

func (e *Engine) do(fn func() (Status, error)) (Status, error) {
	req := cmdReq{fn: fn, reply: make(chan cmdResp, 1)}
	select {
	case e.cmdCh <- req:
	case <-e.stopCh:
		return Status{}, apperr.New(apperr.ServiceNotRunning, "engine is shutting down")
	}
	select {
	case resp := <-req.reply:
		return resp.status, resp.err
	case <-e.stopCh:
		return Status{}, apperr.New(apperr.ServiceNotRunning, "engine is shutting down")
	}
}

func (e *Engine) runLoop() {
	defer close(e.loopDone)
	defer func() {
		e.closePipeline()
	}()

	for {
		// Process all pending commands first so controls are never starved by audio.
		for {
			select {
			case <-e.stopCh:
				return
			case req := <-e.cmdCh:
				st, err := req.fn()
				req.reply <- cmdResp{status: st, err: err}
				continue
			case res := <-e.prefetchCh:
				e.handlePrefetchResult(res)
				continue
			default:
			}
			break
		}

		state := e.machine.State()
		if state == core.Playing && e.cur != nil {
			e.maybeStartPrefetch()
			e.stepAudio()
			e.schedulePersist(false)
			continue
		}

		// Not playing: if HTTP listeners are attached, stream silence at 1x so
		// open HTTP connections stay alive across pause/stop.
		if e.bcast.Count() > 0 {
			e.bcast.Publish(e.silence)
			if !e.opts.Unpaced {
				e.pacer.Wait(e.chunkFrames)
			}
			continue
		}

		// Truly idle: reset pacer and block with zero CPU usage.
		e.pacer.Reset()
		select {
		case <-e.stopCh:
			return
		case req := <-e.cmdCh:
			st, err := req.fn()
			req.reply <- cmdResp{status: st, err: err}
		case res := <-e.prefetchCh:
			e.handlePrefetchResult(res)
		case <-e.bcast.Joined():
		}
	}
}

func (e *Engine) maybeStartPrefetch() {
	if e.pref != nil || e.prefetching || e.cur == nil {
		return
	}
	nextIdx := e.machine.PeekNext()
	if nextIdx < 0 {
		return
	}
	// If we already failed prefetching this exact generation, don't retry in a tight loop.
	if e.prefErr != nil && e.prefGen == e.machine.Gen() {
		return
	}
	tot := e.cur.TotalFrames()
	pos := e.posFrames.Load()
	if tot >= 0 && (tot-pos) > e.prefetchWindow {
		return
	}
	snap := e.machine.Snapshot()
	if nextIdx >= len(snap.Source.Tracks) {
		return
	}
	nextPath := snap.Source.Tracks[nextIdx]
	gen := snap.Gen
	e.prefetching = true
	rate := e.sampleRate
	preFrames := e.prebufferFrames
	go func() {
		tr, err := audio.Open(nextPath, rate)
		if err == nil {
			err = tr.Prebuffer(preFrames)
			if err != nil {
				_ = tr.Close()
				tr = nil
			}
		}
		e.prefetchCh <- prefetchResult{
			gen:   gen,
			index: nextIdx,
			path:  nextPath,
			track: tr,
			err:   err,
		}
	}()
}

func (e *Engine) handlePrefetchResult(res prefetchResult) {
	e.prefetching = false
	if res.gen != e.machine.Gen() || e.machine.State() == core.Stopped {
		if res.track != nil {
			_ = res.track.Close()
		}
		return
	}
	if res.err != nil {
		e.prefErr = res.err
		e.prefGen = res.gen
		return
	}
	if e.pref != nil {
		_ = e.pref.Close()
	}
	e.pref = res.track
	e.prefGen = res.gen
	e.prefErr = nil
}

func (e *Engine) invalidatePrefetch() {
	if e.pref != nil {
		_ = e.pref.Close()
		e.pref = nil
	}
	e.prefErr = nil
	e.prefGen = 0
}

func (e *Engine) closePipeline() {
	if e.cur != nil {
		_ = e.cur.Close()
		e.cur = nil
	}
	e.invalidatePrefetch()
}

func (e *Engine) stepAudio() {
	filled := 0
	for filled < e.chunkFrames && e.cur != nil {
		n, err := e.cur.Read(e.fbuf[filled*2 : e.chunkFrames*2])
		filled += n
		e.posFrames.Add(int64(n))
		if errors.Is(err, io.EOF) {
			if !e.spliceToNext() {
				break
			}
			continue
		}
		if err != nil {
			e.emitError(apperr.Wrap(apperr.DecodeError, err, "decode %s", filepath.Base(e.cur.Path)))
			if !e.spliceToNext() {
				break
			}
			continue
		}
		if n == 0 {
			if !e.spliceToNext() {
				break
			}
		}
	}
	if filled == 0 {
		return
	}

	gain := float32(e.machine.Volume()) / 100.0
	if e.machine.Muted() {
		gain = 0
	}
	samples := filled * 2
	var peakL, peakR float32
	for i := 0; i < samples; i++ {
		v := softLimit(e.fbuf[i] * gain)
		if i&1 == 0 {
			if a := abs32(v); a > peakL {
				peakL = a
			}
		} else if a := abs32(v); a > peakR {
			peakR = a
		}
		s := int16(math.Round(float64(v * 32767.0)))
		binary.LittleEndian.PutUint16(e.pcm[i*2:], uint16(s))
	}
	e.levelL.Store(uint32(min32(peakL, 1) * 10000))
	e.levelR.Store(uint32(min32(peakR, 1) * 10000))
	e.levelTime.Store(time.Now().UnixNano())

	outBytes := e.pcm[:filled*4]
	if e.sinkKind.HasHTTP() || e.bcast.Count() > 0 {
		e.bcast.Publish(outBytes)
	}
	if err := e.out.Write(outBytes); err != nil {
		e.emitError(err)
		if e.machine.ForcePause() {
			e.out.Pause()
			e.updateSnapshots()
			e.bus.Publish(events.State, e.Status())
			e.schedulePersist(true)
		}
		return
	}
	if !e.hasClock && !e.opts.Unpaced {
		e.pacer.Wait(filled)
	}
}

// spliceToNext transitions at a sample boundary to the next playable track in
// the queue without resetting the sink or HTTP stream.
func (e *Engine) spliceToNext() bool {
	if e.cur != nil {
		_ = e.cur.Close()
		e.cur = nil
	}

	// If a prefetch for the current generation is still in flight, wait for it.
	if e.prefetching {
		res := <-e.prefetchCh
		e.handlePrefetchResult(res)
	}

	genBefore := e.machine.Gen()
	var prefTrack *audio.Track
	var prefErr error
	if e.prefGen == genBefore {
		prefTrack = e.pref
		prefErr = e.prefErr
		e.pref = nil
		e.prefErr = nil
		e.prefGen = 0
	} else {
		e.invalidatePrefetch()
	}

	firstAttempt := true
	maxAttempts := len(e.machine.Snapshot().Source.Tracks) + 1
	for attempts := 0; attempts < maxAttempts; attempts++ {
		if !e.machine.Advance() {
			e.posFrames.Store(0)
			e.durFrames.Store(0)
			e.updateSnapshots()
			e.bus.Publish(events.State, e.Status())
			e.schedulePersist(true)
			return false
		}

		var tr *audio.Track
		var err error
		if firstAttempt && (prefTrack != nil || prefErr != nil) {
			tr = prefTrack
			err = prefErr
			prefTrack = nil
			prefErr = nil
		} else {
			path := e.machine.CurrentTrack()
			tr, err = audio.Open(path, e.sampleRate)
			if err == nil {
				err = tr.Prebuffer(e.prebufferFrames)
				if err != nil {
					_ = tr.Close()
					tr = nil
				}
			}
		}
		firstAttempt = false

		if err != nil {
			e.emitError(err)
			continue
		}

		e.cur = tr
		e.posFrames.Store(0)
		e.durFrames.Store(max(0, tr.TotalFrames()))
		e.updateSnapshots()
		e.bus.Publish(events.Track, e.Status())
		e.schedulePersist(true)
		return true
	}

	e.machine.ForceStop()
	e.posFrames.Store(0)
	e.durFrames.Store(0)
	e.updateSnapshots()
	e.bus.Publish(events.State, e.Status())
	e.schedulePersist(true)
	return false
}

func (e *Engine) emitError(err error) {
	if err == nil {
		return
	}
	ae := apperr.As(err)
	e.lastErr = ae.Error()
	e.updateSnapshots()
	e.bus.Publish(events.Error, ae)
}

// softLimit applies a C¹-continuous rational soft-knee peak limiter above |v| > 0.90.
// Signals within [-0.90, 0.90] are 100% linear (bit-exact); hot inter-sample peaks
// from lossy decoders or heavy bass transients are smoothly compressed asymptotically
// toward ±1.0 rather than sheared into square waves by hard digital clipping.
func softLimit(v float32) float32 {
	const knee = 0.90
	const head = 1.0 - knee // 0.10
	switch {
	case v > knee:
		d := v - knee
		return knee + (head*d)/(head+d)
	case v < -knee:
		d := -v - knee
		return -(knee + (head*d)/(head+d))
	default:
		return v
	}
}

// primeDevice pre-fills ~160 ms (8 chunks) into the hardware output ring buffer
// before unpausing the audio device so the OS callback has an immediate cushion.
func (e *Engine) primeDevice() {
	if !e.hasClock || e.opts.CustomOutput != nil {
		return
	}
	const preRollChunks = 8
	for i := 0; i < preRollChunks && e.cur != nil && e.machine.State() == core.Playing; i++ {
		e.stepAudio()
	}
}

func (e *Engine) startCurrentTrack() error {
	e.closePipeline()
	path := e.machine.CurrentTrack()
	tr, err := audio.Open(path, e.sampleRate)
	if err != nil {
		return err
	}
	if err := tr.Prebuffer(e.chunkFrames * 8); err != nil {
		_ = tr.Close()
		return err
	}
	e.cur = tr
	e.posFrames.Store(0)
	e.durFrames.Store(max(0, tr.TotalFrames()))
	e.out.Flush()
	if e.machine.State() == core.Playing {
		e.primeDevice()
		e.out.Resume()
	}
	e.pacer.Reset()
	return nil
}

// Play loads and plays path, or resumes the loaded queue when path is empty.
func (e *Engine) Play(path string, loop bool) (Status, error) {
	if strings.TrimSpace(path) == "" {
		return e.do(func() (Status, error) {
			if loop && e.machine.Loaded() && !e.machine.Snapshot().Loop {
				if e.machine.Snapshot().Source.Type == core.Playlist {
					_, _ = e.machine.ToggleLoop()
				}
			}
			eff, err := e.machine.Play()
			if err != nil {
				return e.Status(), err
			}
			switch eff {
			case core.EffectStart:
				if err := e.startCurrentTrack(); err != nil {
					e.machine.ForceStop()
					e.emitError(err)
					return e.Status(), err
				}
			case core.EffectResume:
				e.out.Resume()
				e.pacer.Reset()
			}
			e.lastErr = ""
			e.updateSnapshots()
			st := e.Status()
			e.bus.Publish(events.State, st)
			e.schedulePersist(true)
			return st, nil
		})
	}

	abs, err := config.NormalizePath(path, e.opts.MediaRoot)
	if err != nil {
		return e.Status(), err
	}
	info, err := os.Stat(abs)
	if err != nil {
		if os.IsNotExist(err) {
			return e.Status(), apperr.New(apperr.FileNotFound, "%s does not exist", abs)
		}
		return e.Status(), apperr.Wrap(apperr.FileNotFound, err, "stat %s", abs)
	}

	if info.IsDir() {
		files, err := media.Scan(abs, e.opts.MaxQueue)
		if err != nil {
			return e.Status(), err
		}
		if len(files) == 0 {
			entries, _ := os.ReadDir(abs)
			for _, ent := range entries {
				if !strings.HasPrefix(ent.Name(), ".") && ent.Type().IsRegular() {
					return e.Status(), apperr.New(apperr.UnsupportedFormat, "no supported audio files in %s", abs)
				}
			}
			return e.Status(), apperr.New(apperr.FileNotFound, "no audio files found in %s", abs)
		}
		src := core.Source{
			Type:   core.Playlist,
			Path:   abs,
			Name:   filepath.Base(abs),
			Tracks: files,
		}
		return e.loadAndStart(src, loop, true)
	}

	if media.IsPlaylist(abs) {
		return e.Status(), apperr.New(apperr.UnsupportedFormat,
			"%s is a playlist JSON file; use 'eklips playlist play'", filepath.Base(abs))
	}
	if err := media.CheckAudioFile(abs); err != nil {
		return e.Status(), err
	}
	src := core.Source{
		Type:   core.Single,
		Path:   abs,
		Name:   filepath.Base(abs),
		Tracks: []string{abs},
	}
	return e.loadAndStart(src, loop, false)
}

func (e *Engine) loadAndStart(src core.Source, loop bool, persistPlaylist bool) (Status, error) {
	// Open and prebuffer the first playable track before mutating machine state
	// so invalid/corrupt files fail cleanly without clobbering existing playback.
	var firstTrack *audio.Track
	firstIdx := 0
	var openErr error
	for i, p := range src.Tracks {
		tr, err := audio.Open(p, e.sampleRate)
		if err == nil {
			err = tr.Prebuffer(e.chunkFrames * 8)
			if err != nil {
				_ = tr.Close()
				tr = nil
			}
		}
		if err == nil {
			firstTrack = tr
			firstIdx = i
			break
		}
		openErr = err
		if src.Type == core.Single {
			break
		}
	}
	if firstTrack == nil {
		if openErr != nil {
			return e.Status(), openErr
		}
		return e.Status(), apperr.New(apperr.DecodeError, "no playable tracks in %s", src.Path)
	}

	if persistPlaylist && e.store != nil {
		if _, err := e.store.SavePlaylist(src.Name, src.Path, src.Tracks); err != nil {
			_ = firstTrack.Close()
			return e.Status(), err
		}
	}

	return e.do(func() (Status, error) {
		e.closePipeline()
		if _, err := e.machine.Load(src, loop); err != nil {
			_ = firstTrack.Close()
			return e.Status(), err
		}
		for i := 0; i < firstIdx; i++ {
			e.machine.Advance()
		}
		e.cur = firstTrack
		e.posFrames.Store(0)
		e.durFrames.Store(max(0, firstTrack.TotalFrames()))
		e.lastErr = ""
		e.out.Flush()
		e.primeDevice()
		e.out.Resume()
		e.pacer.Reset()
		e.updateSnapshots()
		st := e.Status()
		e.bus.Publish(events.Queue, e.Queue())
		e.bus.Publish(events.Track, st)
		e.bus.Publish(events.State, st)
		e.schedulePersist(true)
		return st, nil
	})
}

// Pause pauses active playback.
func (e *Engine) Pause() (Status, error) {
	return e.do(func() (Status, error) {
		eff, err := e.machine.Pause()
		if err != nil {
			return e.Status(), err
		}
		if eff == core.EffectPause {
			e.out.Pause()
		}
		e.updateSnapshots()
		st := e.Status()
		e.bus.Publish(events.State, st)
		e.schedulePersist(true)
		return st, nil
	})
}

// Stop stops playback while retaining the loaded source.
func (e *Engine) Stop() (Status, error) {
	return e.do(func() (Status, error) {
		eff, err := e.machine.Stop()
		if err != nil {
			return e.Status(), err
		}
		if eff == core.EffectStop {
			e.closePipeline()
			e.out.Flush()
			e.out.Pause()
			e.posFrames.Store(0)
			e.durFrames.Store(0)
		}
		e.updateSnapshots()
		st := e.Status()
		e.bus.Publish(events.State, st)
		e.schedulePersist(true)
		return st, nil
	})
}

// Next advances to the next track in the playlist.
func (e *Engine) Next() (Status, error) {
	return e.do(func() (Status, error) {
		wasPaused := e.machine.State() == core.Paused
		if _, err := e.machine.Next(); err != nil {
			return e.Status(), err
		}
		if err := e.startCurrentTrack(); err != nil {
			e.emitError(err)
			return e.Status(), err
		}
		if wasPaused {
			e.out.Pause()
		}
		e.updateSnapshots()
		st := e.Status()
		e.bus.Publish(events.Track, st)
		e.schedulePersist(true)
		return st, nil
	})
}

// Previous moves to the previous track in the playlist.
func (e *Engine) Previous() (Status, error) {
	return e.do(func() (Status, error) {
		wasPaused := e.machine.State() == core.Paused
		if _, err := e.machine.Previous(); err != nil {
			return e.Status(), err
		}
		if err := e.startCurrentTrack(); err != nil {
			e.emitError(err)
			return e.Status(), err
		}
		if wasPaused {
			e.out.Pause()
		}
		e.updateSnapshots()
		st := e.Status()
		e.bus.Publish(events.Track, st)
		e.schedulePersist(true)
		return st, nil
	})
}

// VolumeUp increases volume by 5.
func (e *Engine) VolumeUp() (Status, error) {
	return e.do(func() (Status, error) {
		if _, err := e.machine.VolumeUp(); err != nil {
			return e.Status(), err
		}
		e.updateSnapshots()
		st := e.Status()
		e.bus.Publish(events.Volume, map[string]any{"volume": st.Volume, "muted": st.Muted})
		e.schedulePersist(true)
		return st, nil
	})
}

// VolumeDown decreases volume by 5.
func (e *Engine) VolumeDown() (Status, error) {
	return e.do(func() (Status, error) {
		if _, err := e.machine.VolumeDown(); err != nil {
			return e.Status(), err
		}
		e.updateSnapshots()
		st := e.Status()
		e.bus.Publish(events.Volume, map[string]any{"volume": st.Volume, "muted": st.Muted})
		e.schedulePersist(true)
		return st, nil
	})
}

// ToggleMute toggles mute while retaining the volume level.
func (e *Engine) ToggleMute() (Status, error) {
	return e.do(func() (Status, error) {
		if _, err := e.machine.ToggleMute(); err != nil {
			return e.Status(), err
		}
		e.updateSnapshots()
		st := e.Status()
		e.bus.Publish(events.Mute, map[string]any{"volume": st.Volume, "muted": st.Muted})
		e.schedulePersist(true)
		return st, nil
	})
}

// ToggleShuffle toggles playlist shuffle without restarting the current track.
func (e *Engine) ToggleShuffle() (Status, error) {
	return e.do(func() (Status, error) {
		if _, err := e.machine.ToggleShuffle(); err != nil {
			return e.Status(), err
		}
		e.invalidatePrefetch()
		e.updateSnapshots()
		st := e.Status()
		e.bus.Publish(events.Shuffle, map[string]any{"shuffle": st.Shuffle})
		e.bus.Publish(events.Queue, e.Queue())
		e.schedulePersist(true)
		return st, nil
	})
}

// ToggleLoop toggles playlist loop.
func (e *Engine) ToggleLoop() (Status, error) {
	return e.do(func() (Status, error) {
		if _, err := e.machine.ToggleLoop(); err != nil {
			return e.Status(), err
		}
		e.invalidatePrefetch()
		e.updateSnapshots()
		st := e.Status()
		e.bus.Publish(events.Loop, map[string]any{"loop": st.Loop})
		e.schedulePersist(true)
		return st, nil
	})
}

// SeekTo jumps to an absolute position in the current track.
func (e *Engine) SeekTo(posMs int64) (Status, error) {
	return e.do(func() (Status, error) { return e.seekLocked(posMs) })
}

// SeekBy jumps by a relative number of milliseconds (±5s, ±30s, ...).
func (e *Engine) SeekBy(deltaMs int64) (Status, error) {
	return e.do(func() (Status, error) {
		cur := e.posFrames.Load() * 1000 / int64(e.sampleRate)
		return e.seekLocked(cur + deltaMs)
	})
}

func (e *Engine) seekLocked(posMs int64) (Status, error) {
	if e.cur == nil || e.machine.State() == core.Stopped {
		return e.Status(), apperr.New(apperr.InvalidState, "no active track to seek")
	}
	if posMs < 0 {
		posMs = 0
	}
	target := posMs * int64(e.sampleRate) / 1000
	dur := e.durFrames.Load()
	if dur > 0 && target >= dur {
		// Seeking past the end behaves like a natural track transition.
		if !e.spliceToNext() {
			e.updateSnapshots()
			st := e.Status()
			e.bus.Publish(events.State, st)
			e.schedulePersist(true)
			return st, nil
		}
		e.updateSnapshots()
		st := e.Status()
		e.bus.Publish(events.State, st)
		e.schedulePersist(true)
		return st, nil
	}

	curFrames := e.posFrames.Load()
	if target >= curFrames {
		if err := e.cur.Skip(target - curFrames); err != nil {
			e.emitError(err)
			return e.Status(), err
		}
		e.posFrames.Store(target)
	} else {
		path := e.machine.CurrentTrack()
		tr, err := audio.Open(path, e.sampleRate)
		if err != nil {
			e.emitError(err)
			return e.Status(), err
		}
		if err := tr.Skip(target); err != nil {
			_ = tr.Close()
			e.emitError(err)
			return e.Status(), err
		}
		_ = e.cur.Close()
		e.cur = tr
		e.durFrames.Store(max(0, tr.TotalFrames()))
		e.out.Flush()
		e.posFrames.Store(target)
		if e.machine.State() == core.Playing {
			e.primeDevice()
			e.out.Resume()
		} else {
			e.out.Pause()
		}
	}
	e.pacer.Reset()
	e.updateSnapshots()
	st := e.Status()
	e.bus.Publish(events.State, st)
	e.schedulePersist(true)
	return st, nil
}

// PlayIndex jumps directly to the queue track at index and starts it.
func (e *Engine) PlayIndex(index int) (Status, error) {
	return e.do(func() (Status, error) {
		wasPaused := e.machine.State() == core.Paused
		eff, err := e.machine.SelectIndex(index)
		if err != nil {
			return e.Status(), err
		}
		if eff == core.EffectStart {
			if err := e.startCurrentTrack(); err != nil {
				e.emitError(err)
				return e.Status(), err
			}
			if wasPaused {
				e.out.Pause()
			}
		}
		e.updateSnapshots()
		st := e.Status()
		e.bus.Publish(events.Track, st)
		e.bus.Publish(events.State, st)
		e.schedulePersist(true)
		return st, nil
	})
}

// QueueRemove removes a track from the loaded queue.
func (e *Engine) QueueRemove(index int) (Status, error) {
	return e.do(func() (Status, error) {
		wasPaused := e.machine.State() == core.Paused
		eff, err := e.machine.RemoveTrack(index)
		if err != nil {
			return e.Status(), err
		}
		e.invalidatePrefetch()
		switch eff {
		case core.EffectStart:
			if err := e.startCurrentTrack(); err != nil {
				e.emitError(err)
				return e.Status(), err
			}
			if wasPaused {
				e.out.Pause()
			}
		case core.EffectStop:
			e.closePipeline()
			e.out.Flush()
			e.out.Pause()
			e.posFrames.Store(0)
			e.durFrames.Store(0)
		}
		e.updateSnapshots()
		st := e.Status()
		e.bus.Publish(events.Queue, e.Queue())
		e.bus.Publish(events.Track, st)
		e.bus.Publish(events.State, st)
		e.schedulePersist(true)
		return st, nil
	})
}

// QueueMove reorders a track within the loaded queue.
func (e *Engine) QueueMove(fromIndex, toIndex int) (Status, error) {
	return e.do(func() (Status, error) {
		if _, err := e.machine.MoveTrack(fromIndex, toIndex); err != nil {
			return e.Status(), err
		}
		e.invalidatePrefetch()
		e.updateSnapshots()
		st := e.Status()
		e.bus.Publish(events.Queue, e.Queue())
		e.bus.Publish(events.Track, st)
		e.schedulePersist(true)
		return st, nil
	})
}

// QueueClear empties the queue and stops playback.
func (e *Engine) QueueClear() (Status, error) {
	return e.do(func() (Status, error) {
		eff := e.machine.Clear()
		if eff == core.EffectStop {
			e.closePipeline()
			e.out.Flush()
			e.out.Pause()
		}
		e.posFrames.Store(0)
		e.durFrames.Store(0)
		e.updateSnapshots()
		st := e.Status()
		e.bus.Publish(events.Queue, e.Queue())
		e.bus.Publish(events.State, st)
		e.schedulePersist(true)
		return st, nil
	})
}

// QueueAppend appends files or directories to the loaded queue. Directories
// are expanded to their supported audio files. When nothing is loaded, the
// appended queue starts playing.
func (e *Engine) QueueAppend(paths []string) (Status, error) {
	var expanded []string
	for _, p := range paths {
		abs, err := config.NormalizePath(p, e.opts.MediaRoot)
		if err != nil {
			return e.Status(), err
		}
		info, err := os.Stat(abs)
		if err != nil {
			if os.IsNotExist(err) {
				return e.Status(), apperr.New(apperr.FileNotFound, "%s does not exist", abs)
			}
			return e.Status(), apperr.Wrap(apperr.FileNotFound, err, "stat %s", abs)
		}
		if info.IsDir() {
			files, err := media.Scan(abs, e.opts.MaxQueue)
			if err != nil {
				return e.Status(), err
			}
			expanded = append(expanded, files...)
			continue
		}
		if err := media.CheckAudioFile(abs); err != nil {
			return e.Status(), err
		}
		expanded = append(expanded, abs)
	}
	if len(expanded) == 0 {
		return e.Status(), apperr.New(apperr.FileNotFound, "no audio files to append")
	}
	snap := e.Queue()
	curCount := len(snap.Tracks)
	if e.opts.MaxQueue > 0 && curCount+len(expanded) > e.opts.MaxQueue {
		return e.Status(), apperr.New(apperr.InvalidCommand,
			"queue would exceed the maximum of %d tracks", e.opts.MaxQueue)
	}

	// Opening the first appended track before mutating state gives a clean
	// error when a file is corrupt and nothing was loaded yet.
	var first *audio.Track
	firstIdx := -1
	if curCount == 0 {
		for i, p := range expanded {
			tr, err := audio.Open(p, e.sampleRate)
			if err == nil {
				err = tr.Prebuffer(e.chunkFrames * 8)
				if err != nil {
					_ = tr.Close()
					tr = nil
				}
			}
			if err == nil {
				first = tr
				firstIdx = i
				break
			}
		}
		if first == nil {
			return e.Status(), apperr.New(apperr.DecodeError, "no playable tracks to append")
		}
	}

	return e.do(func() (Status, error) {
		eff, err := e.machine.AppendTracks(expanded)
		if err != nil {
			if first != nil {
				_ = first.Close()
			}
			return e.Status(), err
		}
		if eff == core.EffectStart {
			for i := 0; i < firstIdx; i++ {
				e.machine.Advance()
			}
			e.closePipeline()
			e.cur = first
			e.posFrames.Store(0)
			e.durFrames.Store(max(0, first.TotalFrames()))
			e.lastErr = ""
			e.out.Flush()
			e.primeDevice()
			e.out.Resume()
		} else if first != nil {
			_ = first.Close()
		}
		e.invalidatePrefetch()
		e.updateSnapshots()
		st := e.Status()
		e.bus.Publish(events.Queue, e.Queue())
		e.bus.Publish(events.Track, st)
		e.bus.Publish(events.State, st)
		e.schedulePersist(true)
		return st, nil
	})
}

// Browse lists a directory for the TUI file browser. An empty path starts at
// the configured media root (or the daemon's working directory).
func (e *Engine) Browse(path string) (BrowseResult, error) {
	base := strings.TrimSpace(path)
	if base == "" {
		base = e.opts.MediaRoot
		if base == "" {
			wd, err := os.Getwd()
			if err != nil {
				return BrowseResult{}, apperr.Wrap(apperr.Internal, err, "resolve working directory")
			}
			base = wd
		}
	}
	abs, err := config.NormalizePath(base, e.opts.MediaRoot)
	if err != nil {
		return BrowseResult{}, err
	}
	info, err := os.Stat(abs)
	if err != nil {
		if os.IsNotExist(err) {
			return BrowseResult{}, apperr.New(apperr.FileNotFound, "%s does not exist", abs)
		}
		return BrowseResult{}, apperr.Wrap(apperr.FileNotFound, err, "stat %s", abs)
	}
	if !info.IsDir() {
		return BrowseResult{}, apperr.New(apperr.InvalidCommand, "%s is not a directory", abs)
	}
	entries, err := os.ReadDir(abs)
	if err != nil {
		return BrowseResult{}, apperr.Wrap(apperr.FileNotFound, err, "read %s", abs)
	}

	root := ""
	if e.opts.MediaRoot != "" {
		if r, err := config.NormalizePath(e.opts.MediaRoot, ""); err == nil {
			root = r
		}
	}
	result := BrowseResult{Path: abs, Root: root, Entries: make([]BrowseEntry, 0, len(entries))}
	parent := filepath.Dir(abs)
	if root != "" && (abs == root || !strings.HasPrefix(parent+string(filepath.Separator), root+string(filepath.Separator))) {
		parent = ""
	} else if abs == string(filepath.Separator) {
		parent = ""
	}
	result.Parent = parent

	for _, ent := range entries {
		name := ent.Name()
		if strings.HasPrefix(name, ".") {
			continue
		}
		full := filepath.Join(abs, name)
		if ent.IsDir() {
			result.Entries = append(result.Entries, BrowseEntry{Name: name, Path: full, IsDir: true})
			continue
		}
		if ent.Type()&os.ModeSymlink != 0 {
			// Resolve symlinks through the same media-root validation used by
			// playback so the browser cannot list files outside the root.
			resolved, err := config.NormalizePath(full, e.opts.MediaRoot)
			if err != nil {
				continue
			}
			st, err := os.Stat(resolved)
			if err != nil {
				continue
			}
			if st.IsDir() {
				result.Entries = append(result.Entries, BrowseEntry{Name: name, Path: resolved, IsDir: true})
				continue
			}
			if !media.IsAudio(name) {
				continue
			}
			result.Entries = append(result.Entries, BrowseEntry{
				Name:     name,
				Path:     resolved,
				Audio:    true,
				Size:     st.Size(),
				Metadata: e.metadataFor(resolved),
			})
			continue
		}
		if !media.IsAudio(name) {
			continue
		}
		st, err := ent.Info()
		var size int64
		if err == nil {
			size = st.Size()
		}
		result.Entries = append(result.Entries, BrowseEntry{
			Name:     name,
			Path:     full,
			Audio:    true,
			Size:     size,
			Metadata: e.metadataFor(full),
		})
	}
	sort.SliceStable(result.Entries, func(i, j int) bool {
		a, b := result.Entries[i], result.Entries[j]
		if a.IsDir != b.IsDir {
			return a.IsDir
		}
		return media.NaturalLess(a.Name, b.Name)
	})
	return result, nil
}

// PlaylistList lists saved playlists from the database.
func (e *Engine) PlaylistList() ([]PlaylistInfo, error) {
	if e.store == nil {
		return []PlaylistInfo{}, nil
	}
	list, err := e.store.ListPlaylists()
	if err != nil {
		return nil, err
	}
	out := make([]PlaylistInfo, 0, len(list))
	for _, p := range list {
		out = append(out, PlaylistInfo{
			Name:       p.Name,
			SourcePath: p.SourcePath,
			TrackCount: p.TrackCount,
			UpdatedAt:  p.UpdatedAt,
		})
	}
	return out, nil
}

// PlaylistInspect returns a stored playlist and its tracks.
func (e *Engine) PlaylistInspect(target string) (PlaylistView, error) {
	target = strings.TrimSpace(target)
	if target == "" {
		return PlaylistView{}, apperr.New(apperr.InvalidCommand, "missing playlist path or alias")
	}
	if e.store == nil {
		return PlaylistView{}, apperr.New(apperr.DBError, "database is not configured")
	}
	isAlias := store.ValidAliasName(target) &&
		!strings.ContainsRune(target, '/') &&
		!strings.ContainsRune(target, filepath.Separator) &&
		!strings.HasSuffix(strings.ToLower(target), ".json")
	if isAlias {
		a, err := e.store.GetAlias(target)
		if err != nil {
			return PlaylistView{}, err
		}
		target = a.PlaylistPath
	}
	if e.opts.MediaRoot != "" {
		norm, err := config.NormalizePath(target, e.opts.MediaRoot)
		if err != nil {
			return PlaylistView{}, err
		}
		target = norm
	}
	name, tracks, err := e.store.PlaylistTracks(target)
	if err != nil {
		return PlaylistView{}, err
	}
	if len(tracks) == 0 {
		if pl, lerr := playlist.Load(target, e.opts.MaxQueue); lerr == nil {
			name, tracks = pl.Name, pl.Tracks
		} else {
			return PlaylistView{}, apperr.New(apperr.FileNotFound, "playlist %s is not saved", target)
		}
	}
	view := PlaylistView{PlaylistInfo: PlaylistInfo{Name: name, SourcePath: target, TrackCount: len(tracks)}}
	view.Tracks = make([]QueueItem, len(tracks))
	for i, p := range tracks {
		view.Tracks[i] = QueueItem{Index: i, Path: p, Name: filepath.Base(p), Metadata: e.metadataFor(p)}
	}
	return view, nil
}

// PlaylistRemove deletes a saved playlist by source path or alias.
func (e *Engine) PlaylistRemove(target string) error {
	if e.store == nil {
		return apperr.New(apperr.DBError, "database is not configured")
	}
	target = strings.TrimSpace(target)
	if target == "" {
		return apperr.New(apperr.InvalidCommand, "missing playlist path or alias")
	}
	if store.ValidAliasName(target) && !strings.ContainsRune(target, '/') {
		if _, err := e.store.GetAlias(target); err == nil {
			return e.store.RemoveAlias(target)
		}
	}
	return e.store.DeletePlaylist(target)
}

// PlaylistCreate writes either a template playlist.json (when dir is empty) or
// a scanned directory playlist.
func (e *Engine) PlaylistCreate(dir, output string, force bool) (string, error) {
	var outPath string
	var err error
	if strings.TrimSpace(dir) == "" {
		target := output
		if target == "" {
			target = "playlist.json"
		}
		outPath, err = config.NormalizePath(target, e.opts.MediaRoot)
		if err != nil {
			return "", err
		}
		if err := playlist.Write(playlist.Template(), outPath, force); err != nil {
			return "", err
		}
		return outPath, nil
	}

	dirAbs, err := config.NormalizePath(dir, e.opts.MediaRoot)
	if err != nil {
		return "", err
	}
	target := output
	if target == "" {
		target = filepath.Join(dirAbs, "playlist.json")
	}
	outPath, err = config.NormalizePath(target, e.opts.MediaRoot)
	if err != nil {
		return "", err
	}
	pl, err := playlist.FromDir(dirAbs, outPath, e.opts.MaxQueue)
	if err != nil {
		return "", err
	}
	if err := playlist.Write(pl, outPath, force); err != nil {
		return "", err
	}
	return outPath, nil
}

// PlaylistPlay loads and plays a JSON playlist by path or saved alias.
func (e *Engine) PlaylistPlay(target string, loop bool) (Status, error) {
	target = strings.TrimSpace(target)
	if target == "" {
		return e.Status(), apperr.New(apperr.InvalidCommand, "missing playlist path or alias")
	}

	var plPath string
	isAlias := false
	if e.store != nil && store.ValidAliasName(target) && !strings.HasSuffix(strings.ToLower(target), ".json") {
		if a, err := e.store.GetAlias(target); err == nil {
			plPath = a.PlaylistPath
			isAlias = true
		} else if !strings.ContainsRune(target, '/') && !strings.ContainsRune(target, filepath.Separator) {
			if _, statErr := os.Stat(target); statErr != nil {
				return e.Status(), err
			}
		}
	}
	if plPath == "" {
		var err error
		plPath, err = config.NormalizePath(target, e.opts.MediaRoot)
		if err != nil {
			return e.Status(), err
		}
	} else if e.opts.MediaRoot != "" {
		var err error
		plPath, err = config.NormalizePath(plPath, e.opts.MediaRoot)
		if err != nil {
			return e.Status(), err
		}
	}

	loaded, err := playlist.Load(plPath, e.opts.MaxQueue)
	if err != nil {
		if isAlias && apperr.Is(err, apperr.FileNotFound) {
			return e.Status(), apperr.New(apperr.AliasNotFound,
				"alias %q points to missing playlist %s", target, plPath)
		}
		return e.Status(), err
	}
	if e.opts.MediaRoot != "" {
		for i, tr := range loaded.Tracks {
			if _, err := config.NormalizePath(tr, e.opts.MediaRoot); err != nil {
				return e.Status(), apperr.New(apperr.Forbidden, "tracks[%d] is outside media root", i)
			}
		}
	}
	src := core.Source{
		Type:   core.Playlist,
		Path:   loaded.Source,
		Name:   loaded.Name,
		Tracks: loaded.Tracks,
	}
	return e.loadAndStart(src, loop, true)
}

// AliasSet saves or updates an alias after validating the target playlist.
func (e *Engine) AliasSet(name, playlistPath string) (store.Alias, error) {
	if e.store == nil {
		return store.Alias{}, apperr.New(apperr.DBError, "database is not configured")
	}
	if !store.ValidAliasName(name) {
		return store.Alias{}, apperr.New(apperr.InvalidCommand, "invalid alias name %q", name)
	}
	abs, err := config.NormalizePath(playlistPath, e.opts.MediaRoot)
	if err != nil {
		return store.Alias{}, err
	}
	if _, err := playlist.Load(abs, e.opts.MaxQueue); err != nil {
		return store.Alias{}, err
	}
	return e.store.SetAlias(name, abs)
}

// AliasList lists all saved aliases.
func (e *Engine) AliasList() ([]store.Alias, error) {
	if e.store == nil {
		return []store.Alias{}, nil
	}
	return e.store.ListAliases()
}

// AliasRemove deletes an alias.
func (e *Engine) AliasRemove(name string) error {
	if e.store == nil {
		return apperr.New(apperr.DBError, "database is not configured")
	}
	return e.store.RemoveAlias(name)
}

// OnDeviceChange pauses playback when the host audio output device changes and
// emits a device event. It never auto-resumes.
func (e *Engine) OnDeviceChange(reason string) {
	_, _ = e.do(func() (Status, error) {
		paused := e.machine.ForcePause()
		if paused {
			e.out.Pause()
			e.updateSnapshots()
		}
		st := e.Status()
		e.bus.Publish(events.Device, map[string]any{
			"reason": reason,
			"paused": paused,
			"state":  st.State,
		})
		if paused {
			e.bus.Publish(events.State, st)
			e.schedulePersist(true)
		}
		return st, nil
	})
}

// Close shuts down the engine, flushes the final playback state to SQLite, and
// releases the audio sink and single-instance lock.
func (e *Engine) Close() error {
	e.closeOnce.Do(func() {
		e.bus.Publish(events.Shutdown, map[string]any{"status": "shutting_down"})
		pb := e.playbackRow()
		close(e.stopCh)
		<-e.loopDone
		<-e.persistDone
		if e.store != nil {
			_ = e.store.SavePlayback(pb)
		}
		_ = e.out.Close()
		_ = e.lock.Release()
	})
	return nil
}
