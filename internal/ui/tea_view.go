package ui

import tea "charm.land/bubbletea/v2"

type teaView = tea.View

func newTeaView(content string) tea.View {
	view := tea.NewView(content)
	view.AltScreen = true
	return view
}
