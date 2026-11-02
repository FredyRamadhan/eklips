package tui

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/key"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/frdn/eklips/internal/apperr"
	"github.com/frdn/eklips/internal/audio"
	"github.com/frdn/eklips/internal/client"
	"github.com/frdn/eklips/internal/core"
	"github.com/frdn/eklips/internal/engine"
	"github.com/frdn/eklips/internal/events"
	"github.com/frdn/eklips/internal/store"
)

const (
	progressTickInterval = 250 * time.Millisecond // 4 Hz local interpolation
	levelTickInterval    = 125 * time.Millisecond // 8 Hz VU meter
	minReconnectBackoff  = 150 * time.Millisecond
	maxReconnectBackoff  = 3 * time.Second
)

type viewID int

const (
	viewQueue viewID = iota
	viewBrowser
	viewPlaylists
)

type inputMode int

const (
	inputNone inputMode = iota
	inputQueueFilter
	inputBrowserFilter
	inputCommand
)

type confirmKind int

const (
	confirmNone confirmKind = iota
	confirmQuitDaemon
	confirmClearQueue
	confirmDeletePlaylist
)

type playlistRow struct {
	Kind       string // "playlist" or "alias"
	Name       string
	Target     string
	TrackCount int
	Updated    time.Time
}

type confirmState struct {
	kind   confirmKind
	target string
	label  string
}

// keyMap holds the keybindings shown in the help footer.
type keyMap struct {
	PlayPause    key.Binding
	Stop         key.Binding
	Next         key.Binding
	Prev         key.Binding
	VolUp        key.Binding
	VolDown      key.Binding
	Mute         key.Binding
	Shuffle      key.Binding
	Loop         key.Binding
	Up           key.Binding
	Down         key.Binding
	Top          key.Binding
	Bottom       key.Binding
	SeekForward  key.Binding
	SeekBack     key.Binding
	SeekForwardL key.Binding
	SeekBackL    key.Binding
	Enter        key.Binding
	Remove       key.Binding
	MoveUp       key.Binding
	MoveDown     key.Binding
	Clear        key.Binding
	Tab          key.Binding
	View1        key.Binding
	View2        key.Binding
	View3        key.Binding
	Filter       key.Binding
	Command      key.Binding
	Back         key.Binding
	Append       key.Binding
	PlayDir      key.Binding
	Help         key.Binding
	QuitTUI      key.Binding
	QuitDaemon   key.Binding
}

var defaultKeys = keyMap{
	PlayPause:    key.NewBinding(key.WithKeys(" "), key.WithHelp("space", "play/pause")),
	Stop:         key.NewBinding(key.WithKeys("s"), key.WithHelp("s", "stop")),
	Next:         key.NewBinding(key.WithKeys("n"), key.WithHelp("n", "next")),
	Prev:         key.NewBinding(key.WithKeys("p"), key.WithHelp("p", "prev")),
	VolUp:        key.NewBinding(key.WithKeys("+", "="), key.WithHelp("+", "vol+")),
	VolDown:      key.NewBinding(key.WithKeys("-", "_"), key.WithHelp("-", "vol-")),
	Mute:         key.NewBinding(key.WithKeys("m"), key.WithHelp("m", "mute")),
	Shuffle:      key.NewBinding(key.WithKeys("r"), key.WithHelp("r", "shuffle")),
	Loop:         key.NewBinding(key.WithKeys("L"), key.WithHelp("L", "loop")),
	Up:           key.NewBinding(key.WithKeys("up", "k"), key.WithHelp("↑/k", "up")),
	Down:         key.NewBinding(key.WithKeys("down", "j"), key.WithHelp("↓/j", "down")),
	Top:          key.NewBinding(key.WithKeys("g"), key.WithHelp("g", "top")),
	Bottom:       key.NewBinding(key.WithKeys("G"), key.WithHelp("G", "bottom")),
	SeekForward:  key.NewBinding(key.WithKeys("l", "right"), key.WithHelp("l/→", "seek +5s")),
	SeekBack:     key.NewBinding(key.WithKeys("h", "left"), key.WithHelp("h/←", "seek -5s")),
	SeekForwardL: key.NewBinding(key.WithKeys("]"), key.WithHelp("]", "seek +30s")),
	SeekBackL:    key.NewBinding(key.WithKeys("["), key.WithHelp("[", "seek -30s")),
	Enter:        key.NewBinding(key.WithKeys("enter"), key.WithHelp("Enter", "play")),
	Remove:       key.NewBinding(key.WithKeys("x", "d"), key.WithHelp("x/d", "remove")),
	MoveUp:       key.NewBinding(key.WithKeys("K"), key.WithHelp("K", "move up")),
	MoveDown:     key.NewBinding(key.WithKeys("J"), key.WithHelp("J", "move down")),
	Clear:        key.NewBinding(key.WithKeys("C"), key.WithHelp("C", "clear")),
	Tab:          key.NewBinding(key.WithKeys("tab"), key.WithHelp("Tab", "next view")),
	View1:        key.NewBinding(key.WithKeys("1"), key.WithHelp("1", "queue")),
	View2:        key.NewBinding(key.WithKeys("2"), key.WithHelp("2", "files")),
	View3:        key.NewBinding(key.WithKeys("3"), key.WithHelp("3", "playlists")),
	Filter:       key.NewBinding(key.WithKeys("/"), key.WithHelp("/", "filter")),
	Command:      key.NewBinding(key.WithKeys(":"), key.WithHelp(":", "command")),
	Back:         key.NewBinding(key.WithKeys("backspace", "h", "left"), key.WithHelp("⌫/h", "parent")),
	Append:       key.NewBinding(key.WithKeys("a"), key.WithHelp("a", "append")),
	PlayDir:      key.NewBinding(key.WithKeys("p"), key.WithHelp("p", "play folder")),
	Help:         key.NewBinding(key.WithKeys("?"), key.WithHelp("?", "help")),
	QuitTUI:      key.NewBinding(key.WithKeys("q", "esc", "ctrl+c"), key.WithHelp("q/Esc", "quit TUI")),
	QuitDaemon:   key.NewBinding(key.WithKeys("Q"), key.WithHelp("Q", "quit daemon")),
}

// Messages processed by the Bubble Tea model.
type (
	connectedMsg struct {
		status engine.Status
		queue  engine.Queue
		events <-chan client.SSEEvent
	}
	sseEventMsg struct {
		event  client.SSEEvent
		events <-chan client.SSEEvent
	}
	disconnectedMsg struct {
		err error
	}
	reconnectTickMsg struct{}
	progressTickMsg  time.Time
	levelTickMsg     struct{}
	levelsMsg        struct {
		levels engine.Levels
		err    error
	}
	browseResultMsg struct {
		result engine.BrowseResult
		err    error
	}
	playlistsResultMsg struct {
		playlists []engine.PlaylistInfo
		aliases   []store.Alias
		err       error
	}
	playlistInspectMsg struct {
		target string
		view   engine.PlaylistView
		err    error
	}
	actionResultMsg struct {
		status *engine.Status
		queue  *engine.Queue
		err    error
		quit   bool
	}
)

// Model is the Bubble Tea state model for the TUI client. It holds no playback
// engine and owns only presentation state.
type Model struct {
	cl     *client.Client
	ctx    context.Context
	cancel context.CancelFunc
	styles styles
	keys   keyMap
	width  int
	height int

	connected     bool
	status        engine.Status
	queue         engine.Queue
	errBanner     string
	infoBanner    string
	confirm       confirmState
	showFullHelp  bool
	ticking       bool
	levelTicking  bool
	lastSyncTime  time.Time
	lastSyncPosMs int64
	lastEventID   uint64
	backoff       time.Duration
	QuitDaemonReq bool

	view        viewID
	cursor      int // absolute index into queue.Tracks
	queueTop    int
	queueFilter string

	inputMode inputMode
	inputText string
	inputPrev string

	browser       engine.BrowseResult
	browserCursor int
	browserTop    int
	browserFilter string
	browserLoaded bool

	playlists       []playlistRow
	playlistCursor  int
	playlistTop     int
	playlistsLoaded bool
	preview         engine.PlaylistView

	levels engine.Levels

	// Render cache to avoid redundant string building when nothing changed.
	dirty      bool
	cachedView string
}

// NewModel constructs a TUI Model bound to cl.
func NewModel(parentCtx context.Context, cl *client.Client) *Model {
	ctx, cancel := context.WithCancel(parentCtx)
	return &Model{
		cl:        cl,
		ctx:       ctx,
		cancel:    cancel,
		styles:    defaultStyles,
		keys:      defaultKeys,
		width:     80,
		height:    24,
		connected: false,
		backoff:   minReconnectBackoff,
		dirty:     true,
	}
}

// SetTheme switches the active color palette. Unknown names fall back to the
// default theme and return false.
func (m *Model) SetTheme(name string) bool {
	name = strings.TrimSpace(name)
	if name == "" {
		name = "default"
	}
	ok := false
	for k := range themes {
		if equalFoldASCII(k, name) {
			ok = true
			break
		}
	}
	if !ok {
		return false
	}
	m.styles = newStyles(paletteFor(name))
	m.dirty = true
	return true
}

// Theme returns the active theme name.
func (m *Model) Theme() string { return m.styles.theme }

// Close cancels any background SSE/HTTP operations owned by the model.
func (m *Model) Close() {
	if m.cancel != nil {
		m.cancel()
	}
}

// Init starts the initial connection and SSE subscription.
func (m *Model) Init() tea.Cmd { return m.connectCmd() }

func (m *Model) connectCmd() tea.Cmd {
	cl := m.cl
	ctx := m.ctx
	lastID := m.lastEventID
	return func() tea.Msg {
		if cl == nil {
			return disconnectedMsg{err: apperr.New(apperr.ServiceNotRunning, "no client configured")}
		}
		cctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		st, err := cl.Status(cctx)
		cancel()
		if err != nil {
			return disconnectedMsg{err: err}
		}
		qctx, qcancel := context.WithTimeout(ctx, 2*time.Second)
		q, _ := cl.Queue(qctx)
		qcancel()

		evCh, err := cl.SubscribeEvents(ctx, lastID)
		if err != nil {
			return disconnectedMsg{err: err}
		}
		return connectedMsg{status: st, queue: q, events: evCh}
	}
}

func waitNextSSE(ctx context.Context, ch <-chan client.SSEEvent) tea.Cmd {
	return func() tea.Msg {
		select {
		case <-ctx.Done():
			return nil
		case ev, ok := <-ch:
			if !ok {
				return disconnectedMsg{
					err: apperr.New(apperr.ServiceNotRunning, "connection to daemon lost"),
				}
			}
			return sseEventMsg{event: ev, events: ch}
		}
	}
}

func scheduleProgressTick() tea.Cmd {
	return tea.Tick(progressTickInterval, func(t time.Time) tea.Msg {
		return progressTickMsg(t)
	})
}

func scheduleLevelTick() tea.Cmd {
	return tea.Tick(levelTickInterval, func(time.Time) tea.Msg {
		return levelTickMsg{}
	})
}

func scheduleReconnect(d time.Duration) tea.Cmd {
	return tea.Tick(d, func(time.Time) tea.Msg {
		return reconnectTickMsg{}
	})
}

// Update handles incoming events, user keys, and timers.
func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		if msg.Width != m.width || msg.Height != m.height {
			m.width = msg.Width
			m.height = msg.Height
			m.dirty = true
		}
		return m, nil

	case connectedMsg:
		m.connected = true
		m.backoff = minReconnectBackoff
		m.errBanner = ""
		m.applyStatus(msg.status)
		m.queue = msg.queue
		m.clampCursor()
		m.centerQueueOnCurrent()
		m.dirty = true
		cmds := []tea.Cmd{waitNextSSE(m.ctx, msg.events)}
		if cmd := m.ensureProgressTick(); cmd != nil {
			cmds = append(cmds, cmd)
		}
		if m.view == viewBrowser && !m.browserLoaded {
			cmds = append(cmds, m.browseCmd(""))
		}
		if m.view == viewPlaylists && !m.playlistsLoaded {
			cmds = append(cmds, m.playlistsCmd())
		}
		return m, tea.Batch(cmds...)

	case sseEventMsg:
		if msg.event.ID > 0 {
			m.lastEventID = msg.event.ID
		}
		if m.applySSEEvent(msg.event) {
			m.dirty = true
		}
		cmds := []tea.Cmd{waitNextSSE(m.ctx, msg.events)}
		if cmd := m.ensureProgressTick(); cmd != nil {
			cmds = append(cmds, cmd)
		}
		return m, tea.Batch(cmds...)

	case disconnectedMsg:
		m.connected = false
		m.ticking = false
		m.levelTicking = false
		if msg.err != nil {
			m.errBanner = apperr.As(msg.err).Error()
		} else {
			m.errBanner = string(apperr.ServiceNotRunning) + ": daemon disconnected"
		}
		m.dirty = true
		d := m.backoff
		m.backoff = min(maxReconnectBackoff, m.backoff*2)
		return m, scheduleReconnect(d)

	case reconnectTickMsg:
		if m.connected {
			return m, nil
		}
		return m, m.connectCmd()

	case progressTickMsg:
		if !m.connected || m.status.State != core.Playing {
			m.ticking = false
			m.levelTicking = false
			return m, nil
		}
		elapsed := time.Since(m.lastSyncTime).Milliseconds()
		newPos := m.lastSyncPosMs + elapsed
		if m.status.DurationMs > 0 && newPos > m.status.DurationMs {
			newPos = m.status.DurationMs
		}
		if newPos/250 != m.status.PositionMs/250 {
			m.status.PositionMs = newPos
			m.dirty = true
		}
		cmds := []tea.Cmd{scheduleProgressTick()}
		if cmd := m.ensureLevelTick(); cmd != nil {
			cmds = append(cmds, cmd)
		}
		return m, tea.Batch(cmds...)

	case levelTickMsg:
		m.levelTicking = false
		if !m.connected || m.status.State != core.Playing {
			return m, nil
		}
		return m, m.levelCmd()

	case levelsMsg:
		if msg.err == nil {
			m.levels = msg.levels
			m.dirty = true
		}
		if m.connected && m.status.State == core.Playing && !m.levelTicking {
			m.levelTicking = true
			return m, scheduleLevelTick()
		}
		return m, nil

	case browseResultMsg:
		if msg.err != nil {
			m.errBanner = apperr.As(msg.err).Error()
		} else {
			m.browser = msg.result
			m.browserLoaded = true
			m.browserCursor = 0
			m.browserTop = 0
			m.errBanner = ""
		}
		m.dirty = true
		return m, nil

	case playlistsResultMsg:
		if msg.err != nil {
			m.errBanner = apperr.As(msg.err).Error()
		} else {
			m.playlists = m.playlists[:0]
			for _, p := range msg.playlists {
				m.playlists = append(m.playlists, playlistRow{
					Kind:       "playlist",
					Name:       p.Name,
					Target:     p.SourcePath,
					TrackCount: p.TrackCount,
					Updated:    p.UpdatedAt,
				})
			}
			for _, a := range msg.aliases {
				m.playlists = append(m.playlists, playlistRow{
					Kind:   "alias",
					Name:   a.Name,
					Target: a.Name,
				})
			}
			m.playlistsLoaded = true
			m.errBanner = ""
			if m.playlistCursor >= len(m.playlists) {
				m.playlistCursor = max(0, len(m.playlists)-1)
			}
		}
		m.dirty = true
		return m, nil

	case playlistInspectMsg:
		if msg.err != nil {
			m.errBanner = apperr.As(msg.err).Error()
		} else {
			m.preview = msg.view
			m.errBanner = ""
		}
		m.dirty = true
		return m, nil

	case actionResultMsg:
		if msg.quit {
			m.QuitDaemonReq = true
			m.Close()
			return m, tea.Quit
		}
		if msg.err != nil {
			ae := apperr.As(msg.err)
			m.errBanner = ae.Error()
			if ae.Code == apperr.ServiceNotRunning {
				m.connected = false
				m.ticking = false
				m.dirty = true
				return m, scheduleReconnect(m.backoff)
			}
			m.dirty = true
			return m, nil
		}
		m.errBanner = ""
		if msg.status != nil {
			m.applyStatus(*msg.status)
		}
		if msg.queue != nil {
			m.queue = *msg.queue
			m.clampCursor()
			if m.queueFilter != "" {
				m.clampCursorToFilter()
			}
			m.centerQueueOnCurrent()
		}
		m.dirty = true
		return m, m.ensureProgressTick()

	case tea.KeyMsg:
		return m.handleKey(msg)
	}

	return m, nil
}

func (m *Model) applyStatus(st engine.Status) {
	m.status = st
	m.lastSyncTime = time.Now()
	m.lastSyncPosMs = st.PositionMs
	for i := range m.queue.Tracks {
		m.queue.Tracks[i].Current = m.queue.Tracks[i].Index == st.TrackIndex
	}
	m.queue.CurrentIndex = st.TrackIndex
}

// applySSEEvent folds an event into the model and reports whether anything
// visible changed. Unchanged events do not dirty the render cache.
func (m *Model) applySSEEvent(ev client.SSEEvent) bool {
	switch ev.Type {
	case events.Snapshot, events.State, events.Track:
		var st engine.Status
		if json.Unmarshal(ev.Data, &st) == nil {
			old := m.status
			m.applyStatus(st)
			if ev.Type == events.Track {
				m.centerQueueOnCurrent()
			}
			return ev.Type == events.Track || old != st
		}
	case events.Queue:
		var q engine.Queue
		if json.Unmarshal(ev.Data, &q) == nil {
			m.queue = q
			m.clampCursor()
			if m.queueFilter != "" {
				m.clampCursorToFilter()
			}
			m.centerQueueOnCurrent()
			return true
		}
	case events.Volume, events.Mute:
		var v struct {
			Volume int  `json:"volume"`
			Muted  bool `json:"muted"`
		}
		if json.Unmarshal(ev.Data, &v) == nil {
			if m.status.Volume == v.Volume && m.status.Muted == v.Muted {
				return false
			}
			m.status.Volume = v.Volume
			m.status.Muted = v.Muted
			return true
		}
	case events.Shuffle:
		var s struct {
			Shuffle bool `json:"shuffle"`
		}
		if json.Unmarshal(ev.Data, &s) == nil {
			if m.status.Shuffle == s.Shuffle {
				return false
			}
			m.status.Shuffle = s.Shuffle
			m.queue.Shuffle = s.Shuffle
			return true
		}
	case events.Loop:
		var l struct {
			Loop bool `json:"loop"`
		}
		if json.Unmarshal(ev.Data, &l) == nil {
			if m.status.Loop == l.Loop {
				return false
			}
			m.status.Loop = l.Loop
			m.queue.Loop = l.Loop
			return true
		}
	case events.Error:
		var ae apperr.Error
		if json.Unmarshal(ev.Data, &ae) == nil {
			m.errBanner = ae.Error()
			return true
		}
	case events.Device:
		var d struct {
			Reason string     `json:"reason"`
			Paused bool       `json:"paused"`
			State  core.State `json:"state"`
		}
		if json.Unmarshal(ev.Data, &d) == nil {
			changed := false
			if d.State != "" && m.status.State != d.State {
				m.status.State = d.State
				changed = true
			}
			if d.Paused {
				m.errBanner = "Paused: " + d.Reason
				changed = true
			}
			return changed
		}
	case events.Shutdown:
		m.connected = false
		m.ticking = false
		m.errBanner = string(apperr.ServiceNotRunning) + ": daemon stopped"
		return true
	}
	return false
}

func (m *Model) ensureProgressTick() tea.Cmd {
	if m.connected && m.status.State == core.Playing && !m.ticking {
		m.ticking = true
		return scheduleProgressTick()
	}
	return nil
}

func (m *Model) ensureLevelTick() tea.Cmd {
	if m.connected && m.status.State == core.Playing && !m.levelTicking {
		m.levelTicking = true
		return scheduleLevelTick()
	}
	return nil
}

func (m *Model) levelCmd() tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(m.ctx, 750*time.Millisecond)
		defer cancel()
		lv, err := m.cl.Levels(ctx)
		return levelsMsg{levels: lv, err: err}
	}
}

func (m *Model) browseCmd(path string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(m.ctx, 4*time.Second)
		defer cancel()
		res, err := m.cl.Browse(ctx, path)
		return browseResultMsg{result: res, err: err}
	}
}

func (m *Model) playlistsCmd() tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(m.ctx, 4*time.Second)
		defer cancel()
		pl, err := m.cl.PlaylistList(ctx)
		if err != nil {
			return playlistsResultMsg{err: err}
		}
		al, err := m.cl.AliasList(ctx)
		if err != nil {
			return playlistsResultMsg{err: err}
		}
		return playlistsResultMsg{playlists: pl, aliases: al}
	}
}

func (m *Model) inspectCmd(target string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(m.ctx, 4*time.Second)
		defer cancel()
		v, err := m.cl.PlaylistInspect(ctx, target)
		return playlistInspectMsg{target: target, view: v, err: err}
	}
}

func (m *Model) runAction(fn func(ctx context.Context) (*engine.Status, *engine.Queue, error), quit bool) tea.Cmd {
	return func() tea.Msg {
		if m.cl == nil {
			return actionResultMsg{err: apperr.New(apperr.ServiceNotRunning, "no client configured")}
		}
		ctx, cancel := context.WithTimeout(m.ctx, 4*time.Second)
		defer cancel()
		st, q, err := fn(ctx)
		if err != nil {
			return actionResultMsg{err: err}
		}
		return actionResultMsg{status: st, queue: q, quit: quit}
	}
}

func (m *Model) statusAction(fn func(ctx context.Context) (engine.Status, error)) tea.Cmd {
	return m.runAction(func(ctx context.Context) (*engine.Status, *engine.Queue, error) {
		st, err := fn(ctx)
		if err != nil {
			return nil, nil, err
		}
		return &st, nil, nil
	}, false)
}

// --- queue cursor & filtering --------------------------------------------

func (m *Model) queueIndices() []int {
	n := len(m.queue.Tracks)
	if n == 0 {
		return nil
	}
	filter := strings.ToLower(strings.TrimSpace(m.queueFilter))
	if filter == "" {
		out := make([]int, n)
		for i := range out {
			out[i] = i
		}
		return out
	}
	out := make([]int, 0, n)
	for i, t := range m.queue.Tracks {
		if queueItemMatches(t, filter) {
			out = append(out, i)
		}
	}
	return out
}

func queueItemMatches(t engine.QueueItem, filter string) bool {
	hay := strings.ToLower(strings.Join([]string{
		t.Name, t.Path, t.Metadata.Title, t.Metadata.Artist, t.Metadata.Album,
	}, " "))
	return fuzzyContains(hay, filter)
}

// fuzzyContains matches filter against hay using case-insensitive substring or
// ordered-subsequence matching, so "swn" finds "Sine Wave Now".
func fuzzyContains(hay, filter string) bool {
	filter = strings.ToLower(strings.TrimSpace(filter))
	if filter == "" {
		return true
	}
	terms := strings.Fields(filter)
	for _, term := range terms {
		if strings.Contains(hay, term) {
			continue
		}
		i := 0
		for _, r := range hay {
			if i < len(term) && byte(r) == term[i] {
				i++
			}
		}
		if i < len(term) {
			return false
		}
	}
	return true
}

func (m *Model) queuePosition() int {
	idx := m.queueIndices()
	for p, i := range idx {
		if i == m.cursor {
			return p
		}
	}
	return 0
}

func (m *Model) clampCursor() {
	n := len(m.queue.Tracks)
	if n == 0 {
		m.cursor = 0
		return
	}
	if m.cursor < 0 {
		m.cursor = 0
	}
	if m.cursor >= n {
		m.cursor = n - 1
	}
}

func (m *Model) moveQueueCursor(delta int) {
	idx := m.queueIndices()
	if len(idx) == 0 {
		return
	}
	pos := m.queuePosition()
	pos += delta
	if pos < 0 {
		pos = 0
	}
	if pos >= len(idx) {
		pos = len(idx) - 1
	}
	m.cursor = idx[pos]
	m.ensureQueueCursorVisible()
	m.dirty = true
}

func (m *Model) ensureQueueCursorVisible() {
	visible := m.queueViewRows()
	pos := m.queuePosition()
	if pos < m.queueTop {
		m.queueTop = pos
	}
	if pos >= m.queueTop+visible {
		m.queueTop = pos - visible + 1
	}
	if m.queueTop < 0 {
		m.queueTop = 0
	}
}

func (m *Model) centerQueueOnCurrent() {
	if m.queueFilter != "" {
		return
	}
	if m.status.TrackIndex >= 0 && m.status.TrackIndex < len(m.queue.Tracks) {
		m.cursor = m.status.TrackIndex
	}
	visible := m.queueViewRows()
	pos := m.queuePosition()
	if pos < m.queueTop || pos >= m.queueTop+visible {
		m.queueTop = max(0, pos-visible/2)
	}
}

func (m *Model) queueViewRows() int {
	rows := m.height - 15
	if rows < 3 {
		return 3
	}
	return rows
}

// --- browser cursor -------------------------------------------------------

func (m *Model) browserIndices() []int {
	filter := strings.ToLower(strings.TrimSpace(m.browserFilter))
	if filter == "" {
		out := make([]int, len(m.browser.Entries))
		for i := range out {
			out[i] = i
		}
		return out
	}
	out := make([]int, 0, len(m.browser.Entries))
	for i, e := range m.browser.Entries {
		hay := strings.ToLower(e.Name + " " + e.Metadata.Title + " " + e.Metadata.Artist + " " + e.Metadata.Album)
		if fuzzyContains(hay, filter) {
			out = append(out, i)
		}
	}
	return out
}

func (m *Model) browserPosition() int {
	idx := m.browserIndices()
	for p, i := range idx {
		if i == m.browserCursor {
			return p
		}
	}
	return 0
}

func (m *Model) moveBrowserCursor(delta int) {
	idx := m.browserIndices()
	if len(idx) == 0 {
		return
	}
	pos := m.browserPosition() + delta
	if pos < 0 {
		pos = 0
	}
	if pos >= len(idx) {
		pos = len(idx) - 1
	}
	m.browserCursor = idx[pos]
	m.ensureBrowserCursorVisible()
	m.dirty = true
}

func (m *Model) ensureBrowserCursorVisible() {
	rows := m.browserViewRows()
	pos := m.browserPosition()
	if pos < m.browserTop {
		m.browserTop = pos
	}
	if pos >= m.browserTop+rows {
		m.browserTop = pos - rows + 1
	}
	if m.browserTop < 0 {
		m.browserTop = 0
	}
}

func (m *Model) browserViewRows() int {
	rows := m.height - 9
	if rows < 3 {
		return 3
	}
	return rows
}

func (m *Model) selectedBrowserEntry() (engine.BrowseEntry, bool) {
	if m.browserCursor < 0 || m.browserCursor >= len(m.browser.Entries) {
		return engine.BrowseEntry{}, false
	}
	return m.browser.Entries[m.browserCursor], true
}

// --- playlists cursor -----------------------------------------------------

func (m *Model) movePlaylistCursor(delta int) {
	if len(m.playlists) == 0 {
		return
	}
	m.playlistCursor += delta
	if m.playlistCursor < 0 {
		m.playlistCursor = 0
	}
	if m.playlistCursor >= len(m.playlists) {
		m.playlistCursor = len(m.playlists) - 1
	}
	m.ensurePlaylistCursorVisible()
	m.dirty = true
}

func (m *Model) ensurePlaylistCursorVisible() {
	rows := m.playlistsViewRows()
	if m.playlistCursor < m.playlistTop {
		m.playlistTop = m.playlistCursor
	}
	if m.playlistCursor >= m.playlistTop+rows {
		m.playlistTop = m.playlistCursor - rows + 1
	}
	if m.playlistTop < 0 {
		m.playlistTop = 0
	}
}

func (m *Model) playlistsViewRows() int {
	rows := m.height - 10
	if rows < 3 {
		return 3
	}
	return rows
}

func (m *Model) currentPlaylistRow() (playlistRow, bool) {
	if m.playlistCursor < 0 || m.playlistCursor >= len(m.playlists) {
		return playlistRow{}, false
	}
	return m.playlists[m.playlistCursor], true
}

// --- shared helpers -------------------------------------------------------

func (m *Model) setView(v viewID) tea.Cmd {
	m.view = v
	m.dirty = true
	m.showFullHelp = false
	var cmd tea.Cmd
	switch v {
	case viewBrowser:
		if !m.browserLoaded {
			cmd = m.browseCmd("")
		}
	case viewPlaylists:
		if !m.playlistsLoaded {
			cmd = m.playlistsCmd()
		}
	}
	return cmd
}

func (m *Model) currentTrackTitle() string {
	if m.status.Metadata.Display() != "" {
		return m.status.Metadata.Display()
	}
	if m.status.CurrentTrack != "" {
		return filepath.Base(m.status.CurrentTrack)
	}
	return ""
}

func (m *Model) itemTitle(t engine.QueueItem) string {
	if d := t.Metadata.Display(); d != "" {
		return d
	}
	return t.Name
}

func (m *Model) metadataOf(e engine.BrowseEntry) audio.Metadata {
	return e.Metadata
}

func formatMillis(ms int64) string {
	if ms <= 0 {
		return "00:00"
	}
	totalSec := ms / 1000
	mins := totalSec / 60
	secs := totalSec % 60
	if mins >= 60 {
		hours := mins / 60
		return fmt.Sprintf("%d:%02d:%02d", hours, mins%60, secs)
	}
	return fmt.Sprintf("%02d:%02d", mins, secs)
}

func formatBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}
