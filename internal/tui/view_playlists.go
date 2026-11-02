package tui

import (
	"fmt"
	"strings"
	"time"
)

func (m *Model) renderPlaylists(b *strings.Builder) {
	b.WriteString(m.styles.sectionTitle.Render("Playlists & Aliases") + "\n")
	if !m.playlistsLoaded {
		b.WriteString("  " + m.styles.metaLabel.Render("Loading…") + "\n")
		return
	}
	if len(m.playlists) == 0 {
		b.WriteString("  " + m.styles.metaLabel.Render("(no saved playlists or aliases)") + "\n")
		return
	}
	rows := m.playlistsViewRows()
	start := m.playlistTop
	if start >= len(m.playlists) {
		start = max(0, len(m.playlists)-1)
	}
	end := min(len(m.playlists), start+rows)
	for i := start; i < end; i++ {
		row := m.playlists[i]
		selected := i == m.playlistCursor
		cursor := "  "
		if selected {
			cursor = m.styles.cursorBar.Render("❯") + " "
		}
		tag := m.styles.pillOff.Render(row.Kind)
		name := m.styles.listSelected.Render(row.Name)
		if !selected {
			name = m.styles.queueItem.Render(row.Name)
		}
		extra := ""
		if row.Kind == "playlist" {
			extra = "  " + m.styles.metaLabel.Render(fmt.Sprintf("%d tracks", row.TrackCount))
			if !row.Updated.IsZero() {
				extra += "  " + m.styles.dim.Render(row.Updated.Local().Format(time.DateOnly))
			}
		} else {
			extra = "  " + m.styles.metaLabel.Render("alias")
		}
		b.WriteString(fmt.Sprintf("%s%s %s%s\n", cursor, tag, name, extra))
	}

	if len(m.preview.Tracks) > 0 {
		b.WriteString("\n")
		b.WriteString(m.renderPlaylistPreview())
	}
}

func (m *Model) renderPlaylistPreview() string {
	const maxTracks = 10
	var lines []string
	title := m.preview.Name
	if title == "" {
		title = m.preview.SourcePath
	}
	lines = append(lines, m.styles.sectionTitle.Render(title))
	for i, t := range m.preview.Tracks {
		if i >= maxTracks {
			lines = append(lines, m.styles.dim.Render(fmt.Sprintf("… %d more", len(m.preview.Tracks)-maxTracks)))
			break
		}
		name := t.Metadata.Display()
		if name == "" {
			name = t.Name
		}
		lines = append(lines, m.styles.metaLabel.Render(fmt.Sprintf("%2d. ", t.Index+1))+m.styles.queueItem.Render(name))
	}
	return m.styles.previewBox.Render(strings.Join(lines, "\n"))
}
