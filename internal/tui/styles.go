// Package tui implements the built-in Bubble Tea terminal music player client.
package tui

import (
	"github.com/charmbracelet/lipgloss"
)

// styles holds preallocated Lip Gloss styles so View() never allocates styles
// on the render path.
type styles struct {
	theme string

	app           lipgloss.Style
	header        lipgloss.Style
	headerURL     lipgloss.Style
	badgePlay     lipgloss.Style
	badgePause    lipgloss.Style
	badgeStop     lipgloss.Style
	badgeOffline  lipgloss.Style
	tabActive     lipgloss.Style
	tabInactive   lipgloss.Style
	sectionTitle  lipgloss.Style
	trackTitle    lipgloss.Style
	trackArtist   lipgloss.Style
	metaLabel     lipgloss.Style
	metaValue     lipgloss.Style
	pillOn        lipgloss.Style
	pillOff       lipgloss.Style
	queueCurrent  lipgloss.Style
	queueItem     lipgloss.Style
	queueIndex    lipgloss.Style
	cursorBar     lipgloss.Style
	cursorItem    lipgloss.Style
	cursorCurrent lipgloss.Style
	dirEntry      lipgloss.Style
	fileEntry     lipgloss.Style
	browserPath   lipgloss.Style
	listSelected  lipgloss.Style
	previewBox    lipgloss.Style
	meterLabel    lipgloss.Style
	meterFill     lipgloss.Style
	meterEmpty    lipgloss.Style
	meterLow      lipgloss.Style
	meterMid      lipgloss.Style
	meterHigh     lipgloss.Style
	errorBanner   lipgloss.Style
	infoBanner    lipgloss.Style
	confirmBox    lipgloss.Style
	footer        lipgloss.Style
	prompt        lipgloss.Style
	input         lipgloss.Style
	dim           lipgloss.Style
}

var defaultStyles = newStyles(paletteFor("default"))

func newStyles(p palette) styles {
	return styles{
		theme: p.Name,
		app:   lipgloss.NewStyle().Padding(0, 1),
		header: lipgloss.NewStyle().
			Bold(true).
			Foreground(p.HeaderFG).
			Background(p.HeaderBG).
			Padding(0, 1),
		headerURL: lipgloss.NewStyle().
			Foreground(p.HeaderFG).
			Background(p.HeaderBG),
		badgePlay: lipgloss.NewStyle().
			Bold(true).
			Foreground(p.DarkText).
			Background(p.Green).
			Padding(0, 1),
		badgePause: lipgloss.NewStyle().
			Bold(true).
			Foreground(p.DarkText).
			Background(p.Yellow).
			Padding(0, 1),
		badgeStop: lipgloss.NewStyle().
			Bold(true).
			Foreground(p.Text).
			Background(p.Surface2).
			Padding(0, 1),
		badgeOffline: lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("231")).
			Background(p.Red).
			Padding(0, 1),
		tabActive: lipgloss.NewStyle().
			Bold(true).
			Foreground(p.DarkText).
			Background(p.Accent).
			Padding(0, 1),
		tabInactive: lipgloss.NewStyle().
			Foreground(p.Muted).
			Background(p.Surface).
			Padding(0, 1),
		sectionTitle: lipgloss.NewStyle().
			Bold(true).
			Foreground(p.Accent),
		trackTitle: lipgloss.NewStyle().
			Bold(true).
			Foreground(p.Text),
		trackArtist: lipgloss.NewStyle().
			Foreground(p.Accent2),
		metaLabel: lipgloss.NewStyle().
			Foreground(p.Muted),
		metaValue: lipgloss.NewStyle().
			Bold(true).
			Foreground(p.Text),
		pillOn: lipgloss.NewStyle().
			Bold(true).
			Foreground(p.DarkText).
			Background(p.Accent).
			Padding(0, 1),
		pillOff: lipgloss.NewStyle().
			Foreground(p.Muted).
			Background(p.Surface).
			Padding(0, 1),
		queueCurrent: lipgloss.NewStyle().
			Bold(true).
			Foreground(p.Green),
		queueItem: lipgloss.NewStyle().
			Foreground(p.Text),
		queueIndex: lipgloss.NewStyle().
			Foreground(p.Muted),
		cursorBar: lipgloss.NewStyle().
			Bold(true).
			Foreground(p.Accent),
		cursorItem: lipgloss.NewStyle().
			Foreground(p.CursorFG).
			Background(p.CursorBG),
		cursorCurrent: lipgloss.NewStyle().
			Bold(true).
			Foreground(p.CursorFG).
			Background(p.CursorBG),
		dirEntry: lipgloss.NewStyle().
			Bold(true).
			Foreground(p.Accent),
		fileEntry: lipgloss.NewStyle().
			Foreground(p.Text),
		browserPath: lipgloss.NewStyle().
			Bold(true).
			Foreground(p.Accent2),
		listSelected: lipgloss.NewStyle().
			Bold(true).
			Foreground(p.CursorFG).
			Background(p.CursorBG),
		previewBox: lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(p.Accent2).
			Padding(0, 1),
		meterLabel: lipgloss.NewStyle().
			Foreground(p.Muted),
		meterFill: lipgloss.NewStyle().
			Foreground(p.Green),
		meterEmpty: lipgloss.NewStyle().
			Foreground(p.BarEmpty),
		meterLow: lipgloss.NewStyle().
			Foreground(p.Green),
		meterMid: lipgloss.NewStyle().
			Foreground(p.Yellow),
		meterHigh: lipgloss.NewStyle().
			Foreground(p.Red),
		errorBanner: lipgloss.NewStyle().
			Bold(true).
			Foreground(p.ErrorFG).
			Background(p.ErrorBG).
			Padding(0, 1),
		infoBanner: lipgloss.NewStyle().
			Foreground(p.Text).
			Background(p.Surface2).
			Padding(0, 1),
		confirmBox: lipgloss.NewStyle().
			Bold(true).
			Foreground(p.DarkText).
			Background(p.Yellow).
			Padding(0, 1),
		footer: lipgloss.NewStyle().
			Foreground(p.Muted),
		prompt: lipgloss.NewStyle().
			Bold(true).
			Foreground(p.DarkText).
			Background(p.Accent).
			Padding(0, 1),
		input: lipgloss.NewStyle().
			Foreground(p.Text).
			Background(p.PromptBG).
			Padding(0, 1),
		dim: lipgloss.NewStyle().
			Foreground(p.Muted),
	}
}
