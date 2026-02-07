package app

import (
	"fmt"

	"github.com/charmbracelet/lipgloss"
)

type themePalette struct {
	Name           string
	BorderColor    string
	AccentColor    string
	MutedColor     string
	TabActiveBG    string
	TabActiveFG    string
	TabInactiveFG  string
	WhitespaceTint string
}

var palettes = []themePalette{
	{
		Name:           "Ocean",
		BorderColor:    "240",
		AccentColor:    "63",
		MutedColor:     "241",
		TabActiveBG:    "57",
		TabActiveFG:    "230",
		TabInactiveFG:  "240",
		WhitespaceTint: "235",
	},
	{
		Name:           "Forest",
		BorderColor:    "65",
		AccentColor:    "41",
		MutedColor:     "72",
		TabActiveBG:    "34",
		TabActiveFG:    "230",
		TabInactiveFG:  "108",
		WhitespaceTint: "236",
	},
	{
		Name:           "Amber",
		BorderColor:    "179",
		AccentColor:    "214",
		MutedColor:     "137",
		TabActiveBG:    "172",
		TabActiveFG:    "232",
		TabInactiveFG:  "179",
		WhitespaceTint: "236",
	},
}

func (m *Model) setTheme(index int) {
	if len(palettes) == 0 {
		return
	}
	if index < 0 {
		index = 0
	}
	m.themeIndex = index % len(palettes)
	applyPalette(palettes[m.themeIndex])
	m.themeName = palettes[m.themeIndex].Name
}

func (m *Model) cycleTheme() {
	m.setTheme(m.themeIndex + 1)
	m.setActionStatus(fmt.Sprintf("Theme switched to %s", m.themeName), nil)
}

func applyPalette(p themePalette) {
	barStyle = lipgloss.NewStyle().Foreground(lipgloss.Color(p.BorderColor)).Padding(0, 1)
	boxStyle = lipgloss.NewStyle().Border(lipgloss.NormalBorder()).BorderForeground(lipgloss.Color(p.BorderColor)).Padding(0, 1)
	placeholder = lipgloss.NewStyle().Foreground(lipgloss.Color(p.BorderColor))
	tabActive = lipgloss.NewStyle().Padding(0, 2).Foreground(lipgloss.Color(p.TabActiveFG)).Background(lipgloss.Color(p.TabActiveBG)).Bold(true)
	tabInactive = lipgloss.NewStyle().Padding(0, 2).Foreground(lipgloss.Color(p.TabInactiveFG))
	modalStyle = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(lipgloss.Color(p.AccentColor)).Padding(1, 2)
	modalTitle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color(p.AccentColor))
	modalHint = lipgloss.NewStyle().Foreground(lipgloss.Color(p.MutedColor))
	logoStyle = lipgloss.NewStyle().Foreground(lipgloss.Color(p.BorderColor)).PaddingRight(1)
}
