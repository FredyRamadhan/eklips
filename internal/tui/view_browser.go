package tui

import (
	"fmt"
	"strings"

	"github.com/frdn/eklips/internal/engine"
)

func (m *Model) renderBrowser(b *strings.Builder) {
	b.WriteString(m.styles.sectionTitle.Render("File Browser") + "\n")
	if m.browser.Path == "" {
		b.WriteString("  " + m.styles.metaLabel.Render("Loading directory…") + "\n")
		return
	}
	pathLine := m.styles.browserPath.Render(m.browser.Path)
	if m.browser.Root != "" && m.browser.Path == m.browser.Root {
		pathLine += " " + m.styles.dim.Render("(media root)")
	}
	if m.browserFilter != "" {
		pathLine += "  " + m.styles.metaLabel.Render("filter: "+m.browserFilter)
	}
	b.WriteString("  " + pathLine + "\n\n")

	idx := m.browserIndices()
	if len(idx) == 0 {
		b.WriteString("  " + m.styles.metaLabel.Render("(no music or folders)") + "\n")
		return
	}
	rows := m.browserViewRows()
	start := m.browserTop
	if start >= len(idx) {
		start = max(0, len(idx)-1)
	}
	end := min(len(idx), start+rows)
	for pos := start; pos < end; pos++ {
		i := idx[pos]
		e := m.browser.Entries[i]
		selected := i == m.browserCursor
		cursor := "  "
		if selected {
			cursor = m.styles.cursorBar.Render("❯") + " "
		}
		icon := "🎵"
		name := e.Name
		style := m.styles.fileEntry
		if e.IsDir {
			icon = "📁"
			name = e.Name + "/"
			style = m.styles.dirEntry
		} else if disp := e.Metadata.Display(); disp != "" {
			name = disp
		}
		if selected {
			style = m.styles.cursorItem
			if e.IsDir {
				style = m.styles.cursorCurrent
			}
		}
		extra := ""
		if !e.IsDir && e.Size > 0 {
			extra = "  " + m.styles.dim.Render(formatBytes(e.Size))
		}
		b.WriteString(fmt.Sprintf("%s%s %s%s\n", cursor, icon, style.Render(name), extra))
	}

	if e, ok := m.selectedBrowserEntry(); ok && e.Audio {
		b.WriteString("\n")
		b.WriteString(m.renderBrowsePreview(e))
	}
}

func (m *Model) renderBrowsePreview(e engine.BrowseEntry) string {
	md := e.Metadata
	rows := []string{}
	add := func(label, value string) {
		if strings.TrimSpace(value) != "" {
			rows = append(rows, m.styles.metaLabel.Render(label+": ")+m.styles.metaValue.Render(value))
		}
	}
	add("Title", md.Title)
	add("Artist", md.Artist)
	add("Album", md.Album)
	add("Year", md.Year)
	if md.TrackNumber > 0 {
		add("Track", fmt.Sprintf("%d", md.TrackNumber))
	}
	add("Size", formatBytes(e.Size))
	add("Path", e.Path)
	if len(rows) == 0 {
		rows = append(rows, m.styles.metaLabel.Render(e.Name))
	}
	return m.styles.previewBox.Render(strings.Join(rows, "\n"))
}
