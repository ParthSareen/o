package tui

import (
	"fmt"
	"sort"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

var (
	selectorTitleStyle = lipgloss.NewStyle().
				Bold(true)

	selectorItemStyle = lipgloss.NewStyle().
				PaddingLeft(4)

	selectorSelectedItemStyle = lipgloss.NewStyle().
					PaddingLeft(2).
					Bold(true).
					Background(lipgloss.AdaptiveColor{Light: "254", Dark: "236"})

	selectorDescStyle = lipgloss.NewStyle().
				Foreground(lipgloss.AdaptiveColor{Light: "242", Dark: "246"})

	selectorDescLineStyle = selectorDescStyle.
				PaddingLeft(6)

	selectorFilterStyle = lipgloss.NewStyle().
				Foreground(lipgloss.AdaptiveColor{Light: "242", Dark: "246"}).
				Italic(true)

	selectorInputStyle = lipgloss.NewStyle().
				Foreground(lipgloss.AdaptiveColor{Light: "235", Dark: "252"})

	selectorDefaultTagStyle = lipgloss.NewStyle().
				Foreground(lipgloss.AdaptiveColor{Light: "242", Dark: "246"})

	selectorHelpStyle = lipgloss.NewStyle().
				Foreground(lipgloss.AdaptiveColor{Light: "244", Dark: "244"})

	selectorMoreStyle = lipgloss.NewStyle().
				PaddingLeft(6).
				Foreground(lipgloss.AdaptiveColor{Light: "242", Dark: "246"}).
				Italic(true)

	sectionHeaderStyle = lipgloss.NewStyle().
				PaddingLeft(2).
				Bold(true).
				Foreground(lipgloss.AdaptiveColor{Light: "240", Dark: "249"})
)

const maxSelectorItems = 10

type SelectItem struct {
	Name              string
	Description       string
	Recommended       bool
	AvailabilityBadge string
}

type SelectorModel = selectorModel

type selectorItemsUpdatedMsg struct {
	items []SelectItem
}

func waitForSelectorItems(updates <-chan []SelectItem) tea.Cmd {
	if updates == nil {
		return nil
	}
	return func() tea.Msg {
		items, ok := <-updates
		if !ok {
			return nil
		}
		return selectorItemsUpdatedMsg{items: items}
	}
}

// selectorModel is the bubbletea model for single selection.
type selectorModel struct {
	title        string
	items        []SelectItem
	updates      <-chan []SelectItem
	filter       string
	cursor       int
	scrollOffset int
	selected     string
	cancelled    bool
	helpText     string
	width        int
	rankFiltered bool
}

func selectorModelWithCurrent(title string, items []SelectItem, current string) selectorModel {
	m := selectorModel{
		title:  title,
		items:  items,
		cursor: cursorForCurrent(items, current),
	}
	m.updateScroll(m.otherStart())
	return m
}

func NewModelSelectorModel(title string, items []SelectItem, current, filter string) SelectorModel {
	m := selectorModelWithCurrent(title, items, current)
	m.filter = strings.TrimSpace(filter)
	m.rankFiltered = true
	if m.filter != "" {
		m.cursor = 0
		m.scrollOffset = 0
	}
	return m
}

func currentItemName(items []SelectItem, cursor int) string {
	if cursor < 0 || cursor >= len(items) {
		return ""
	}
	return items[cursor].Name
}

func indexOfItemName(items []SelectItem, name string) int {
	for i, item := range items {
		if item.Name == name {
			return i
		}
	}
	return -1
}

func cursorForItemName(items []SelectItem, name string, fallback int) int {
	if len(items) == 0 {
		return 0
	}
	if name != "" {
		if i := indexOfItemName(items, name); i >= 0 {
			return i
		}
	}
	if fallback < 0 {
		return 0
	}
	if fallback >= len(items) {
		return len(items) - 1
	}
	return fallback
}

func (m selectorModel) filteredItems() []SelectItem {
	if m.filter == "" {
		return m.items
	}
	filterLower := strings.ToLower(m.filter)
	var result []SelectItem
	for _, item := range m.items {
		if m.rankFiltered {
			if selectItemMatchScore(item, filterLower).ok {
				result = append(result, item)
			}
			continue
		}
		if strings.Contains(strings.ToLower(item.Name), filterLower) {
			result = append(result, item)
		}
	}
	if m.rankFiltered {
		sortSelectItemsForFilter(result, filterLower)
	}
	return result
}

func (m selectorModel) Init() tea.Cmd {
	return waitForSelectorItems(m.updates)
}

// otherStart returns the index of the first non-recommended item in the filtered list.
// When filtering, all items scroll together so this returns 0.
func (m selectorModel) otherStart() int {
	if m.filter != "" {
		return 0
	}
	filtered := m.filteredItems()
	for i, item := range filtered {
		if !item.Recommended {
			return i
		}
	}
	return len(filtered)
}

// updateNavigation handles navigation keys (up/down/pgup/pgdown/filter/backspace).
// It does NOT handle Enter, Esc, or CtrlC. This is used by both the standalone
// selector and the TUI modal (which intercepts Enter/Esc for its own logic).
func (m *selectorModel) updateNavigation(msg tea.KeyMsg) {
	filtered := m.filteredItems()
	otherStart := m.otherStart()

	switch msg.Type {
	case tea.KeyUp:
		if m.cursor > 0 {
			m.cursor--
			m.updateScroll(otherStart)
		}

	case tea.KeyDown:
		if m.cursor < len(filtered)-1 {
			m.cursor++
			m.updateScroll(otherStart)
		}

	case tea.KeyPgUp:
		m.cursor -= maxSelectorItems
		if m.cursor < 0 {
			m.cursor = 0
		}
		m.updateScroll(otherStart)

	case tea.KeyPgDown:
		m.cursor += maxSelectorItems
		if m.cursor >= len(filtered) {
			m.cursor = len(filtered) - 1
		}
		m.updateScroll(otherStart)

	case tea.KeyBackspace:
		if len(m.filter) > 0 {
			m.filter = m.filter[:len(m.filter)-1]
			m.cursor = 0
			m.scrollOffset = 0
		}

	case tea.KeyRunes:
		m.filter += string(msg.Runes)
		m.cursor = 0
		m.scrollOffset = 0
	}
}

func (m *selectorModel) UpdateNavigation(msg tea.KeyMsg) {
	m.updateNavigation(msg)
}

func (m *selectorModel) Move(delta int) {
	if delta == 0 {
		return
	}
	filtered := m.filteredItems()
	if len(filtered) == 0 {
		m.cursor = 0
		m.scrollOffset = 0
		return
	}
	m.cursor += delta
	if m.cursor < 0 {
		m.cursor = 0
	}
	if m.cursor >= len(filtered) {
		m.cursor = len(filtered) - 1
	}
	m.updateScroll(m.otherStart())
}

func (m *selectorModel) SetHelpText(help string) {
	m.helpText = help
}

func (m selectorModel) Filter() string {
	return m.filter
}

func (m selectorModel) FilteredItems() []SelectItem {
	return append([]SelectItem(nil), m.filteredItems()...)
}

func (m selectorModel) SelectedItem() (SelectItem, bool) {
	filtered := m.filteredItems()
	if len(filtered) == 0 || m.cursor < 0 || m.cursor >= len(filtered) {
		return SelectItem{}, false
	}
	return filtered[m.cursor], true
}

func (m selectorModel) RenderContent() string {
	return m.renderContent()
}

// updateScroll adjusts scrollOffset based on cursor position.
// When not filtering, scrollOffset is relative to the "More" (non-recommended) section.
// When filtering, it's relative to the full filtered list.
func (m *selectorModel) updateScroll(otherStart int) {
	if m.filter != "" {
		if m.cursor < m.scrollOffset {
			m.scrollOffset = m.cursor
		}
		if m.cursor >= m.scrollOffset+maxSelectorItems {
			m.scrollOffset = m.cursor - maxSelectorItems + 1
		}
		return
	}

	// Cursor is in recommended section — reset "More" scroll to top
	if m.cursor < otherStart {
		m.scrollOffset = 0
		return
	}

	// Cursor is in "More" section — scroll relative to others
	posInOthers := m.cursor - otherStart
	maxOthers := maxSelectorItems - otherStart
	if maxOthers < 3 {
		maxOthers = 3
	}
	if posInOthers < m.scrollOffset {
		m.scrollOffset = posInOthers
	}
	if posInOthers >= m.scrollOffset+maxOthers {
		m.scrollOffset = posInOthers - maxOthers + 1
	}
}

func (m selectorModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		wasSet := m.width > 0
		m.width = msg.Width
		if wasSet {
			return m, tea.ClearScreen
		}
		return m, nil

	case selectorItemsUpdatedMsg:
		current := currentItemName(m.filteredItems(), m.cursor)
		m.items = msg.items
		m.cursor = cursorForItemName(m.filteredItems(), current, m.cursor)
		m.updateScroll(m.otherStart())
		return m, waitForSelectorItems(m.updates)

	case tea.KeyMsg:
		switch msg.Type {
		case tea.KeyCtrlC, tea.KeyEsc:
			m.cancelled = true
			return m, tea.Quit

		case tea.KeyLeft:
			m.cancelled = true
			return m, tea.Quit

		case tea.KeyEnter:
			filtered := m.filteredItems()
			if len(filtered) > 0 && m.cursor < len(filtered) {
				m.selected = filtered[m.cursor].Name
			}
			return m, tea.Quit

		default:
			m.updateNavigation(msg)
		}
	}

	return m, nil
}

func cursorItemSuffix(item SelectItem) string {
	if item.AvailabilityBadge == "" {
		return ""
	}
	return " " + selectorDefaultTagStyle.Render("("+item.AvailabilityBadge+")")
}

func (m selectorModel) renderItem(s *strings.Builder, item SelectItem, idx int) {
	if idx == m.cursor {
		s.WriteString(selectorSelectedItemStyle.Render("▸ " + item.Name))
		s.WriteString(cursorItemSuffix(item))
	} else {
		s.WriteString(selectorItemStyle.Render(item.Name))
	}
	s.WriteString("\n")
	if item.Description != "" {
		s.WriteString(selectorDescLineStyle.Render(item.Description))
		s.WriteString("\n")
	}
}

func (m selectorModel) renderCompactItem(s *strings.Builder, item SelectItem, idx int) {
	if idx == m.cursor {
		s.WriteString(selectorSelectedItemStyle.Render("▸ " + item.Name))
		s.WriteString(cursorItemSuffix(item))
	} else {
		s.WriteString(selectorItemStyle.Render(item.Name))
	}
	s.WriteString("\n")
}

// renderContent renders the selector content (title, items, help text) without
// checking the cancelled/selected state. This is used by both View() (standalone mode)
// and by the TUI modal which embeds a selectorModel.
func (m selectorModel) renderContent() string {
	var s strings.Builder

	s.WriteString(selectorTitleStyle.Render(m.title))
	s.WriteString(" ")
	if m.filter == "" {
		s.WriteString(selectorFilterStyle.Render("Type to filter..."))
	} else {
		s.WriteString(selectorInputStyle.Render(m.filter))
	}
	s.WriteString("\n\n")

	filtered := m.filteredItems()

	if len(filtered) == 0 {
		s.WriteString(selectorItemStyle.Render(selectorDescStyle.Render("(no matches)")))
		s.WriteString("\n")
	} else if m.filter != "" {
		s.WriteString(sectionHeaderStyle.Render("Top Results"))
		s.WriteString("\n")

		displayCount := min(len(filtered), maxSelectorItems)
		for i := range displayCount {
			idx := m.scrollOffset + i
			if idx >= len(filtered) {
				break
			}
			m.renderItem(&s, filtered[idx], idx)
		}

		if remaining := len(filtered) - m.scrollOffset - displayCount; remaining > 0 {
			s.WriteString(selectorMoreStyle.Render(fmt.Sprintf("... and %d more", remaining)))
			s.WriteString("\n")
		}
	} else {
		// Split into pinned recommended and scrollable others
		var recItems, otherItems []int
		for i, item := range filtered {
			if item.Recommended {
				recItems = append(recItems, i)
			} else {
				otherItems = append(otherItems, i)
			}
		}

		// Always render all recommended items (pinned)
		if len(recItems) > 0 {
			s.WriteString(sectionHeaderStyle.Render("Recommended"))
			s.WriteString("\n")
			for _, idx := range recItems {
				m.renderItem(&s, filtered[idx], idx)
			}
		}

		if len(otherItems) > 0 {
			s.WriteString("\n")
			s.WriteString(sectionHeaderStyle.Render("More"))
			s.WriteString("\n")

			maxOthers := maxSelectorItems - len(recItems)
			if maxOthers < 3 {
				maxOthers = 3
			}
			displayCount := min(len(otherItems), maxOthers)

			for i := range displayCount {
				idx := m.scrollOffset + i
				if idx >= len(otherItems) {
					break
				}
				m.renderItem(&s, filtered[otherItems[idx]], otherItems[idx])
			}

			if remaining := len(otherItems) - m.scrollOffset - displayCount; remaining > 0 {
				s.WriteString(selectorMoreStyle.Render(fmt.Sprintf("... and %d more", remaining)))
				s.WriteString("\n")
			}
		}
	}

	s.WriteString("\n")
	help := "↑/↓ navigate • enter select • ← back"
	if m.helpText != "" {
		help = m.helpText
	}
	s.WriteString(selectorHelpStyle.Render(help))

	return s.String()
}

func (m selectorModel) RenderCompactContent(maxItems int) string {
	var s strings.Builder

	s.WriteString(selectorTitleStyle.Render(m.title))
	s.WriteString(" ")
	if m.filter == "" {
		s.WriteString(selectorFilterStyle.Render("Type to filter..."))
	} else {
		s.WriteString(selectorInputStyle.Render(m.filter))
	}
	s.WriteString("\n")

	filtered := m.filteredItems()
	if len(filtered) == 0 {
		s.WriteString(selectorItemStyle.Render(selectorDescStyle.Render("(no matches)")))
		s.WriteString("\n")
	} else {
		maxItems = max(1, maxItems)
		start := 0
		if len(filtered) > maxItems {
			start = m.cursor - maxItems/2
			if start < 0 {
				start = 0
			}
			if maxStart := len(filtered) - maxItems; start > maxStart {
				start = maxStart
			}
		}
		end := min(len(filtered), start+maxItems)
		if start > 0 {
			s.WriteString(selectorMoreStyle.Render(fmt.Sprintf("... %d more above", start)))
			s.WriteString("\n")
		}
		for idx := start; idx < end; idx++ {
			m.renderCompactItem(&s, filtered[idx], idx)
		}
		if remaining := len(filtered) - end; remaining > 0 {
			s.WriteString(selectorMoreStyle.Render(fmt.Sprintf("... and %d more", remaining)))
			s.WriteString("\n")
		}
	}

	help := "↑/↓ navigate • enter select • esc cancel"
	if m.helpText != "" {
		help = m.helpText
	}
	s.WriteString(selectorHelpStyle.Render(help))

	return s.String()
}

func (m selectorModel) View() string {
	if m.cancelled || m.selected != "" {
		return ""
	}

	s := m.renderContent()
	if m.width > 0 {
		return lipgloss.NewStyle().MaxWidth(m.width).Render(s)
	}
	return s
}

type selectItemScore struct {
	ok          bool
	rank        int
	index       int
	lengthDelta int
	recommended int
	name        string
}

func sortSelectItemsForFilter(items []SelectItem, filter string) {
	filter = strings.ToLower(strings.TrimSpace(filter))
	sort.SliceStable(items, func(i, j int) bool {
		return compareSelectItemsForFilter(items[i], items[j], filter) < 0
	})
}

func compareSelectItemsForFilter(a, b SelectItem, filter string) int {
	aScore := selectItemMatchScore(a, filter)
	bScore := selectItemMatchScore(b, filter)
	for _, cmp := range []int{
		compareSelectorInt(aScore.rank, bScore.rank),
		compareSelectorInt(aScore.index, bScore.index),
		compareSelectorInt(aScore.lengthDelta, bScore.lengthDelta),
		compareSelectorInt(aScore.recommended, bScore.recommended),
		strings.Compare(aScore.name, bScore.name),
	} {
		if cmp != 0 {
			return cmp
		}
	}
	return 0
}

func selectItemMatchScore(item SelectItem, filter string) selectItemScore {
	filter = strings.ToLower(strings.TrimSpace(filter))
	name := strings.ToLower(strings.TrimSpace(item.Name))
	description := strings.ToLower(strings.TrimSpace(item.Description))
	score := selectItemScore{
		rank:        4,
		index:       1 << 20,
		lengthDelta: 1 << 20,
		name:        name,
	}
	if item.Recommended {
		score.recommended = -1
	}
	if filter == "" {
		score.ok = true
		return score
	}
	nameRunes := len([]rune(name))
	filterRunes := len([]rune(filter))
	if name == filter {
		score.ok = true
		score.rank = 0
		score.index = 0
		score.lengthDelta = 0
		return score
	}
	if strings.HasPrefix(name, filter) {
		score.ok = true
		score.rank = 1
		score.index = 0
		score.lengthDelta = max(0, nameRunes-filterRunes)
		return score
	}
	if index := strings.Index(name, filter); index >= 0 {
		score.ok = true
		score.rank = 2
		score.index = len([]rune(name[:index]))
		score.lengthDelta = max(0, nameRunes-filterRunes)
		return score
	}
	if index := strings.Index(description, filter); index >= 0 {
		score.ok = true
		score.rank = 3
		score.index = len([]rune(description[:index]))
		score.lengthDelta = max(0, nameRunes-filterRunes)
	}
	return score
}

func compareSelectorInt(a, b int) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	default:
		return 0
	}
}

// cursorForCurrent returns the item index matching current, or 0 if not found.
func cursorForCurrent(items []SelectItem, current string) int {
	if current == "" {
		return 0
	}

	// Prefer exact name matches before tag-prefix fallback so "qwen3.5" does not
	// incorrectly select "qwen3.5:cloud" (and vice versa) based on list order.
	if i := indexOfItemName(items, current); i >= 0 {
		return i
	}

	for i, item := range items {
		if strings.HasPrefix(item.Name, current+":") || strings.HasPrefix(current, item.Name+":") {
			return i
		}
	}

	return 0
}
