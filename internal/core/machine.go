// Package core implements the pure playback state machine. It performs no
// I/O; the engine applies its effects to decoders and sinks.
package core

import (
	"math/rand/v2"
	"path/filepath"

	"github.com/frdn/eklips/internal/apperr"
)

// State is the playback state.
type State string

// Playback states.
const (
	Stopped State = "stopped"
	Playing State = "playing"
	Paused  State = "paused"
)

// SourceType is the kind of loaded source.
type SourceType string

// Source types. None means nothing is loaded.
const (
	None     SourceType = ""
	Single   SourceType = "single"
	Playlist SourceType = "playlist"
)

// VolumeStep is the increment used by VolumeUp/VolumeDown.
const VolumeStep = 5

// DefaultVolume is the volume used on first start.
const DefaultVolume = 80

// Source describes the loaded media.
type Source struct {
	Type   SourceType
	Path   string   // file, directory, or playlist JSON path
	Name   string   // display name
	Tracks []string // absolute track paths, in source order
}

// Effect tells the engine what to do with the audio pipeline.
type Effect int

// Effects returned by transitions.
const (
	EffectNone   Effect = iota
	EffectStart         // (re)start the track at the cursor from the beginning
	EffectPause         // pause output, keep decoder position
	EffectResume        // resume output from the paused position
	EffectStop          // stop output and release the decoder
)

func (e Effect) String() string {
	switch e {
	case EffectStart:
		return "start"
	case EffectPause:
		return "pause"
	case EffectResume:
		return "resume"
	case EffectStop:
		return "stop"
	}
	return "none"
}

// Machine is the playback state machine. It is not safe for concurrent use;
// the engine serialises access through its actor loop.
type Machine struct {
	state   State
	src     Source
	order   []int // play order: indices into src.Tracks
	cursor  int   // position in order
	volume  int
	muted   bool
	shuffle bool
	loop    bool
	gen     uint64 // bumped on any change affecting "what plays next"
	rng     *rand.Rand
}

// NewMachine returns a machine with nothing loaded.
func NewMachine(seed uint64) *Machine {
	return &Machine{
		state:  Stopped,
		volume: DefaultVolume,
		rng:    rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15)),
	}
}

// Snapshot is an immutable view of machine state.
type Snapshot struct {
	State      State
	Source     Source
	Order      []int
	Cursor     int
	TrackIndex int // index into Source.Tracks, -1 if none
	Volume     int
	Muted      bool
	Shuffle    bool
	Loop       bool
	Gen        uint64
}

// Snapshot returns a copy of the current state. Slices are shared but never
// mutated in place by the machine (they are replaced on change).
func (m *Machine) Snapshot() Snapshot {
	return Snapshot{
		State:      m.state,
		Source:     m.src,
		Order:      m.order,
		Cursor:     m.cursor,
		TrackIndex: m.TrackIndex(),
		Volume:     m.volume,
		Muted:      m.muted,
		Shuffle:    m.shuffle,
		Loop:       m.loop,
		Gen:        m.gen,
	}
}

// State returns the playback state.
func (m *Machine) State() State { return m.state }

// Loaded reports whether a source is loaded.
func (m *Machine) Loaded() bool { return m.src.Type != None && len(m.src.Tracks) > 0 }

// Gen returns the queue generation.
func (m *Machine) Gen() uint64 { return m.gen }

// Volume returns the configured volume (0..100).
func (m *Machine) Volume() int { return m.volume }

// Muted reports whether output is muted.
func (m *Machine) Muted() bool { return m.muted }

// TrackIndex returns the index into Source.Tracks of the current track, or -1.
func (m *Machine) TrackIndex() int {
	if !m.Loaded() || m.cursor < 0 || m.cursor >= len(m.order) {
		return -1
	}
	return m.order[m.cursor]
}

// CurrentTrack returns the path of the current track, or "".
func (m *Machine) CurrentTrack() string {
	i := m.TrackIndex()
	if i < 0 {
		return ""
	}
	return m.src.Tracks[i]
}

// Load replaces the source and starts playback from the first track.
// Shuffle is reset; loop is set from the argument.
func (m *Machine) Load(src Source, loop bool) (Effect, error) {
	if src.Type == None || len(src.Tracks) == 0 {
		return EffectNone, apperr.New(apperr.NoMediaLoaded, "source has no tracks")
	}
	if src.Name == "" {
		src.Name = filepath.Base(src.Path)
	}
	m.src = src
	m.order = identity(len(src.Tracks))
	m.cursor = 0
	m.shuffle = false
	m.loop = loop
	m.state = Playing
	m.gen++
	return EffectStart, nil
}

// Play resumes the loaded source.
func (m *Machine) Play() (Effect, error) {
	if !m.Loaded() {
		return EffectNone, apperr.New(apperr.NoMediaLoaded, "nothing is loaded; use play <path>")
	}
	switch m.state {
	case Playing:
		return EffectNone, nil
	case Paused:
		m.state = Playing
		return EffectResume, nil
	default:
		m.state = Playing
		return EffectStart, nil
	}
}

// Pause pauses playback.
func (m *Machine) Pause() (Effect, error) {
	if !m.Loaded() {
		return EffectNone, apperr.New(apperr.NoMediaLoaded, "nothing is loaded")
	}
	if m.state != Playing {
		return EffectNone, apperr.New(apperr.InvalidState, "cannot pause while %s", m.state)
	}
	m.state = Paused
	return EffectPause, nil
}

// Stop stops playback, keeping the queue loaded.
func (m *Machine) Stop() (Effect, error) {
	if !m.Loaded() {
		return EffectNone, apperr.New(apperr.NoMediaLoaded, "nothing is loaded")
	}
	if m.state == Stopped {
		return EffectNone, apperr.New(apperr.InvalidState, "already stopped")
	}
	m.state = Stopped
	return EffectStop, nil
}

func (m *Machine) requirePlaylistActive() error {
	if !m.Loaded() {
		return apperr.New(apperr.NoMediaLoaded, "nothing is loaded")
	}
	if m.src.Type != Playlist {
		return apperr.New(apperr.PlaylistRequired, "a playlist must be loaded")
	}
	if m.state == Stopped {
		return apperr.New(apperr.InvalidState, "playback is stopped")
	}
	return nil
}

// Next moves to the next track in play order.
func (m *Machine) Next() (Effect, error) {
	if err := m.requirePlaylistActive(); err != nil {
		return EffectNone, err
	}
	next := m.nextCursor()
	if next < 0 {
		return EffectNone, apperr.New(apperr.InvalidState, "end of playlist")
	}
	m.cursor = next
	m.gen++
	return m.restartEffect(), nil
}

// Previous moves to the previous track; at the first track without loop it
// restarts the current track.
func (m *Machine) Previous() (Effect, error) {
	if err := m.requirePlaylistActive(); err != nil {
		return EffectNone, err
	}
	switch {
	case m.cursor > 0:
		m.cursor--
	case m.loop:
		m.cursor = len(m.order) - 1
	}
	m.gen++
	return m.restartEffect(), nil
}

// restartEffect starts the new track; a paused machine stays paused but the
// pipeline is reloaded (EffectStart is applied, then paused by the engine).
func (m *Machine) restartEffect() Effect { return EffectStart }

// VolumeUp raises volume by VolumeStep.
func (m *Machine) VolumeUp() (Effect, error) {
	if !m.Loaded() {
		return EffectNone, apperr.New(apperr.NoMediaLoaded, "nothing is loaded")
	}
	if m.volume >= 100 {
		return EffectNone, apperr.New(apperr.VolumeMax, "volume is already 100")
	}
	m.volume = min(100, m.volume+VolumeStep)
	return EffectNone, nil
}

// VolumeDown lowers volume by VolumeStep.
func (m *Machine) VolumeDown() (Effect, error) {
	if !m.Loaded() {
		return EffectNone, apperr.New(apperr.NoMediaLoaded, "nothing is loaded")
	}
	if m.volume <= 0 {
		return EffectNone, apperr.New(apperr.VolumeMin, "volume is already 0")
	}
	m.volume = max(0, m.volume-VolumeStep)
	return EffectNone, nil
}

// ToggleMute toggles mute; the volume is retained.
func (m *Machine) ToggleMute() (Effect, error) {
	if !m.Loaded() {
		return EffectNone, apperr.New(apperr.NoMediaLoaded, "nothing is loaded")
	}
	m.muted = !m.muted
	return EffectNone, nil
}

// ToggleShuffle toggles shuffle without restarting the current track.
func (m *Machine) ToggleShuffle() (Effect, error) {
	if !m.Loaded() {
		return EffectNone, apperr.New(apperr.NoMediaLoaded, "nothing is loaded")
	}
	if m.src.Type != Playlist {
		return EffectNone, apperr.New(apperr.PlaylistRequired, "shuffle requires a playlist")
	}
	cur := m.TrackIndex()
	m.shuffle = !m.shuffle
	if m.shuffle {
		m.order = m.shuffledOrder(cur)
		m.cursor = 0
	} else {
		m.order = identity(len(m.src.Tracks))
		m.cursor = cur
	}
	m.gen++
	return EffectNone, nil
}

// ToggleLoop toggles loop.
func (m *Machine) ToggleLoop() (Effect, error) {
	if !m.Loaded() {
		return EffectNone, apperr.New(apperr.NoMediaLoaded, "nothing is loaded")
	}
	if m.src.Type != Playlist {
		return EffectNone, apperr.New(apperr.PlaylistRequired, "loop requires a playlist")
	}
	m.loop = !m.loop
	m.gen++
	return EffectNone, nil
}

// PeekNext returns the track index that would play after the current one, or
// -1 when the queue ends. It does not mutate state.
func (m *Machine) PeekNext() int {
	c := m.nextCursor()
	if c < 0 {
		return -1
	}
	return m.order[c]
}

// PeekNextFrom returns the track index following cursor position c.
func (m *Machine) peekCursorAfter(c int) int {
	if c+1 < len(m.order) {
		return c + 1
	}
	if m.loop {
		return 0
	}
	return -1
}

func (m *Machine) nextCursor() int {
	if !m.Loaded() {
		return -1
	}
	if m.src.Type == Single {
		if m.loop {
			return m.cursor
		}
		return -1
	}
	return m.peekCursorAfter(m.cursor)
}

// Advance commits a natural track transition (end of track or skip of a
// broken track). It returns false when the queue ended; the machine is then
// stopped with the cursor reset to the first track.
func (m *Machine) Advance() bool {
	next := m.nextCursor()
	if next < 0 {
		m.state = Stopped
		m.cursor = 0
		m.gen++
		return false
	}
	m.cursor = next
	m.gen++
	return true
}

// SelectIndex makes the track at index in Source.Tracks current and returns
// EffectStart. Selecting a track while stopped begins playback.
func (m *Machine) SelectIndex(index int) (Effect, error) {
	if !m.Loaded() {
		return EffectNone, apperr.New(apperr.NoMediaLoaded, "nothing is loaded")
	}
	if index < 0 || index >= len(m.src.Tracks) {
		return EffectNone, apperr.New(apperr.InvalidCommand, "track index %d is out of range", index)
	}
	if m.state == Stopped {
		m.state = Playing
	}
	for i, ti := range m.order {
		if ti == index {
			m.cursor = i
			m.gen++
			return EffectStart, nil
		}
	}
	// Defensive fallback: repair the order and select the requested track.
	m.order = identity(len(m.src.Tracks))
	m.cursor = index
	m.gen++
	return EffectStart, nil
}

// RemoveTrack removes the track at index from the queue. When a non-current
// track is removed, playback continues uninterrupted. Removing the current
// track selects the following track (or clears an empty queue).
func (m *Machine) RemoveTrack(index int) (Effect, error) {
	if !m.Loaded() {
		return EffectNone, apperr.New(apperr.NoMediaLoaded, "nothing is loaded")
	}
	if index < 0 || index >= len(m.src.Tracks) {
		return EffectNone, apperr.New(apperr.InvalidCommand, "track index %d is out of range", index)
	}
	oldCurrent := m.TrackIndex()
	wasStopped := m.state == Stopped

	tracks := make([]string, 0, len(m.src.Tracks)-1)
	for i, p := range m.src.Tracks {
		if i != index {
			tracks = append(tracks, p)
		}
	}
	if len(tracks) == 0 {
		m.src = Source{}
		m.order = nil
		m.cursor = 0
		m.state = Stopped
		m.gen++
		return EffectStop, nil
	}

	order := make([]int, 0, len(m.order)-1)
	for _, ti := range m.order {
		if ti == index {
			continue
		}
		if ti > index {
			ti--
		}
		order = append(order, ti)
	}
	m.src.Tracks = tracks
	m.order = order
	m.gen++

	if oldCurrent == index {
		if m.cursor >= len(order) {
			m.cursor = len(order) - 1
		}
		if m.cursor < 0 {
			m.cursor = 0
		}
		if wasStopped {
			m.state = Stopped
			return EffectNone, nil
		}
		return EffectStart, nil
	}
	cur := oldCurrent
	if cur > index {
		cur--
	}
	m.cursor = indexOf(order, cur)
	if m.cursor < 0 {
		m.cursor = 0
	}
	return EffectNone, nil
}

// MoveTrack moves the track at fromIndex so that it ends up at toIndex.
// Playback of the current track is preserved.
func (m *Machine) MoveTrack(fromIndex, toIndex int) (Effect, error) {
	if !m.Loaded() {
		return EffectNone, apperr.New(apperr.NoMediaLoaded, "nothing is loaded")
	}
	n := len(m.src.Tracks)
	if fromIndex < 0 || fromIndex >= n || toIndex < 0 || toIndex >= n {
		return EffectNone, apperr.New(apperr.InvalidCommand, "track indices %d -> %d are out of range", fromIndex, toIndex)
	}
	if fromIndex == toIndex {
		return EffectNone, nil
	}
	current := m.TrackIndex()

	tracks := append([]string(nil), m.src.Tracks...)
	moved := tracks[fromIndex]
	tracks = append(tracks[:fromIndex], tracks[fromIndex+1:]...)
	tracks = append(tracks, "")
	copy(tracks[toIndex+1:], tracks[toIndex:])
	tracks[toIndex] = moved

	// Map old track indices to their new positions.
	mapping := make([]int, n)
	for old := range mapping {
		switch {
		case old == fromIndex:
			mapping[old] = toIndex
		case fromIndex < toIndex && old > fromIndex && old <= toIndex:
			mapping[old] = old - 1
		case toIndex < fromIndex && old >= toIndex && old < fromIndex:
			mapping[old] = old + 1
		default:
			mapping[old] = old
		}
	}
	order := make([]int, len(m.order))
	for i, ti := range m.order {
		order[i] = mapping[ti]
	}
	m.src.Tracks = tracks
	m.order = order
	if current >= 0 {
		m.cursor = indexOf(order, mapping[current])
		if m.cursor < 0 {
			m.cursor = 0
		}
	}
	if !m.shuffle {
		m.order = identity(n)
		m.cursor = mapping[current]
	}
	m.gen++
	return EffectNone, nil
}

// Clear empties the queue and stops playback.
func (m *Machine) Clear() Effect {
	won := m.state != Stopped || m.Loaded()
	m.src = Source{}
	m.order = nil
	m.cursor = 0
	m.state = Stopped
	m.gen++
	if won {
		return EffectStop
	}
	return EffectNone
}

// AppendTracks appends paths to the loaded queue. A single-file source becomes
// a playlist queue. It returns EffectStart when nothing was previously loaded.
func (m *Machine) AppendTracks(paths []string) (Effect, error) {
	if len(paths) == 0 {
		return EffectNone, nil
	}
	if !m.Loaded() {
		m.src = Source{Type: Playlist, Name: "Queue", Tracks: append([]string(nil), paths...)}
		m.order = identity(len(paths))
		m.cursor = 0
		m.state = Playing
		m.gen++
		return EffectStart, nil
	}
	if m.src.Type == Single {
		m.src.Type = Playlist
	}
	start := len(m.src.Tracks)
	m.src.Tracks = append(m.src.Tracks, paths...)
	if m.shuffle {
		rest := make([]int, len(paths))
		for i := range rest {
			rest[i] = start + i
		}
		m.rng.Shuffle(len(rest), func(i, j int) { rest[i], rest[j] = rest[j], rest[i] })
		m.order = append(m.order, rest...)
	} else {
		m.order = append(m.order, identity(len(paths))...)
		for i := range m.order[start:] {
			m.order[start+i] += start
		}
	}
	m.gen++
	return EffectNone, nil
}

func indexOf(s []int, v int) int {
	for i, x := range s {
		if x == v {
			return i
		}
	}
	return -1
}

// ForcePause pauses if playing (used for device changes). It reports whether
// the state changed.
func (m *Machine) ForcePause() bool {
	if m.state != Playing {
		return false
	}
	m.state = Paused
	return true
}

// ForceStop stops playback regardless of state (used on fatal decode errors).
func (m *Machine) ForceStop() {
	if m.state != Stopped {
		m.state = Stopped
	}
}

// RestoreParams describes persisted state.
type RestoreParams struct {
	Source  Source
	Index   int // track index into Source.Tracks
	State   State
	Volume  int
	Muted   bool
	Shuffle bool
	Loop    bool
}

// Restore loads persisted state. The caller decides whether to keep the
// stored state or force Stopped.
func (m *Machine) Restore(p RestoreParams) {
	m.volume = clamp(p.Volume, 0, 100)
	m.muted = p.Muted
	if p.Source.Type == None || len(p.Source.Tracks) == 0 {
		m.src = Source{}
		m.order = nil
		m.cursor = 0
		m.state = Stopped
		m.shuffle, m.loop = false, false
		m.gen++
		return
	}
	m.src = p.Source
	idx := clamp(p.Index, 0, len(p.Source.Tracks)-1)
	m.loop = p.Loop
	m.shuffle = p.Shuffle && p.Source.Type == Playlist
	if m.shuffle {
		m.order = m.shuffledOrder(idx)
		m.cursor = 0
	} else {
		m.order = identity(len(p.Source.Tracks))
		m.cursor = idx
	}
	if p.State == Paused || p.State == Playing {
		m.state = p.State
	} else {
		m.state = Stopped
	}
	m.gen++
}

func (m *Machine) shuffledOrder(first int) []int {
	n := len(m.src.Tracks)
	order := make([]int, 0, n)
	if first >= 0 {
		order = append(order, first)
	}
	rest := make([]int, 0, n)
	for i := 0; i < n; i++ {
		if i != first {
			rest = append(rest, i)
		}
	}
	m.rng.Shuffle(len(rest), func(i, j int) { rest[i], rest[j] = rest[j], rest[i] })
	return append(order, rest...)
}

func identity(n int) []int {
	o := make([]int, n)
	for i := range o {
		o[i] = i
	}
	return o
}

func clamp(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
