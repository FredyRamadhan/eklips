package tui

import "github.com/charmbracelet/lipgloss"

// palette is the set of semantic colors used to build a styles bundle.
type palette struct {
	Name     string
	HeaderBG lipgloss.Color
	HeaderFG lipgloss.Color
	Accent   lipgloss.Color
	Accent2  lipgloss.Color
	Green    lipgloss.Color
	Yellow   lipgloss.Color
	Red      lipgloss.Color
	Text     lipgloss.Color
	Muted    lipgloss.Color
	Surface  lipgloss.Color
	Surface2 lipgloss.Color
	DarkText lipgloss.Color
	BarFill  lipgloss.Color
	BarEmpty lipgloss.Color
	CursorBG lipgloss.Color
	CursorFG lipgloss.Color
	ErrorFG  lipgloss.Color
	ErrorBG  lipgloss.Color
	PromptBG lipgloss.Color
}

var themes = map[string]palette{
	"default": {
		Name:     "default",
		HeaderBG: lipgloss.Color("62"),
		HeaderFG: lipgloss.Color("230"),
		Accent:   lipgloss.Color("45"),  // electric cyan
		Accent2:  lipgloss.Color("201"), // magenta
		Green:    lipgloss.Color("42"),
		Yellow:   lipgloss.Color("214"),
		Red:      lipgloss.Color("196"),
		Text:     lipgloss.Color("252"),
		Muted:    lipgloss.Color("244"),
		Surface:  lipgloss.Color("236"),
		Surface2: lipgloss.Color("238"),
		DarkText: lipgloss.Color("16"),
		BarFill:  lipgloss.Color("45"),
		BarEmpty: lipgloss.Color("238"),
		CursorBG: lipgloss.Color("62"),
		CursorFG: lipgloss.Color("231"),
		ErrorFG:  lipgloss.Color("231"),
		ErrorBG:  lipgloss.Color("160"),
		PromptBG: lipgloss.Color("236"),
	},
	"nord": {
		Name:     "nord",
		HeaderBG: lipgloss.Color("24"),
		HeaderFG: lipgloss.Color("231"),
		Accent:   lipgloss.Color("81"),  // frost cyan
		Accent2:  lipgloss.Color("111"), // frost blue
		Green:    lipgloss.Color("150"),
		Yellow:   lipgloss.Color("222"),
		Red:      lipgloss.Color("167"),
		Text:     lipgloss.Color("253"),
		Muted:    lipgloss.Color("110"),
		Surface:  lipgloss.Color("236"),
		Surface2: lipgloss.Color("238"),
		DarkText: lipgloss.Color("236"),
		BarFill:  lipgloss.Color("81"),
		BarEmpty: lipgloss.Color("238"),
		CursorBG: lipgloss.Color("24"),
		CursorFG: lipgloss.Color("254"),
		ErrorFG:  lipgloss.Color("231"),
		ErrorBG:  lipgloss.Color("131"),
		PromptBG: lipgloss.Color("236"),
	},
	"gruvbox": {
		Name:     "gruvbox",
		HeaderBG: lipgloss.Color("94"),
		HeaderFG: lipgloss.Color("230"),
		Accent:   lipgloss.Color("214"), // warm amber
		Accent2:  lipgloss.Color("109"), // forest green
		Green:    lipgloss.Color("142"),
		Yellow:   lipgloss.Color("214"),
		Red:      lipgloss.Color("167"),
		Text:     lipgloss.Color("223"),
		Muted:    lipgloss.Color("137"),
		Surface:  lipgloss.Color("235"),
		Surface2: lipgloss.Color("237"),
		DarkText: lipgloss.Color("235"),
		BarFill:  lipgloss.Color("214"),
		BarEmpty: lipgloss.Color("237"),
		CursorBG: lipgloss.Color("94"),
		CursorFG: lipgloss.Color("230"),
		ErrorFG:  lipgloss.Color("230"),
		ErrorBG:  lipgloss.Color("124"),
		PromptBG: lipgloss.Color("235"),
	},
	"tokyonight": {
		Name:     "tokyonight",
		HeaderBG: lipgloss.Color("24"),
		HeaderFG: lipgloss.Color("189"),
		Accent:   lipgloss.Color("51"),  // neon cyan
		Accent2:  lipgloss.Color("141"), // neon purple
		Green:    lipgloss.Color("120"),
		Yellow:   lipgloss.Color("221"),
		Red:      lipgloss.Color("203"),
		Text:     lipgloss.Color("189"),
		Muted:    lipgloss.Color("61"),
		Surface:  lipgloss.Color("234"),
		Surface2: lipgloss.Color("236"),
		DarkText: lipgloss.Color("234"),
		BarFill:  lipgloss.Color("141"),
		BarEmpty: lipgloss.Color("236"),
		CursorBG: lipgloss.Color("24"),
		CursorFG: lipgloss.Color("195"),
		ErrorFG:  lipgloss.Color("231"),
		ErrorBG:  lipgloss.Color("89"),
		PromptBG: lipgloss.Color("234"),
	},
	"catppuccin": {
		Name:     "catppuccin",
		HeaderBG: lipgloss.Color("60"),
		HeaderFG: lipgloss.Color("230"),
		Accent:   lipgloss.Color("116"), // mocha teal
		Accent2:  lipgloss.Color("183"), // mocha mauve
		Green:    lipgloss.Color("151"),
		Yellow:   lipgloss.Color("223"),
		Red:      lipgloss.Color("211"),
		Text:     lipgloss.Color("223"),
		Muted:    lipgloss.Color("146"),
		Surface:  lipgloss.Color("235"),
		Surface2: lipgloss.Color("237"),
		DarkText: lipgloss.Color("235"),
		BarFill:  lipgloss.Color("116"),
		BarEmpty: lipgloss.Color("237"),
		CursorBG: lipgloss.Color("60"),
		CursorFG: lipgloss.Color("230"),
		ErrorFG:  lipgloss.Color("230"),
		ErrorBG:  lipgloss.Color("132"),
		PromptBG: lipgloss.Color("235"),
	},
}

// themeNames returns the built-in theme names in display order.
func themeNames() []string {
	return []string{"default", "nord", "gruvbox", "tokyonight", "catppuccin"}
}

// paletteFor resolves a theme name case-insensitively, falling back to default.
func paletteFor(name string) palette {
	for k, v := range themes {
		if equalFoldASCII(k, name) {
			return v
		}
	}
	return themes["default"]
}

func equalFoldASCII(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		ca, cb := a[i], b[i]
		if ca >= 'A' && ca <= 'Z' {
			ca += 'a' - 'A'
		}
		if cb >= 'A' && cb <= 'Z' {
			cb += 'a' - 'A'
		}
		if ca != cb {
			return false
		}
	}
	return true
}
