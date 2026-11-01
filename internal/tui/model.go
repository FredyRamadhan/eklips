package tui

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/help"
	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/progress"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/frdn/eklips/internal/apperr"
	"github.com/frdn/eklips/internal/client"
	"github.com/frdn/eklips/internal/core"
	"github.com/frdn/eklips/internal/engine"
	"github.com/frdn/eklips/internal/events"
)

const (
	progressTickInterval = 250 * time.Millisecond // 4 Hz local interpolation
	minReconnectBackoff  = 150 * time.Millisecond
	maxReconnectBackoff  = 3 * time.Second
)

type keyMap struct {
	PlayPause  key.Binding
	Stop       key.Binding
	Next       key.Binding
	Prev       key.Binding
	VolUp      key.Binding
	VolDown    key.Binding
	Mute       key.Binding
	Shuffle    key.Binding
	Loop       key.Binding
	Up         key.Binding
	Down       key.Binding
	Help       key.Binding
	QuitTUI    key.Binding
	QuitDaemon key.Binding
}

func (k keyMap) ShortHelp() []key.Binding {
	return []key.Binding{
		k.PlayPause, k.Stop, k.Next, k.Prev, k.VolUp, k.VolDown,
		k.Mute, k.Shuffle, k.Loop, k.Help, k.QuitTUI, k.QuitDaemon,
	}
}

func (k keyMap) FullHelp() [][]key.Binding {
	return [][]key.Binding{
		{k.PlayPause, k.Stop, k.Next, k.Prev},
		{k.VolUp, k.VolDown, k.Mute, k.Shuffle, k.Loop},
		{k.Up, k.Down, k.Help, k.QuitTUI, k.QuitDaemon},
	}
}

var defaultKeys = keyMap{
	PlayPause:  key.NewBinding(key.WithKeys(" "), key.WithHelp("space", "play/pause")),
	Stop:       key.NewBinding(key.WithKeys("s"), key.WithHelp("s", "stop")),
	Next:       key.NewBinding(key.WithKeys("n"), key.WithHelp("n", "next")),
	Prev:       key.NewBinding(key.WithKeys("p"), key.WithHelp("p", "prev")),
	VolUp:      key.NewBinding(key.WithKeys("+", "="), key.WithHelp("+", "vol+")),
	VolDown:    key.NewBinding(key.WithKeys("-", "_"), key.WithHelp("-", "vol-")),
	Mute:       key.NewBinding(key.WithKeys("m"), key.WithHelp("m", "mute")),
	Shuffle:    key.NewBinding(key.WithKeys("r"), key.WithHelp("r", "shuffle")),
	Loop:       key.NewBinding(key.WithKeys("l"), key.WithHelp("l", "loop")),
	Up:         key.NewBinding(key.WithKeys("up", "k"), key.WithHelp("↑/k", "scroll up")),
	Down:       key.NewBinding(key.WithKeys("down", "j"), key.WithHelp("↓/j", "scroll down")),
	Help:       key.NewBinding(key.WithKeys("?"), key.WithHelp("?", "help")),
	QuitTUI:    key.NewBinding(key.WithKeys("q", "esc", "ctrl+c"), key.WithHelp("q/Esc", "quit TUI")),
	QuitDaemon: key.NewBinding(key.WithKeys("Q"), key.WithHelp("Q", "quit daemon")),
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
	actionResultMsg  struct {
		status *engine.Status
		queue  *engine.Queue
		err    error
		quit   bool
	}
)

// Model is the Bubble Tea state model for the TUI client. It holds no playback
// engine and owns only presentation state.
type Model struct {
	cl            *client.Client
	ctx           context.Context
	cancel        context.CancelFunc
	styles        styles
	keys          keyMap
	help          help.Model
	prog          progress.Model
	width         int
	height        int
	connected     bool
	status        engine.Status
	queue         engine.Queue
	scrollOffset  int
	errBanner     string
	showFullHelp  bool
	confirmQuitD  bool
	ticking       bool
	lastSyncTime  time.Time
	lastSyncPosMs int64
	lastEventID   uint64
	backoff       time.Duration
	QuitDaemonReq bool

	// Render cache to avoid redundant string building when nothing changed.
	dirty      bool
	cachedView string
}

// NewModel constructs a TUI Model bound to cl.
func NewModel(parentCtx context.Context, cl *client.Client) *Model {
	ctx, cancel := context.WithCancel(parentCtx)
	p := progress.New(progress.WithDefaultGradient(), progress.WithoutPercentage())
	p.Width = 40
	h := help.New()
	h.ShowAll = false
	return &Model{
		cl:        cl,
		ctx:       ctx,
		cancel:    cancel,
		styles:    defaultStyles,
		keys:      defaultKeys,
		help:      h,
		prog:      p,
		width:     80,
		height:    24,
		connected: false,
		backoff:   minReconnectBackoff,
		dirty:     true,
	}
}

// Close cancels any background SSE/HTTP operations owned by the model.
func (m *Model) Close() {
	if m.cancel != nil {
		m.cancel()
	}
}

// Init starts the initial connection and SSE subscription.
func (m *Model) Init() tea.Cmd {
	return m.connectCmd()
}

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
			barW := msg.Width - 22
			if barW < 10 {
				barW = 10
			} else if barW > 80 {
				barW = 80
			}
			m.prog.Width = barW
			m.help.Width = msg.Width - 4
			m.dirty = true
		}
		return m, nil

	case connectedMsg:
		m.connected = true
		m.backoff = minReconnectBackoff
		m.errBanner = ""
		m.applyStatus(msg.status)
		m.queue = msg.queue
		m.centerQueueOnCurrent()
		m.dirty = true
		cmds := []tea.Cmd{waitNextSSE(m.ctx, msg.events)}
		if cmd := m.ensureProgressTick(); cmd != nil {
			cmds = append(cmds, cmd)
		}
		return m, tea.Batch(cmds...)

	case sseEventMsg:
		if msg.event.ID > 0 {
			m.lastEventID = msg.event.ID
		}
		m.applySSEEvent(msg.event)
		m.dirty = true
		cmds := []tea.Cmd{waitNextSSE(m.ctx, msg.events)}
		if cmd := m.ensureProgressTick(); cmd != nil {
			cmds = append(cmds, cmd)
		}
		return m, tea.Batch(cmds...)

	case disconnectedMsg:
		m.connected = false
		m.ticking = false
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
		return m, scheduleProgressTick()

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
			m.centerQueueOnCurrent()
		}
		m.dirty = true
		return m, m.ensureProgressTick()

	case tea.KeyMsg:
		return m.handleKey(msg)
	}

	return m, nil
}

func (m *Model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.confirmQuitD {
		switch msg.String() {
		case "y", "Y", "enter":
			m.confirmQuitD = false
			m.dirty = true
			return m, m.runAction(func(ctx context.Context) (*engine.Status, *engine.Queue, error) {
				err := m.cl.Shutdown(ctx)
				return nil, nil, err
			}, true)
		case "n", "N", "esc", "q":
			m.confirmQuitD = false
			m.dirty = true
			return m, nil
		case "ctrl+c":
			m.Close()
			return m, tea.Quit
		}
		return m, nil
	}

	switch {
	case key.Matches(msg, m.keys.QuitTUI):
		m.Close()
		return m, tea.Quit

	case key.Matches(msg, m.keys.QuitDaemon):
		m.confirmQuitD = true
		m.dirty = true
		return m, nil

	case key.Matches(msg, m.keys.Help):
		m.showFullHelp = !m.showFullHelp
		m.help.ShowAll = m.showFullHelp
		m.dirty = true
		return m, nil

	case key.Matches(msg, m.keys.Up):
		if m.scrollOffset > 0 {
			m.scrollOffset--
			m.dirty = true
		}
		return m, nil

	case key.Matches(msg, m.keys.Down):
		if m.scrollOffset+1 < len(m.queue.Tracks) {
			m.scrollOffset++
			m.dirty = true
		}
		return m, nil

	case key.Matches(msg, m.keys.PlayPause):
		curState := m.status.State
		return m, m.runAction(func(ctx context.Context) (*engine.Status, *engine.Queue, error) {
			var st engine.Status
			var err error
			if curState == core.Playing {
				st, err = m.cl.Pause(ctx)
			} else {
				st, err = m.cl.Play(ctx, "", false)
			}
			return &st, nil, err
		}, false)

	case key.Matches(msg, m.keys.Stop):
		return m, m.runAction(func(ctx context.Context) (*engine.Status, *engine.Queue, error) {
			st, err := m.cl.Stop(ctx)
			return &st, nil, err
		}, false)

	case key.Matches(msg, m.keys.Next):
		return m, m.runAction(func(ctx context.Context) (*engine.Status, *engine.Queue, error) {
			st, err := m.cl.Next(ctx)
			if err != nil {
				return nil, nil, err
			}
			q, _ := m.cl.Queue(ctx)
			return &st, &q, nil
		}, false)

	case key.Matches(msg, m.keys.Prev):
		return m, m.runAction(func(ctx context.Context) (*engine.Status, *engine.Queue, error) {
			st, err := m.cl.Previous(ctx)
			if err != nil {
				return nil, nil, err
			}
			q, _ := m.cl.Queue(ctx)
			return &st, &q, nil
		}, false)

	case key.Matches(msg, m.keys.VolUp):
		return m, m.runAction(func(ctx context.Context) (*engine.Status, *engine.Queue, error) {
			st, err := m.cl.VolumeUp(ctx)
			return &st, nil, err
		}, false)

	case key.Matches(msg, m.keys.VolDown):
		return m, m.runAction(func(ctx context.Context) (*engine.Status, *engine.Queue, error) {
			st, err := m.cl.VolumeDown(ctx)
			return &st, nil, err
		}, false)

	case key.Matches(msg, m.keys.Mute):
		return m, m.runAction(func(ctx context.Context) (*engine.Status, *engine.Queue, error) {
			st, err := m.cl.Mute(ctx)
			return &st, nil, err
		}, false)

	case key.Matches(msg, m.keys.Shuffle):
		return m, m.runAction(func(ctx context.Context) (*engine.Status, *engine.Queue, error) {
			st, err := m.cl.Shuffle(ctx)
			if err != nil {
				return nil, nil, err
			}
			q, _ := m.cl.Queue(ctx)
			return &st, &q, nil
		}, false)

	case key.Matches(msg, m.keys.Loop):
		return m, m.runAction(func(ctx context.Context) (*engine.Status, *engine.Queue, error) {
			st, err := m.cl.Loop(ctx)
			return &st, nil, err
		}, false)
	}

	return m, nil
}

func (m *Model) runAction(fn func(ctx context.Context) (*engine.Status, *engine.Queue, error), quit bool) tea.Cmd {
	return func() tea.Msg {
		if m.cl == nil {
			return actionResultMsg{err: apperr.New(apperr.ServiceNotRunning, "no client configured")}
		}
		ctx, cancel := context.WithTimeout(m.ctx, 3*time.Second)
		defer cancel()
		st, q, err := fn(ctx)
		if err != nil {
			return actionResultMsg{err: err}
		}
		return actionResultMsg{status: st, queue: q, quit: quit}
	}
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

func (m *Model) applySSEEvent(ev client.SSEEvent) {
	switch ev.Type {
	case events.Snapshot, events.State, events.Track:
		var st engine.Status
		if json.Unmarshal(ev.Data, &st) == nil {
			m.applyStatus(st)
			if ev.Type == events.Track {
				m.centerQueueOnCurrent()
			}
		}
	case events.Queue:
		var q engine.Queue
		if json.Unmarshal(ev.Data, &q) == nil {
			m.queue = q
			m.centerQueueOnCurrent()
		}
	case events.Volume, events.Mute:
		var v struct {
			Volume int  `json:"volume"`
			Muted  bool `json:"muted"`
		}
		if json.Unmarshal(ev.Data, &v) == nil {
			m.status.Volume = v.Volume
			m.status.Muted = v.Muted
		}
	case events.Shuffle:
		var s struct {
			Shuffle bool `json:"shuffle"`
		}
		if json.Unmarshal(ev.Data, &s) == nil {
			m.status.Shuffle = s.Shuffle
			m.queue.Shuffle = s.Shuffle
		}
	case events.Loop:
		var l struct {
			Loop bool `json:"loop"`
		}
		if json.Unmarshal(ev.Data, &l) == nil {
			m.status.Loop = l.Loop
			m.queue.Loop = l.Loop
		}
	case events.Error:
		var ae apperr.Error
		if json.Unmarshal(ev.Data, &ae) == nil {
			m.errBanner = ae.Error()
		}
	case events.Device:
		var d struct {
			Reason string     `json:"reason"`
			Paused bool       `json:"paused"`
			State  core.State `json:"state"`
		}
		if json.Unmarshal(ev.Data, &d) == nil {
			if d.State != "" {
				m.status.State = d.State
			}
			if d.Paused {
				m.errBanner = "Paused: " + d.Reason
			}
		}
	case events.Shutdown:
		m.connected = false
		m.ticking = false
		m.errBanner = string(apperr.ServiceNotRunning) + ": daemon stopped"
	}
}

func (m *Model) ensureProgressTick() tea.Cmd {
	if m.connected && m.status.State == core.Playing && !m.ticking {
		m.ticking = true
		return scheduleProgressTick()
	}
	return nil
}

func (m *Model) centerQueueOnCurrent() {
	if m.status.TrackIndex >= 0 && m.status.TrackIndex < len(m.queue.Tracks) {
		visible := m.visibleQueueRows()
		if m.status.TrackIndex < m.scrollOffset || m.status.TrackIndex >= m.scrollOffset+visible {
			m.scrollOffset = max(0, m.status.TrackIndex-visible/2)
		}
	}
}

func (m *Model) visibleQueueRows() int {
	rows := m.height - 12
	if rows < 3 {
		return 3
	}
	return rows
}

// View renders the TUI. It reuses the cached string when no state has changed.
func (m *Model) View() string {
	if !m.dirty && m.cachedView != "" {
		return m.cachedView
	}
	var b strings.Builder
	b.Grow(1024)

	// 1. Header bar: title, connection URL, playback state badge
	urlStr := "local"
	if m.cl != nil {
		urlStr = m.cl.BaseURL()
	}
	var badge string
	if !m.connected {
		badge = m.styles.badgeOffline.Render("DISCONNECTED")
	} else {
		switch m.status.State {
		case core.Playing:
			badge = m.styles.badgePlay.Render("PLAYING")
		case core.Paused:
			badge = m.styles.badgePause.Render("PAUSED")
		default:
			badge = m.styles.badgeStop.Render("STOPPED")
		}
	}
	headerText := fmt.Sprintf("eklips  •  %s", urlStr)
	b.WriteString(m.styles.header.Render(headerText) + " " + badge + "\n\n")

	// 2. Now Playing & Progress Bar
	b.WriteString(m.styles.sectionTitle.Render("Now Playing") + "\n")
	if m.status.CurrentTrack != "" {
		trackName := filepath.Base(m.status.CurrentTrack)
		b.WriteString("  " + m.styles.trackTitle.Render(trackName) + "\n")
		if m.status.SourcePath != "" {
			b.WriteString("  " + m.styles.metaLabel.Render(fmt.Sprintf("%s: %s", m.status.SourceType, m.status.SourcePath)) + "\n")
		}
	} else {
		b.WriteString("  " + m.styles.metaLabel.Render("No media loaded") + "\n")
	}

	var ratio float64
	if m.status.DurationMs > 0 {
		ratio = float64(m.status.PositionMs) / float64(m.status.DurationMs)
		if ratio < 0 {
			ratio = 0
		} else if ratio > 1 {
			ratio = 1
		}
	}
	bar := m.prog.ViewAs(ratio)
	timeStr := fmt.Sprintf("%s / %s", formatMillis(m.status.PositionMs), formatMillis(m.status.DurationMs))
	b.WriteString(fmt.Sprintf("  %s  %s\n\n", bar, m.styles.metaValue.Render(timeStr)))

	// 3. Volume, Mute, Shuffle, Loop indicators
	volText := fmt.Sprintf("Vol: %d%%", m.status.Volume)
	if m.status.Muted {
		volText += " [MUTED]"
	}
	volPill := m.styles.pillOn.Render(volText)
	if m.status.Muted {
		volPill = m.styles.pillOff.Render(volText)
	}
	shufPill := m.styles.pillOff.Render("Shuffle: OFF")
	if m.status.Shuffle {
		shufPill = m.styles.pillOn.Render("Shuffle: ON")
	}
	loopPill := m.styles.pillOff.Render("Loop: OFF")
	if m.status.Loop {
		loopPill = m.styles.pillOn.Render("Loop: ON")
	}
	b.WriteString(fmt.Sprintf("  %s  %s  %s\n\n", volPill, shufPill, loopPill))

	// 4. Scrollable Queue
	qCount := len(m.queue.Tracks)
	b.WriteString(m.styles.sectionTitle.Render(fmt.Sprintf("Queue (%d)", qCount)) + "\n")
	if qCount == 0 {
		b.WriteString("  " + m.styles.metaLabel.Render("(empty)") + "\n")
	} else {
		visible := m.visibleQueueRows()
		start := m.scrollOffset
		if start >= qCount {
			start = max(0, qCount-1)
		}
		end := min(qCount, start+visible)
		for i := start; i < end; i++ {
			item := m.queue.Tracks[i]
			prefix := "   "
			nameStyle := m.styles.queueItem
			if item.Index == m.status.TrackIndex {
				prefix = " ▶ "
				nameStyle = m.styles.queueCurrent
			}
			idxStr := m.styles.queueIndex.Render(fmt.Sprintf("%2d.", item.Index+1))
			b.WriteString(fmt.Sprintf("%s%s %s\n", prefix, idxStr, nameStyle.Render(item.Name)))
		}
	}
	b.WriteByte('\n')

	// 5. Confirmation or Error Banner
	if m.confirmQuitD {
		b.WriteString(m.styles.confirmBox.Render("Quit running daemon and stop playback? [y/N]") + "\n")
	} else if m.errBanner != "" {
		b.WriteString(m.styles.errorBanner.Render("Error: "+m.errBanner) + "\n")
	}

	// 6. Help Footer
	b.WriteString(m.styles.footer.Render(m.help.View(m.keys)))

	m.cachedView = m.styles.app.Render(b.String())
	m.dirty = false
	return m.cachedView
}

func formatMillis(ms int64) string {
	if ms <= 0 {
		return "00:00"
	}
	totalSec := ms / 1000
	mins := totalSec / 60
	secs := totalSec % 60
	return fmt.Sprintf("%02d:%02d", mins, secs)
}
