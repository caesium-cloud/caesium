package app

import (
	"fmt"

	"github.com/caesium-cloud/caesium/cmd/console/ui/detail"
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
	SuccessColor   string
	ErrorColor     string
	RunningColor   string
	PendingColor   string
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
		SuccessColor:   "42",
		ErrorColor:     "196",
		RunningColor:   "214",
		PendingColor:   "240",
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
		SuccessColor:   "76",
		ErrorColor:     "160",
		RunningColor:   "178",
		PendingColor:   "242",
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
		SuccessColor:   "114",
		ErrorColor:     "203",
		RunningColor:   "220",
		PendingColor:   "243",
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

// StatusColors holds the current theme's status color values for use by the DAG renderer.
type StatusColors struct {
	Success string
	Error   string
	Running string
	Pending string
	Accent  string
}

var currentStatusColors StatusColors

// CurrentStatusColors returns the active theme's status colors.
func CurrentStatusColors() StatusColors {
	return currentStatusColors
}

func applyPalette(p themePalette) {
	barStyle = lipgloss.NewStyle().Foreground(lipgloss.Color(p.BorderColor)).Padding(0, 1)
	boxStyle = lipgloss.NewStyle().Border(lipgloss.NormalBorder()).BorderForeground(lipgloss.Color(p.BorderColor)).Padding(0, 1)
	placeholder = lipgloss.NewStyle().Foreground(lipgloss.Color(p.BorderColor))
	tabActive = lipgloss.NewStyle().Padding(0, 2).Foreground(lipgloss.Color(p.TabActiveFG)).Background(lipgloss.Color(p.TabActiveBG)).Bold(true)
	tabInactive = lipgloss.NewStyle().Padding(0, 2).Foreground(lipgloss.Color(p.TabInactiveFG))
	tabBarStyle = lipgloss.NewStyle().BorderBottom(true).BorderStyle(lipgloss.NormalBorder()).BorderBottomForeground(lipgloss.Color(p.BorderColor)).PaddingBottom(0).MarginBottom(0)
	summaryStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("252")).Padding(0, 2)
	filterStyle = lipgloss.NewStyle().Foreground(lipgloss.Color(p.RunningColor)).Padding(0, 2)
	modalStyle = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(lipgloss.Color(p.AccentColor)).Padding(1, 2)
	modalTitle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color(p.AccentColor))
	modalHint = lipgloss.NewStyle().Foreground(lipgloss.Color(p.MutedColor))
	logoStyle = lipgloss.NewStyle().Foreground(lipgloss.Color(p.AccentColor)).Bold(true).PaddingRight(1)
	logoDimStyle = lipgloss.NewStyle().Foreground(lipgloss.Color(p.MutedColor))
	currentStatusColors = StatusColors{
		Success: p.SuccessColor,
		Error:   p.ErrorColor,
		Running: p.RunningColor,
		Pending: p.PendingColor,
		Accent:  p.AccentColor,
	}
	detail.SetAccentColor(p.AccentColor)
}
