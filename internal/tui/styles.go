// Package tui implements the built-in Bubble Tea terminal UI client.
package tui

import (
	"github.com/charmbracelet/lipgloss"
)

// styles holds preallocated Lip Gloss styles so View() never allocates styles
// on the render path.
type styles struct {
	app          lipgloss.Style
	header       lipgloss.Style
	badgePlay    lipgloss.Style
	badgePause   lipgloss.Style
	badgeStop    lipgloss.Style
	badgeOffline lipgloss.Style
	sectionTitle lipgloss.Style
	trackTitle   lipgloss.Style
	metaLabel    lipgloss.Style
	metaValue    lipgloss.Style
	pillOn       lipgloss.Style
	pillOff      lipgloss.Style
	queueCurrent lipgloss.Style
	queueItem    lipgloss.Style
	queueIndex   lipgloss.Style
	errorBanner  lipgloss.Style
	confirmBox   lipgloss.Style
	footer       lipgloss.Style
}

var defaultStyles = newStyles()

func newStyles() styles {
	return styles{
		app: lipgloss.NewStyle().Padding(0, 1),
		header: lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("230")).
			Background(lipgloss.Color("62")).
			Padding(0, 1),
		badgePlay: lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("16")).
			Background(lipgloss.Color("42")).
			Padding(0, 1),
		badgePause: lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("16")).
			Background(lipgloss.Color("214")).
			Padding(0, 1),
		badgeStop: lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("252")).
			Background(lipgloss.Color("240")).
			Padding(0, 1),
		badgeOffline: lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("231")).
			Background(lipgloss.Color("196")).
			Padding(0, 1),
		sectionTitle: lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("111")),
		trackTitle: lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("229")),
		metaLabel: lipgloss.NewStyle().
			Foreground(lipgloss.Color("244")),
		metaValue: lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("252")),
		pillOn: lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("16")).
			Background(lipgloss.Color("81")).
			Padding(0, 1),
		pillOff: lipgloss.NewStyle().
			Foreground(lipgloss.Color("243")).
			Background(lipgloss.Color("236")).
			Padding(0, 1),
		queueCurrent: lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("42")),
		queueItem: lipgloss.NewStyle().
			Foreground(lipgloss.Color("250")),
		queueIndex: lipgloss.NewStyle().
			Foreground(lipgloss.Color("241")),
		errorBanner: lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("231")).
			Background(lipgloss.Color("160")).
			Padding(0, 1),
		confirmBox: lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("16")).
			Background(lipgloss.Color("214")).
			Padding(0, 1),
		footer: lipgloss.NewStyle().
			Foreground(lipgloss.Color("243")),
	}
}
