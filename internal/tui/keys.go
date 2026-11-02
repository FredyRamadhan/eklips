package tui

import (
	"context"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/key"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/frdn/eklips/internal/apperr"
	"github.com/frdn/eklips/internal/core"
	"github.com/frdn/eklips/internal/engine"
)

func (m *Model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.inputMode != inputNone {
		return m.handleInputKey(msg)
	}
	if m.confirm.kind != confirmNone {
		return m.handleConfirmKey(msg)
	}

	switch msg.String() {
	case "ctrl+c":
		m.Close()
		return m, tea.Quit
	case "q", "esc":
		m.Close()
		return m, tea.Quit
	case "Q":
		m.confirm = confirmState{kind: confirmQuitDaemon, label: "Quit running daemon and stop playback?"}
		m.dirty = true
		return m, nil
	case "?":
		m.showFullHelp = !m.showFullHelp
		m.dirty = true
		return m, nil
	case "tab":
		return m, m.setView((m.view + 1) % 3)
	case "1":
		return m, m.setView(viewQueue)
	case "2":
		return m, m.setView(viewBrowser)
	case "3":
		return m, m.setView(viewPlaylists)
	case ":":
		m.openInput(inputCommand, ":")
		return m, nil
	case "/":
		switch m.view {
		case viewBrowser:
			m.openInput(inputBrowserFilter, "/")
		case viewPlaylists:
			m.errBanner = "filtering is not available in the playlists view"
		default:
			m.openInput(inputQueueFilter, "/")
		}
		return m, nil
	}

	var cmd tea.Cmd
	switch m.view {
	case viewQueue:
		if handled, c := m.handleQueueKey(msg); handled {
			return m, c
		}
	case viewBrowser:
		if handled, c := m.handleBrowserKey(msg); handled {
			return m, c
		}
	case viewPlaylists:
		if handled, c := m.handlePlaylistsKey(msg); handled {
			return m, c
		}
	}
	if c := m.handleGlobalPlaybackKey(msg); c != nil {
		cmd = c
	}
	return m, cmd
}

func (m *Model) handleGlobalPlaybackKey(msg tea.KeyMsg) tea.Cmd {
	switch {
	case key.Matches(msg, m.keys.PlayPause):
		curState := m.status.State
		return m.runAction(func(ctx context.Context) (*engine.Status, *engine.Queue, error) {
			var st engine.Status
			var err error
			if curState == core.Playing {
				st, err = m.cl.Pause(ctx)
			} else {
				st, err = m.cl.Play(ctx, "", false)
			}
			if err != nil {
				return nil, nil, err
			}
			return &st, nil, nil
		}, false)
	case key.Matches(msg, m.keys.Stop):
		return m.statusAction(func(ctx context.Context) (engine.Status, error) { return m.cl.Stop(ctx) })
	case key.Matches(msg, m.keys.Next):
		return m.runAction(func(ctx context.Context) (*engine.Status, *engine.Queue, error) {
			st, err := m.cl.Next(ctx)
			if err != nil {
				return nil, nil, err
			}
			q, _ := m.cl.Queue(ctx)
			return &st, &q, nil
		}, false)
	case key.Matches(msg, m.keys.VolUp):
		return m.statusAction(func(ctx context.Context) (engine.Status, error) { return m.cl.VolumeUp(ctx) })
	case key.Matches(msg, m.keys.VolDown):
		return m.statusAction(func(ctx context.Context) (engine.Status, error) { return m.cl.VolumeDown(ctx) })
	case key.Matches(msg, m.keys.Mute):
		return m.statusAction(func(ctx context.Context) (engine.Status, error) { return m.cl.Mute(ctx) })
	case key.Matches(msg, m.keys.Shuffle):
		return m.runAction(func(ctx context.Context) (*engine.Status, *engine.Queue, error) {
			st, err := m.cl.Shuffle(ctx)
			if err != nil {
				return nil, nil, err
			}
			q, _ := m.cl.Queue(ctx)
			return &st, &q, nil
		}, false)
	case key.Matches(msg, m.keys.Loop):
		return m.statusAction(func(ctx context.Context) (engine.Status, error) { return m.cl.Loop(ctx) })
	}
	return nil
}

func (m *Model) handleQueueKey(msg tea.KeyMsg) (bool, tea.Cmd) {
	switch {
	case key.Matches(msg, m.keys.Down):
		m.moveQueueCursor(1)
		return true, nil
	case key.Matches(msg, m.keys.Up):
		m.moveQueueCursor(-1)
		return true, nil
	case key.Matches(msg, m.keys.Top):
		if idx := m.queueIndices(); len(idx) > 0 {
			m.cursor = idx[0]
			m.queueTop = 0
			m.dirty = true
		}
		return true, nil
	case key.Matches(msg, m.keys.Bottom):
		if idx := m.queueIndices(); len(idx) > 0 {
			m.cursor = idx[len(idx)-1]
			m.ensureQueueCursorVisible()
			m.dirty = true
		}
		return true, nil
	case key.Matches(msg, m.keys.Enter):
		if len(m.queue.Tracks) == 0 {
			return true, nil
		}
		idx := m.cursor
		return true, m.runAction(func(ctx context.Context) (*engine.Status, *engine.Queue, error) {
			st, err := m.cl.PlayIndex(ctx, idx)
			if err != nil {
				return nil, nil, err
			}
			q, _ := m.cl.Queue(ctx)
			return &st, &q, nil
		}, false)
	case key.Matches(msg, m.keys.SeekForward):
		return true, m.statusAction(func(ctx context.Context) (engine.Status, error) { return m.cl.SeekBy(ctx, 5000) })
	case key.Matches(msg, m.keys.SeekBack):
		return true, m.statusAction(func(ctx context.Context) (engine.Status, error) { return m.cl.SeekBy(ctx, -5000) })
	case key.Matches(msg, m.keys.SeekForwardL):
		return true, m.statusAction(func(ctx context.Context) (engine.Status, error) { return m.cl.SeekBy(ctx, 30000) })
	case key.Matches(msg, m.keys.SeekBackL):
		return true, m.statusAction(func(ctx context.Context) (engine.Status, error) { return m.cl.SeekBy(ctx, -30000) })
	case key.Matches(msg, m.keys.Remove):
		if len(m.queue.Tracks) == 0 {
			return true, nil
		}
		idx := m.cursor
		return true, m.runAction(func(ctx context.Context) (*engine.Status, *engine.Queue, error) {
			st, err := m.cl.QueueRemove(ctx, idx)
			if err != nil {
				return nil, nil, err
			}
			q, _ := m.cl.Queue(ctx)
			return &st, &q, nil
		}, false)
	case key.Matches(msg, m.keys.MoveUp):
		if m.cursor <= 0 {
			return true, nil
		}
		from := m.cursor
		return true, m.runAction(func(ctx context.Context) (*engine.Status, *engine.Queue, error) {
			st, err := m.cl.QueueMove(ctx, from, from-1)
			if err != nil {
				return nil, nil, err
			}
			q, _ := m.cl.Queue(ctx)
			return &st, &q, nil
		}, false)
	case key.Matches(msg, m.keys.MoveDown):
		if m.cursor >= len(m.queue.Tracks)-1 {
			return true, nil
		}
		from := m.cursor
		return true, m.runAction(func(ctx context.Context) (*engine.Status, *engine.Queue, error) {
			st, err := m.cl.QueueMove(ctx, from, from+1)
			if err != nil {
				return nil, nil, err
			}
			q, _ := m.cl.Queue(ctx)
			return &st, &q, nil
		}, false)
	case key.Matches(msg, m.keys.Clear):
		if len(m.queue.Tracks) == 0 {
			return true, nil
		}
		m.confirm = confirmState{kind: confirmClearQueue, label: "Clear the entire queue and stop playback?"}
		m.dirty = true
		return true, nil
	}
	return false, nil
}

func (m *Model) handleBrowserKey(msg tea.KeyMsg) (bool, tea.Cmd) {
	switch {
	case key.Matches(msg, m.keys.Down):
		m.moveBrowserCursor(1)
		return true, nil
	case key.Matches(msg, m.keys.Up):
		m.moveBrowserCursor(-1)
		return true, nil
	case key.Matches(msg, m.keys.Top):
		if idx := m.browserIndices(); len(idx) > 0 {
			m.browserCursor = idx[0]
			m.browserTop = 0
			m.dirty = true
		}
		return true, nil
	case key.Matches(msg, m.keys.Bottom):
		if idx := m.browserIndices(); len(idx) > 0 {
			m.browserCursor = idx[len(idx)-1]
			m.ensureBrowserCursorVisible()
			m.dirty = true
		}
		return true, nil
	case key.Matches(msg, m.keys.Enter):
		e, ok := m.selectedBrowserEntry()
		if !ok {
			return true, nil
		}
		if e.IsDir {
			return true, m.browseCmd(e.Path)
		}
		return true, m.playPathCmd(e.Path)
	case key.Matches(msg, m.keys.Back):
		if m.browser.Parent != "" {
			return true, m.browseCmd(m.browser.Parent)
		}
		return true, nil
	case key.Matches(msg, m.keys.Append):
		if e, ok := m.selectedBrowserEntry(); ok {
			return true, m.appendPathCmd(e.Path)
		}
		return true, nil
	case key.Matches(msg, m.keys.PlayDir):
		if e, ok := m.selectedBrowserEntry(); ok {
			return true, m.playPathCmd(e.Path)
		}
		return true, nil
	case msg.String() == "r":
		return true, m.browseCmd(m.browser.Path)
	}
	return false, nil
}

func (m *Model) handlePlaylistsKey(msg tea.KeyMsg) (bool, tea.Cmd) {
	switch {
	case key.Matches(msg, m.keys.Down):
		m.movePlaylistCursor(1)
		return true, nil
	case key.Matches(msg, m.keys.Up):
		m.movePlaylistCursor(-1)
		return true, nil
	case key.Matches(msg, m.keys.Top):
		m.playlistCursor = 0
		m.playlistTop = 0
		m.dirty = true
		return true, nil
	case key.Matches(msg, m.keys.Bottom):
		m.playlistCursor = max(0, len(m.playlists)-1)
		m.ensurePlaylistCursorVisible()
		m.dirty = true
		return true, nil
	case key.Matches(msg, m.keys.Enter):
		row, ok := m.currentPlaylistRow()
		if !ok {
			return true, nil
		}
		target := row.Target
		return true, m.runAction(func(ctx context.Context) (*engine.Status, *engine.Queue, error) {
			st, err := m.cl.PlaylistPlay(ctx, target, false)
			if err != nil {
				return nil, nil, err
			}
			q, _ := m.cl.Queue(ctx)
			return &st, &q, nil
		}, false)
	case msg.String() == " ":
		row, ok := m.currentPlaylistRow()
		if !ok {
			return true, nil
		}
		return true, m.inspectCmd(row.Target)
	case key.Matches(msg, m.keys.Remove):
		row, ok := m.currentPlaylistRow()
		if !ok {
			return true, nil
		}
		m.confirm = confirmState{
			kind:   confirmDeletePlaylist,
			target: row.Target,
			label:  "Delete " + row.Kind + " " + row.Name + "?",
		}
		m.dirty = true
		return true, nil
	case msg.String() == "r":
		m.playlistsLoaded = false
		return true, m.playlistsCmd()
	}
	return false, nil
}

func (m *Model) playPathCmd(path string) tea.Cmd {
	return m.statusAction(func(ctx context.Context) (engine.Status, error) {
		return m.cl.Play(ctx, path, false)
	})
}

func (m *Model) appendPathCmd(path string) tea.Cmd {
	return m.runAction(func(ctx context.Context) (*engine.Status, *engine.Queue, error) {
		st, err := m.cl.QueueAppend(ctx, []string{path})
		if err != nil {
			return nil, nil, err
		}
		q, _ := m.cl.Queue(ctx)
		return &st, &q, nil
	}, false)
}

func (m *Model) handleConfirmKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "y", "Y", "enter":
		c := m.confirm
		m.confirm = confirmState{}
		m.dirty = true
		switch c.kind {
		case confirmQuitDaemon:
			return m, m.runAction(func(ctx context.Context) (*engine.Status, *engine.Queue, error) {
				return nil, nil, m.cl.Shutdown(ctx)
			}, true)
		case confirmClearQueue:
			return m, m.runAction(func(ctx context.Context) (*engine.Status, *engine.Queue, error) {
				st, err := m.cl.QueueClear(ctx)
				if err != nil {
					return nil, nil, err
				}
				q, _ := m.cl.Queue(ctx)
				return &st, &q, nil
			}, false)
		case confirmDeletePlaylist:
			target := c.target
			m.playlistsLoaded = false
			m.preview = engine.PlaylistView{}
			return m, m.deleteAndRefreshCmd(target)
		}
		return m, nil
	case "n", "N", "esc", "q":
		m.confirm = confirmState{}
		m.dirty = true
		return m, nil
	case "ctrl+c":
		m.Close()
		return m, tea.Quit
	}
	return m, nil
}

func (m *Model) deleteAndRefreshCmd(target string) tea.Cmd {
	return func() tea.Msg {
		if m.cl == nil {
			return actionResultMsg{err: apperr.New(apperr.ServiceNotRunning, "no client configured")}
		}
		ctx, cancel := context.WithTimeout(m.ctx, 4*time.Second)
		defer cancel()
		if err := m.cl.PlaylistRemove(ctx, target); err != nil {
			return actionResultMsg{err: err}
		}
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

// --- input line -----------------------------------------------------------

func (m *Model) openInput(mode inputMode, prefix string) {
	m.inputMode = mode
	m.inputPrev = ""
	m.inputText = ""
	switch mode {
	case inputQueueFilter:
		m.inputText = m.queueFilter
		m.inputPrev = m.queueFilter
	case inputBrowserFilter:
		m.inputText = m.browserFilter
		m.inputPrev = m.browserFilter
	}
	_ = prefix
	m.dirty = true
}

func (m *Model) closeInput() {
	m.inputMode = inputNone
	m.inputText = ""
	m.inputPrev = ""
	m.dirty = true
}

func (m *Model) handleInputKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc":
		if m.inputMode == inputQueueFilter {
			m.queueFilter = m.inputPrev
			m.ensureQueueCursorVisible()
		} else if m.inputMode == inputBrowserFilter {
			m.browserFilter = m.inputPrev
			m.ensureBrowserCursorVisible()
		}
		m.closeInput()
		return m, nil
	case "enter":
		mode := m.inputMode
		text := m.inputText
		switch mode {
		case inputCommand:
			m.closeInput()
			return m, m.executeCommand(text)
		case inputQueueFilter:
			m.queueFilter = strings.TrimSpace(text)
			m.queueTop = 0
			m.clampCursorToFilter()
		case inputBrowserFilter:
			m.browserFilter = strings.TrimSpace(text)
			m.browserTop = 0
		}
		m.closeInput()
		return m, nil
	case "backspace", "ctrl+h":
		runes := []rune(m.inputText)
		if len(runes) > 0 {
			m.inputText = string(runes[:len(runes)-1])
		}
		m.dirty = true
		return m, nil
	case "ctrl+u":
		m.inputText = ""
		m.dirty = true
		return m, nil
	}
	if len(msg.Runes) > 0 {
		m.inputText += string(msg.Runes)
		m.dirty = true
	}
	return m, nil
}

func (m *Model) clampCursorToFilter() {
	idx := m.queueIndices()
	if len(idx) == 0 {
		return
	}
	for _, i := range idx {
		if i == m.cursor {
			return
		}
	}
	m.cursor = idx[0]
}

// --- command line ---------------------------------------------------------

func (m *Model) executeCommand(line string) tea.Cmd {
	line = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), ":"))
	if line == "" {
		return nil
	}
	cmd, arg, _ := strings.Cut(line, " ")
	arg = strings.TrimSpace(arg)
	switch strings.ToLower(cmd) {
	case "q", "quit":
		m.Close()
		return tea.Quit
	case "theme":
		if arg == "" {
			m.infoBanner = "themes: " + strings.Join(themeNames(), ", ")
			m.dirty = true
			return nil
		}
		if !m.SetTheme(arg) {
			m.errBanner = "unknown theme " + strconv.Quote(arg) + "; try: " + strings.Join(themeNames(), ", ")
		} else {
			m.infoBanner = "theme: " + m.Theme()
		}
		m.dirty = true
		return nil
	case "open", "play":
		if arg == "" {
			m.errBanner = "usage: :open <path>"
			m.dirty = true
			return nil
		}
		return m.playPathCmd(arg)
	case "seek":
		if arg == "" {
			m.errBanner = "usage: :seek <seconds|+5s|-30s|1:20>"
			m.dirty = true
			return nil
		}
		if delta, ok := parseSeekDelta(arg); ok {
			return m.statusAction(func(ctx context.Context) (engine.Status, error) { return m.cl.SeekBy(ctx, delta) })
		}
		if ms, ok := parseSeekPosition(arg); ok {
			return m.statusAction(func(ctx context.Context) (engine.Status, error) { return m.cl.SeekTo(ctx, ms) })
		}
		m.errBanner = "invalid seek argument " + strconv.Quote(arg)
		m.dirty = true
		return nil
	case "clear":
		return m.runAction(func(ctx context.Context) (*engine.Status, *engine.Queue, error) {
			st, err := m.cl.QueueClear(ctx)
			if err != nil {
				return nil, nil, err
			}
			q, _ := m.cl.Queue(ctx)
			return &st, &q, nil
		}, false)
	case "queue":
		return m.setView(viewQueue)
	case "files", "browse":
		return m.setView(viewBrowser)
	case "playlists":
		return m.setView(viewPlaylists)
	case "filter":
		m.queueFilter = arg
		m.queueTop = 0
		m.clampCursorToFilter()
		m.dirty = true
		return nil
	case "pause":
		return m.statusAction(func(ctx context.Context) (engine.Status, error) { return m.cl.Pause(ctx) })
	case "stop":
		return m.statusAction(func(ctx context.Context) (engine.Status, error) { return m.cl.Stop(ctx) })
	case "next":
		return m.runAction(func(ctx context.Context) (*engine.Status, *engine.Queue, error) {
			st, err := m.cl.Next(ctx)
			if err != nil {
				return nil, nil, err
			}
			q, _ := m.cl.Queue(ctx)
			return &st, &q, nil
		}, false)
	case "prev":
		return m.runAction(func(ctx context.Context) (*engine.Status, *engine.Queue, error) {
			st, err := m.cl.Previous(ctx)
			if err != nil {
				return nil, nil, err
			}
			q, _ := m.cl.Queue(ctx)
			return &st, &q, nil
		}, false)
	case "mute":
		return m.statusAction(func(ctx context.Context) (engine.Status, error) { return m.cl.Mute(ctx) })
	case "shuffle":
		return m.statusAction(func(ctx context.Context) (engine.Status, error) { return m.cl.Shuffle(ctx) })
	case "loop":
		return m.statusAction(func(ctx context.Context) (engine.Status, error) { return m.cl.Loop(ctx) })
	case "help":
		m.showFullHelp = true
		m.dirty = true
		return nil
	default:
		m.errBanner = "unknown command :" + cmd
		m.dirty = true
		return nil
	}
}

func parseSeekDelta(s string) (int64, bool) {
	s = strings.TrimSpace(strings.ToLower(s))
	if s == "" {
		return 0, false
	}
	sign := int64(1)
	if s[0] == '+' {
		s = s[1:]
	} else if s[0] == '-' {
		sign = -1
		s = s[1:]
	} else {
		return 0, false
	}
	ms, ok := parseDurationMs(s)
	if !ok {
		return 0, false
	}
	return sign * ms, true
}

func parseDurationMs(s string) (int64, bool) {
	s = strings.TrimSpace(strings.ToLower(s))
	if s == "" {
		return 0, false
	}
	mult := int64(1000)
	switch {
	case strings.HasSuffix(s, "ms"):
		s = strings.TrimSuffix(s, "ms")
		mult = 1
	case strings.HasSuffix(s, "s"):
		s = strings.TrimSuffix(s, "s")
	case strings.HasSuffix(s, "m"):
		s = strings.TrimSuffix(s, "m")
		mult = 60 * 1000
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil || v < 0 {
		return 0, false
	}
	return int64(v * float64(mult)), true
}

func parseSeekPosition(s string) (int64, bool) {
	s = strings.TrimSpace(s)
	if strings.Contains(s, ":") {
		parts := strings.Split(s, ":")
		var total float64
		for _, p := range parts {
			v, err := strconv.ParseFloat(strings.TrimSpace(p), 64)
			if err != nil || v < 0 {
				return 0, false
			}
			total = total*60 + v
		}
		return int64(total * 1000), true
	}
	return parseDurationMs(s)
}
