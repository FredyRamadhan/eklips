package tui

import (
	"fmt"
	"math"
	"strings"

	"github.com/frdn/eklips/internal/core"
)

// View renders the TUI. It reuses the cached string when no state has changed.
func (m *Model) View() string {
	if !m.dirty && m.cachedView != "" {
		return m.cachedView
	}
	var b strings.Builder
	b.Grow(2048)

	b.WriteString(m.renderHeader())
	b.WriteByte('\n')
	b.WriteString(m.renderTabs())
	b.WriteString("\n\n")

	switch m.view {
	case viewBrowser:
		m.renderBrowser(&b)
	case viewPlaylists:
		m.renderPlaylists(&b)
	default:
		m.renderQueue(&b)
	}

	if m.confirm.kind != confirmNone {
		b.WriteString("\n" + m.styles.confirmBox.Render(m.confirm.label+" [y/N]") + "\n")
	} else {
		if m.errBanner != "" {
			b.WriteString("\n" + m.styles.errorBanner.Render("Error: "+m.errBanner) + "\n")
		}
		if m.infoBanner != "" {
			b.WriteString("\n" + m.styles.infoBanner.Render(m.infoBanner) + "\n")
		}
	}
	b.WriteByte('\n')
	if m.inputMode != inputNone {
		b.WriteString(m.renderInputLine())
	} else if m.showFullHelp {
		b.WriteString(m.renderFullHelp())
	} else {
		b.WriteString(m.renderFooter())
	}

	m.cachedView = m.styles.app.Render(b.String())
	m.dirty = false
	return m.cachedView
}

func (m *Model) renderHeader() string {
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
	themeTag := m.styles.headerURL.Render("  theme:" + m.Theme())
	return m.styles.header.Render("eklips  •  "+urlStr) + " " + badge + themeTag
}

func (m *Model) renderTabs() string {
	type tab struct {
		id    viewID
		label string
	}
	tabs := []tab{
		{viewQueue, fmt.Sprintf("1: Queue (%d)", len(m.queue.Tracks))},
		{viewBrowser, "2: Files"},
		{viewPlaylists, fmt.Sprintf("3: Playlists (%d)", len(m.playlists))},
	}
	parts := make([]string, 0, len(tabs))
	for _, t := range tabs {
		if m.view == t.id {
			parts = append(parts, m.styles.tabActive.Render(t.label))
		} else {
			parts = append(parts, m.styles.tabInactive.Render(t.label))
		}
	}
	return strings.Join(parts, " ")
}

func (m *Model) renderQueue(b *strings.Builder) {
	// Now playing card.
	b.WriteString(m.styles.sectionTitle.Render("Now Playing") + "\n")
	if title := m.currentTrackTitle(); title != "" {
		b.WriteString("  " + m.styles.trackTitle.Render(title) + "\n")
		if m.status.SourcePath != "" {
			src := fmt.Sprintf("%s: %s", m.status.SourceType, m.status.SourcePath)
			b.WriteString("  " + m.styles.metaLabel.Render(src) + "\n")
		}
	} else {
		b.WriteString("  " + m.styles.metaLabel.Render("No media loaded — press 2 to browse files") + "\n")
	}

	ratio := 0.0
	if m.status.DurationMs > 0 {
		ratio = float64(m.status.PositionMs) / float64(m.status.DurationMs)
		ratio = math.Max(0, math.Min(1, ratio))
	}
	barW := m.width - 28
	if barW < 10 {
		barW = 10
	}
	if barW > 72 {
		barW = 72
	}
	bar := m.renderBar(ratio, barW)
	timeStr := fmt.Sprintf("%s / %s", formatMillis(m.status.PositionMs), formatMillis(m.status.DurationMs))
	b.WriteString(fmt.Sprintf("  %s  %s\n", bar, m.styles.metaValue.Render(timeStr)))
	b.WriteString(m.renderMeter() + "\n")

	volText := fmt.Sprintf("Vol: %d%%", m.status.Volume)
	volPill := m.styles.pillOn.Render(volText)
	if m.status.Muted {
		volPill = m.styles.pillOff.Render(volText + " MUTED")
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

	// Queue.
	title := fmt.Sprintf("Queue (%d)", len(m.queue.Tracks))
	if m.queueFilter != "" {
		title += "  filter: " + m.queueFilter
	}
	b.WriteString(m.styles.sectionTitle.Render(title) + "\n")
	idx := m.queueIndices()
	if len(idx) == 0 {
		if len(m.queue.Tracks) == 0 {
			b.WriteString("  " + m.styles.metaLabel.Render("(empty)") + "\n")
		} else {
			b.WriteString("  " + m.styles.metaLabel.Render("(no matches)") + "\n")
		}
		return
	}
	visible := m.queueViewRows()
	start := m.queueTop
	if start >= len(idx) {
		start = max(0, len(idx)-1)
	}
	end := min(len(idx), start+visible)
	for pos := start; pos < end; pos++ {
		i := idx[pos]
		item := m.queue.Tracks[i]
		selected := i == m.cursor
		current := item.Index == m.status.TrackIndex || item.Current

		cursor := "  "
		if selected {
			cursor = m.styles.cursorBar.Render("❯") + " "
		}
		marker := " "
		if current {
			marker = m.styles.queueCurrent.Render("▶")
		}
		idxStr := m.styles.queueIndex.Render(fmt.Sprintf("%3d.", item.Index+1))
		name := m.itemTitle(item)
		style := m.styles.queueItem
		switch {
		case selected && current:
			style = m.styles.cursorCurrent
		case selected:
			style = m.styles.cursorItem
		case current:
			style = m.styles.queueCurrent
		}
		b.WriteString(fmt.Sprintf("%s%s %s %s\n", cursor, marker, idxStr, style.Render(name)))
	}
}

func (m *Model) renderBar(ratio float64, width int) string {
	filled := int(math.Round(ratio * float64(width)))
	if filled < 0 {
		filled = 0
	}
	if filled > width {
		filled = width
	}
	return m.styles.meterFill.Render(strings.Repeat("█", filled)) +
		m.styles.meterEmpty.Render(strings.Repeat("░", width-filled))
}

func (m *Model) renderMeter() string {
	const width = 20
	l := meterBar(width, m.levels.Left, m.styles)
	r := meterBar(width, m.levels.Right, m.styles)
	return fmt.Sprintf("  L: %s %6.1f dB   R: %s %6.1f dB",
		l, m.levels.Left, r, m.levels.Right)
}

func meterBar(width int, db float64, st styles) string {
	if db <= -60 {
		db = -60
	}
	pct := (db + 60) / 60
	if pct < 0 {
		pct = 0
	}
	if pct > 1 {
		pct = 1
	}
	filled := int(math.Round(pct * float64(width)))
	if filled < 0 {
		filled = 0
	}
	if filled > width {
		filled = width
	}
	style := st.meterLow
	if db >= -9 {
		style = st.meterHigh
	} else if db >= -24 {
		style = st.meterMid
	}
	return "[" + style.Render(strings.Repeat("█", filled)) +
		st.meterEmpty.Render(strings.Repeat("░", width-filled)) + "]"
}

func (m *Model) renderInputLine() string {
	var prompt, hint string
	switch m.inputMode {
	case inputQueueFilter:
		prompt = "/"
		hint = "filter queue (Enter apply, Esc cancel)"
	case inputBrowserFilter:
		prompt = "/"
		hint = "filter files (Enter apply, Esc cancel)"
	case inputCommand:
		prompt = ":"
		hint = "command (Enter run, Esc cancel)"
	}
	return m.styles.prompt.Render(prompt) + m.styles.input.Render(" "+m.inputText+"▏ ") +
		" " + m.styles.dim.Render(hint)
}

func (m *Model) renderFooter() string {
	var hints []string
	switch m.view {
	case viewQueue:
		hints = []string{
			"space play/pause", "enter play", "j/k move", "h/l seek ±5s", "[/] ±30s",
			"x/d remove", "J/K reorder", "C clear", "/ filter", "1/2/3 view", "? help", "q quit",
		}
	case viewBrowser:
		hints = []string{
			"enter open/play", "a append", "p play folder", "⌫/h parent", "j/k move",
			"/ filter", "r reload", "1/2/3 view", "? help", "q quit",
		}
	case viewPlaylists:
		hints = []string{
			"enter load", "space inspect", "d delete", "j/k move", "r reload",
			"1/2/3 view", "? help", "q quit",
		}
	}
	return m.styles.footer.Render(strings.Join(hints, "  ·  "))
}

func (m *Model) renderFullHelp() string {
	lines := []string{
		"Playback       space play/pause · s stop · n next · p prev · +/- volume · m mute · r shuffle · L loop",
		"Seeking        h/← -5s · l/→ +5s · [ -30s · ] +30s · :seek <time>",
		"Queue          j/k move · g/G top/bottom · Enter play · x/d remove · J/K reorder · C clear · / filter",
		"Files          Enter open/play · a append · p play folder · ⌫/h parent · r reload · / filter",
		"Playlists      Enter load · space inspect · d delete · r reload",
		"Views          1 queue · 2 files · 3 playlists · Tab next · : command · ? help",
		"Commands       :theme <name> · :open <path> · :seek ±5s · :clear · :filter <text> · :queue · :files · :playlists",
		"Quit           q/Esc quit TUI · Q quit daemon",
	}
	return m.styles.infoBanner.Render(strings.Join(lines, "\n"))
}
