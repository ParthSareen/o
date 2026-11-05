package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func items(names ...string) []SelectItem {
	var out []SelectItem
	for _, n := range names {
		out = append(out, SelectItem{Name: n})
	}
	return out
}

func recItems(names ...string) []SelectItem {
	var out []SelectItem
	for _, n := range names {
		out = append(out, SelectItem{Name: n, Recommended: true})
	}
	return out
}

func mixedItems() []SelectItem {
	return []SelectItem{
		{Name: "rec-a", Recommended: true},
		{Name: "rec-b", Recommended: true},
		{Name: "other-1"},
		{Name: "other-2"},
		{Name: "other-3"},
		{Name: "other-4"},
		{Name: "other-5"},
		{Name: "other-6"},
		{Name: "other-7"},
		{Name: "other-8"},
		{Name: "other-9"},
		{Name: "other-10"},
	}
}

func TestFilteredItems(t *testing.T) {
	tests := []struct {
		name   string
		items  []SelectItem
		filter string
		want   []string
	}{
		{
			name:   "no filter returns all",
			items:  items("alpha", "beta", "gamma"),
			filter: "",
			want:   []string{"alpha", "beta", "gamma"},
		},
		{
			name:   "filter matches substring",
			items:  items("llama3.2", "qwen3:8b", "llama2"),
			filter: "llama",
			want:   []string{"llama3.2", "llama2"},
		},
		{
			name:   "filter is case insensitive",
			items:  items("Qwen3:8b", "llama3.2"),
			filter: "QWEN",
			want:   []string{"Qwen3:8b"},
		},
		{
			name:   "no matches",
			items:  items("alpha", "beta"),
			filter: "zzz",
			want:   nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := selectorModel{items: tt.items, filter: tt.filter}
			got := m.filteredItems()
			var gotNames []string
			for _, item := range got {
				gotNames = append(gotNames, item.Name)
			}
			if len(gotNames) != len(tt.want) {
				t.Fatalf("got %v, want %v", gotNames, tt.want)
			}
			for i := range tt.want {
				if gotNames[i] != tt.want[i] {
					t.Errorf("index %d: got %q, want %q", i, gotNames[i], tt.want[i])
				}
			}
		})
	}
}

func TestOtherStart(t *testing.T) {
	tests := []struct {
		name   string
		items  []SelectItem
		filter string
		want   int
	}{
		{
			name:  "all recommended",
			items: recItems("a", "b", "c"),
			want:  3,
		},
		{
			name:  "none recommended",
			items: items("a", "b"),
			want:  0,
		},
		{
			name: "mixed",
			items: []SelectItem{
				{Name: "rec-a", Recommended: true},
				{Name: "rec-b", Recommended: true},
				{Name: "other-1"},
				{Name: "other-2"},
			},
			want: 2,
		},
		{
			name:  "empty",
			items: nil,
			want:  0,
		},
		{
			name: "filtering returns 0",
			items: []SelectItem{
				{Name: "rec-a", Recommended: true},
				{Name: "other-1"},
			},
			filter: "rec",
			want:   0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := selectorModel{items: tt.items, filter: tt.filter}
			if got := m.otherStart(); got != tt.want {
				t.Errorf("otherStart() = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestUpdateScroll(t *testing.T) {
	tests := []struct {
		name       string
		cursor     int
		offset     int
		otherStart int
		filter     string
		wantOffset int
	}{
		{
			name:       "cursor in recommended resets scroll",
			cursor:     1,
			offset:     5,
			otherStart: 3,
			wantOffset: 0,
		},
		{
			name:       "cursor at start of others",
			cursor:     2,
			offset:     0,
			otherStart: 2,
			wantOffset: 0,
		},
		{
			name:       "cursor scrolls down in others",
			cursor:     12,
			offset:     0,
			otherStart: 2,
			wantOffset: 3, // posInOthers=10, maxOthers=8, 10-8+1=3
		},
		{
			name:       "cursor scrolls up in others",
			cursor:     4,
			offset:     5,
			otherStart: 2,
			wantOffset: 2, // posInOthers=2 < offset=5
		},
		{
			name:       "filter mode standard scroll down",
			cursor:     12,
			offset:     0,
			filter:     "x",
			otherStart: 0,
			wantOffset: 3, // 12 - 10 + 1 = 3
		},
		{
			name:       "filter mode standard scroll up",
			cursor:     2,
			offset:     5,
			filter:     "x",
			otherStart: 0,
			wantOffset: 2,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := selectorModel{
				cursor:       tt.cursor,
				scrollOffset: tt.offset,
				filter:       tt.filter,
			}
			m.updateScroll(tt.otherStart)
			if m.scrollOffset != tt.wantOffset {
				t.Errorf("scrollOffset = %d, want %d", m.scrollOffset, tt.wantOffset)
			}
		})
	}
}

func TestSelectorModelWithCurrent_ScrollsToCurrentInMoreSection(t *testing.T) {
	m := selectorModelWithCurrent("Pick:", mixedItems(), "other-10")

	if m.cursor != 11 {
		t.Fatalf("cursor = %d, want 11", m.cursor)
	}
	if m.scrollOffset == 0 {
		t.Fatal("scrollOffset should move to reveal current item in More section")
	}

	content := m.renderContent()
	if !strings.Contains(content, "▸ other-10") {
		t.Fatalf("expected current item to be visible and highlighted\n%s", content)
	}
}

func TestSelectorModelWithCurrent_HighlightsExactLocalWhenCloudVariantExists(t *testing.T) {
	m := selectorModelWithCurrent("Pick:", []SelectItem{
		{Name: "qwen3.5:cloud", Recommended: true},
		{Name: "qwen3.5", Recommended: true},
	}, "qwen3.5")

	if m.cursor != 1 {
		t.Fatalf("cursor = %d, want 1", m.cursor)
	}

	content := m.renderContent()
	if !strings.Contains(content, "▸ qwen3.5") {
		t.Fatalf("expected local qwen3.5 to be highlighted\n%s", content)
	}
	if strings.Contains(content, "▸ qwen3.5:cloud") {
		t.Fatalf("did not expect cloud qwen3.5:cloud to be highlighted\n%s", content)
	}
}

func TestRenderContent_SectionHeaders(t *testing.T) {
	m := selectorModel{
		title: "Pick:",
		items: []SelectItem{
			{Name: "rec-a", Recommended: true},
			{Name: "other-1"},
		},
	}
	content := m.renderContent()

	if !strings.Contains(content, "Recommended") {
		t.Error("should contain 'Recommended' header")
	}
	if !strings.Contains(content, "More") {
		t.Error("should contain 'More' header")
	}
}

func TestRenderContent_FilteredHeader(t *testing.T) {
	m := selectorModel{
		title:  "Pick:",
		items:  items("alpha", "beta", "alphabet"),
		filter: "alpha",
	}
	content := m.renderContent()

	if !strings.Contains(content, "Top Results") {
		t.Error("filtered view should contain 'Top Results' header")
	}
	if strings.Contains(content, "Recommended") {
		t.Error("filtered view should not contain 'Recommended' header")
	}
}

func TestRenderContent_NoMatches(t *testing.T) {
	m := selectorModel{
		title:  "Pick:",
		items:  items("alpha"),
		filter: "zzz",
	}
	content := m.renderContent()

	if !strings.Contains(content, "(no matches)") {
		t.Error("should show '(no matches)' when filter has no results")
	}
}

func TestRenderContent_SelectedItemIndicator(t *testing.T) {
	m := selectorModel{
		title:  "Pick:",
		items:  items("alpha", "beta"),
		cursor: 0,
	}
	content := m.renderContent()

	if !strings.Contains(content, "▸") {
		t.Error("selected item should have ▸ indicator")
	}
}

func TestRenderContent_AvailabilityBadgeOnlyOnCursor(t *testing.T) {
	m := selectorModel{
		title: "Pick:",
		items: []SelectItem{
			{Name: "kimi-k2.6:cloud", AvailabilityBadge: "Upgrade required"},
			{Name: "qwen3.5:cloud", AvailabilityBadge: "Sign in required"},
			{Name: "glm-5:cloud", AvailabilityBadge: "Included"},
		},
		cursor: 0,
	}
	content := m.renderContent()

	if !strings.Contains(content, "(Upgrade required)") {
		t.Fatalf("cursor badge missing:\n%s", content)
	}
	if strings.Contains(content, "(Sign in required)") {
		t.Fatalf("non-cursor badge should not render:\n%s", content)
	}
	if strings.Contains(content, "Included") {
		t.Fatalf("included badge should not render:\n%s", content)
	}
}

func TestSelectorModel_ItemsUpdatedPreservesCursorAndRendersBadge(t *testing.T) {
	m := selectorModelWithCurrent("Pick:", []SelectItem{
		{Name: "kimi-k2.6:cloud", Recommended: true},
		{Name: "llama3.2"},
	}, "kimi-k2.6:cloud")

	updated, _ := m.Update(selectorItemsUpdatedMsg{items: []SelectItem{
		{Name: "kimi-k2.6:cloud", Recommended: true, AvailabilityBadge: "Upgrade required"},
		{Name: "llama3.2"},
	}})
	fm := updated.(selectorModel)
	if fm.cursor != 0 {
		t.Fatalf("cursor = %d, want 0", fm.cursor)
	}
	content := fm.renderContent()
	if !strings.Contains(content, "(Upgrade required)") {
		t.Fatalf("updated badge missing:\n%s", content)
	}
}

func TestRenderContent_Description(t *testing.T) {
	m := selectorModel{
		title: "Pick:",
		items: []SelectItem{
			{Name: "alpha", Description: "the first letter"},
		},
	}
	content := m.renderContent()

	if !strings.Contains(content, "the first letter") {
		t.Error("should render item description")
	}
}

func TestRenderContent_PinnedRecommended(t *testing.T) {
	m := selectorModel{
		title: "Pick:",
		items: mixedItems(),
		// cursor deep in "More" section
		cursor:       8,
		scrollOffset: 3,
	}
	content := m.renderContent()

	// Recommended items should always be visible (pinned)
	if !strings.Contains(content, "rec-a") {
		t.Error("recommended items should always be rendered (pinned)")
	}
	if !strings.Contains(content, "rec-b") {
		t.Error("recommended items should always be rendered (pinned)")
	}
}

func TestRenderContent_MoreOverflowIndicator(t *testing.T) {
	m := selectorModel{
		title: "Pick:",
		items: mixedItems(), // 2 rec + 10 other = 12 total, maxSelectorItems=10
	}
	content := m.renderContent()

	if !strings.Contains(content, "... and") {
		t.Error("should show overflow indicator when more items than visible")
	}
}

func TestUpdateNavigation_CursorBounds(t *testing.T) {
	m := selectorModel{
		items:  items("a", "b", "c"),
		cursor: 0,
	}

	// Up at top stays at 0
	m.updateNavigation(keyMsg(KeyUp))
	if m.cursor != 0 {
		t.Errorf("cursor should stay at 0 when pressing up at top, got %d", m.cursor)
	}

	// Down moves to 1
	m.updateNavigation(keyMsg(KeyDown))
	if m.cursor != 1 {
		t.Errorf("cursor should be 1 after down, got %d", m.cursor)
	}

	// Down to end
	m.updateNavigation(keyMsg(KeyDown))
	m.updateNavigation(keyMsg(KeyDown))
	if m.cursor != 2 {
		t.Errorf("cursor should be 2 at bottom, got %d", m.cursor)
	}
}

func TestUpdateNavigation_FilterResetsState(t *testing.T) {
	m := selectorModel{
		items:        items("alpha", "beta"),
		cursor:       1,
		scrollOffset: 5,
	}

	m.updateNavigation(runeMsg('x'))
	if m.filter != "x" {
		t.Errorf("filter should be 'x', got %q", m.filter)
	}
	if m.cursor != 0 {
		t.Errorf("cursor should reset to 0 on filter, got %d", m.cursor)
	}
	if m.scrollOffset != 0 {
		t.Errorf("scrollOffset should reset to 0 on filter, got %d", m.scrollOffset)
	}
}

func TestUpdateNavigation_Backspace(t *testing.T) {
	m := selectorModel{
		items:  items("alpha"),
		filter: "abc",
		cursor: 1,
	}

	m.updateNavigation(keyMsg(KeyBackspace))
	if m.filter != "ab" {
		t.Errorf("filter should be 'ab' after backspace, got %q", m.filter)
	}
	if m.cursor != 0 {
		t.Errorf("cursor should reset to 0 on backspace, got %d", m.cursor)
	}
}

// --- cursorForCurrent ---

func TestCursorForCurrent(t *testing.T) {
	testItems := []SelectItem{
		{Name: "llama3.2", Recommended: true},
		{Name: "qwen3:8b", Recommended: true},
		{Name: "gemma3:latest"},
		{Name: "deepseek-r1"},
		{Name: "glm-5:cloud"},
	}

	tests := []struct {
		name    string
		current string
		want    int
	}{
		{"empty current", "", 0},
		{"exact match", "qwen3:8b", 1},
		{"no match returns 0", "nonexistent", 0},
		{"bare name matches with :latest suffix", "gemma3", 2},
		{"full tag matches bare item", "llama3.2:latest", 0},
		{"cloud model exact match", "glm-5:cloud", 4},
		{"cloud model bare name", "glm-5", 4},
		{"recommended item exact match", "llama3.2", 0},
		{"recommended item with tag", "qwen3", 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := cursorForCurrent(testItems, tt.current); got != tt.want {
				t.Errorf("cursorForCurrent(%q) = %d, want %d", tt.current, got, tt.want)
			}
		})
	}
}

func TestCursorForCurrent_PrefersExactLocalOverCloudPrefix(t *testing.T) {
	testItems := []SelectItem{
		{Name: "qwen3.5:cloud", Recommended: true},
		{Name: "qwen3.5", Recommended: true},
	}

	if got := cursorForCurrent(testItems, "qwen3.5"); got != 1 {
		t.Errorf("cursorForCurrent(%q) = %d, want %d", "qwen3.5", got, 1)
	}
}

func TestCursorForCurrent_PrefersExactCloudOverLocalPrefix(t *testing.T) {
	testItems := []SelectItem{
		{Name: "qwen3.5", Recommended: true},
		{Name: "qwen3.5:cloud", Recommended: true},
	}

	if got := cursorForCurrent(testItems, "qwen3.5:cloud"); got != 1 {
		t.Errorf("cursorForCurrent(%q) = %d, want %d", "qwen3.5:cloud", got, 1)
	}
}

// --- Left arrow back navigation ---

func TestSelectorLeftArrowCancelsWhenNoFilter(t *testing.T) {
	m := selectorModelWithCurrent("Pick:", items("a", "b", "c"), "")
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyLeft})
	got := updated.(selectorModel)
	if !got.cancelled {
		t.Error("left arrow with empty filter should cancel (go back)")
	}
}

func TestSelectorLeftArrowCancelsWhenFiltering(t *testing.T) {
	m := selectorModelWithCurrent("Pick:", items("a", "b", "c"), "")
	m.filter = "a"
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyLeft})
	got := updated.(selectorModel)
	if !got.cancelled {
		t.Error("left arrow with active filter should still cancel (go back)")
	}
}

// Key message helpers for testing

type keyType = int

const (
	KeyUp        keyType = iota
	KeyDown      keyType = iota
	KeyBackspace keyType = iota
)

func keyMsg(k keyType) tea.KeyMsg {
	switch k {
	case KeyUp:
		return tea.KeyMsg{Type: tea.KeyUp}
	case KeyDown:
		return tea.KeyMsg{Type: tea.KeyDown}
	case KeyBackspace:
		return tea.KeyMsg{Type: tea.KeyBackspace}
	default:
		return tea.KeyMsg{}
	}
}

func runeMsg(r rune) tea.KeyMsg {
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}}
}
