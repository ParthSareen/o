package chat

import (
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/alecthomas/chroma/v2/quick"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/mattn/go-runewidth"
	"github.com/muesli/termenv"
)

func renderMarkdownForView(markdown string, width int) string {
	return renderMarkdownForViewWithCodeCache(markdown, width, nil)
}

type markdownCodeBlockCacheKey struct {
	language  string
	source    string
	formatter string
	style     string
}

func renderMarkdownForViewWithCodeCache(markdown string, width int, codeCache *map[markdownCodeBlockCacheKey]string) string {
	if width < 20 {
		width = 20
	}

	source := strings.Split(strings.TrimRight(markdown, "\n"), "\n")
	var rendered []string
	inCodeBlock := false
	codeLanguage := ""
	var codeLines []string
	for i := 0; i < len(source); i++ {
		line := strings.TrimRight(source[i], "\r")
		trimmed := strings.TrimSpace(line)

		if strings.HasPrefix(trimmed, "```") {
			if inCodeBlock {
				rendered = append(rendered, renderMarkdownCodeBlock(codeLines, codeLanguage, width, codeCache, true)...)
				codeLines = nil
				inCodeBlock = false
				codeLanguage = ""
			} else {
				inCodeBlock = true
				codeLanguage = markdownCodeFenceLanguage(trimmed)
			}
			continue
		}
		if inCodeBlock {
			codeLines = append(codeLines, line)
			continue
		}

		if table, consumed := renderMarkdownTable(source[i:], width); consumed > 0 {
			rendered = append(rendered, table...)
			i += consumed - 1
			continue
		}

		if markdownThematicBreak(trimmed) {
			rendered = append(rendered, chatMetaStyle.Render(strings.Repeat("─", width)))
			continue
		}
		if markdownBlockquoteStart(line) {
			if quoted, consumed := renderMarkdownBlockquote(source[i:], width); consumed > 0 {
				rendered = append(rendered, quoted...)
				i += consumed - 1
				continue
			}
		}
		if _, ok := markdownListItemMatch(line); ok {
			if listed, consumed := renderMarkdownList(source[i:], width); consumed > 0 {
				rendered = append(rendered, listed...)
				i += consumed - 1
				continue
			}
		}

		if heading, ok := markdownHeading(trimmed); ok {
			rendered = append(rendered, chatHeaderStyle.Render(renderMarkdownRunes(parseMarkdownInline(heading))))
			continue
		}

		if trimmed == "" {
			rendered = append(rendered, "")
			continue
		}
		rendered = append(rendered, wrapMarkdownInline(line, width)...)
	}
	if inCodeBlock {
		rendered = append(rendered, renderMarkdownCodeBlock(codeLines, codeLanguage, width, codeCache, false)...)
	}
	return strings.Join(rendered, "\n")
}

func splitRenderedBody(body string) []string {
	body = strings.TrimRight(body, "\n")
	if body == "" {
		return []string{""}
	}
	return strings.Split(body, "\n")
}

func markdownHeading(line string) (string, bool) {
	if !strings.HasPrefix(line, "#") {
		return "", false
	}
	level := 0
	for level < len(line) && line[level] == '#' {
		level++
	}
	if level == 0 || level > 6 || level >= len(line) || line[level] != ' ' {
		return "", false
	}
	return strings.TrimSpace(line[level:]), true
}

type markdownInlineStyle uint8

const (
	markdownPlain markdownInlineStyle = iota
	markdownStrong
	markdownCode
	markdownLink
	markdownImage
)

type markdownInlineRune struct {
	r     rune
	style markdownInlineStyle
}

// wrapMarkdownInline parses a complete source line before wrapping it. That
// keeps emphasis intact when its opening and closing delimiters land on
// different visual lines.
func wrapMarkdownInline(line string, width int) []string {
	return wrapInlineRunes(parseMarkdownInline(line), width)
}

func wrapInlineRunes(runes []markdownInlineRune, width int) []string {
	if len(runes) == 0 {
		return []string{""}
	}

	var rendered []string
	for len(runes) > 0 {
		hardCut, spaceCut, currentWidth := 0, 0, 0
		for i, item := range runes {
			nextWidth := currentWidth + runewidth.RuneWidth(item.r)
			if nextWidth > width {
				break
			}
			currentWidth = nextWidth
			hardCut = i + 1
			if unicode.IsSpace(item.r) && currentWidth > width/2 {
				spaceCut = i
			}
		}
		cut := hardCut
		if spaceCut > 0 {
			cut = spaceCut
		}
		if cut == 0 {
			cut = 1
		}

		lineRunes := trimMarkdownSpace(runes[:cut])
		rendered = append(rendered, renderMarkdownRunes(lineRunes))
		runes = trimMarkdownSpace(runes[cut:])
	}
	return rendered
}

func parseMarkdownInline(line string) []markdownInlineRune {
	var out []markdownInlineRune
	for len(line) > 0 {
		if strings.HasPrefix(line, "![") {
			if text, rest, ok := markdownImageParts(line); ok {
				out = appendMarkdownRunes(out, text, markdownImage)
				line = rest
				continue
			}
		}
		if strings.HasPrefix(line, "[") {
			if text, rest, ok := markdownLinkParts(line); ok {
				out = appendMarkdownRunes(out, text, markdownLink)
				line = rest
				continue
			}
		}
		if strings.HasPrefix(line, "`") {
			if end := strings.Index(line[1:], "`"); end >= 0 {
				out = appendMarkdownRunes(out, line[1:end+1], markdownCode)
				line = line[end+2:]
				continue
			}
		}
		if (strings.HasPrefix(line, "**") || strings.HasPrefix(line, "__")) && canOpenMarkdownStrong(out) {
			delimiter := line[:2]
			if end := strings.Index(line[2:], delimiter); end >= 0 {
				out = appendMarkdownRunes(out, line[2:end+2], markdownStrong)
				line = line[end+4:]
				continue
			}
		}

		r, size := utf8.DecodeRuneInString(line)
		out = append(out, markdownInlineRune{r: r, style: markdownPlain})
		line = line[size:]
	}
	return out
}

// markdownLinkParts parses a Markdown link of the form [text](target) (the
// target may carry an optional "title"). It returns the link text, the
// remaining source after the link, and ok.
func markdownLinkParts(line string) (string, string, bool) {
	return markdownLinkLike(line, "[")
}

// markdownImageParts parses a Markdown image ![alt](src) and returns the alt
// text, the remaining source, and ok.
func markdownImageParts(line string) (string, string, bool) {
	return markdownLinkLike(line, "![")
}

// markdownLinkLike is the shared bracket-matching core for links and images.
// open is the leading delimiter ("[" or "![").
func markdownLinkLike(line, open string) (string, string, bool) {
	if !strings.HasPrefix(line, open) {
		return "", line, false
	}
	close, ok := markdownMatchBracket(line, len(open), '[', ']')
	if !ok || close+1 >= len(line) || line[close+1] != '(' {
		return "", line, false
	}
	end, ok := markdownMatchBracket(line, close+2, '(', ')')
	if !ok {
		return "", line, false
	}
	return line[len(open):close], line[end+1:], true
}

// markdownMatchBracket finds the index of the bracket that closes the one at
// start, honoring backslash escapes and nesting.
func markdownMatchBracket(line string, start int, open, close byte) (int, bool) {
	depth := 1
	i := start
	for i < len(line) {
		switch line[i] {
		case '\\':
			i += 2
			continue
		case open:
			depth++
		case close:
			depth--
			if depth == 0 {
				return i, true
			}
		}
		i++
	}
	return 0, false
}

// canOpenMarkdownStrong keeps delimiter-like text in bare URLs and identifiers
// literal, only treating ** / __ as strong emphasis at the common
// whitespace- or punctuation-delimited form.
func canOpenMarkdownStrong(out []markdownInlineRune) bool {
	if len(out) == 0 {
		return true
	}
	previous := out[len(out)-1].r
	return (unicode.IsSpace(previous) || unicode.IsPunct(previous)) && !markdownStrongInURL(out)
}

func markdownStrongInURL(out []markdownInlineRune) bool {
	start := len(out)
	for start > 0 && !unicode.IsSpace(out[start-1].r) {
		start--
	}

	var token strings.Builder
	for _, item := range out[start:] {
		token.WriteRune(item.r)
	}
	return strings.Contains(token.String(), "://")
}

func appendMarkdownRunes(out []markdownInlineRune, text string, style markdownInlineStyle) []markdownInlineRune {
	for _, r := range text {
		out = append(out, markdownInlineRune{r: r, style: style})
	}
	return out
}

func trimMarkdownSpace(runes []markdownInlineRune) []markdownInlineRune {
	start, end := 0, len(runes)
	for start < end && unicode.IsSpace(runes[start].r) {
		start++
	}
	for end > start && unicode.IsSpace(runes[end-1].r) {
		end--
	}
	return runes[start:end]
}

func renderMarkdownRunes(runes []markdownInlineRune) string {
	var b strings.Builder
	for start := 0; start < len(runes); {
		end := start + 1
		for end < len(runes) && runes[end].style == runes[start].style {
			end++
		}
		var text strings.Builder
		for _, item := range runes[start:end] {
			text.WriteRune(item.r)
		}
		switch runes[start].style {
		case markdownStrong:
			b.WriteString(chatStrongStyle.Render(text.String()))
		case markdownCode:
			b.WriteString(chatInlineCodeStyle.Render(text.String()))
		case markdownLink:
			b.WriteString(chatLinkStyle.Render(text.String()))
		case markdownImage:
			b.WriteString(chatImageStyle.Render(text.String()))
		default:
			b.WriteString(text.String())
		}
		start = end
	}
	return b.String()
}

// markdownThematicBreak reports whether a line is a Markdown thematic break
// (---, ***, ___ with three or more matching characters and optional spaces).
func markdownThematicBreak(line string) bool {
	line = strings.TrimSpace(line)
	if len(line) < 3 {
		return false
	}
	var ch byte
	switch line[0] {
	case '-', '*', '_':
		ch = line[0]
	default:
		return false
	}
	count := 0
	for i := 0; i < len(line); i++ {
		switch line[i] {
		case ch:
			count++
		case ' ', '\t':
			continue
		default:
			return false
		}
	}
	return count >= 3
}

// markdownBlockquoteStart reports whether a line begins a Markdown blockquote.
func markdownBlockquoteStart(line string) bool {
	return strings.HasPrefix(strings.TrimSpace(line), ">")
}

// renderMarkdownBlockquote consumes a run of '>'-prefixed lines, strips one
// level of quote marker, and renders the inner Markdown recursively (so
// nested blockquotes, lists, and tables work) under a faint rule prefix.
func renderMarkdownBlockquote(lines []string, width int) ([]string, int) {
	var inner []string
	consumed := 0
	for consumed < len(lines) {
		trimmed := strings.TrimSpace(lines[consumed])
		if !strings.HasPrefix(trimmed, ">") {
			break
		}
		body := strings.TrimPrefix(trimmed, ">")
		body = strings.TrimPrefix(body, " ")
		inner = append(inner, body)
		consumed++
	}
	if len(inner) == 0 {
		return nil, 0
	}
	innerWidth := max(1, width-2)
	rendered := renderMarkdownForView(strings.Join(inner, "\n"), innerWidth)
	prefix := chatBlockquoteStyle.Render("▎") + " "
	out := make([]string, 0, len(rendered))
	for _, line := range strings.Split(rendered, "\n") {
		out = append(out, prefix+line)
	}
	return out, consumed
}

// markdownListMarker describes a single list-item line's marker.
type markdownListMarker struct {
	indent  int
	ordered bool
	number  string
	marker  string
	content string
}

// markdownListItemMatch parses a line into a list marker, returning ok=false
// for lines that are not list items. A space (or end-of-line) must follow the
// marker, so values like "1.5" or "value__x__" are not mistaken for lists.
func markdownListItemMatch(line string) (markdownListMarker, bool) {
	indent := 0
	for indent < len(line) && line[indent] == ' ' {
		indent++
	}
	rest := line[indent:]
	if rest == "" {
		return markdownListMarker{}, false
	}
	var m markdownListMarker
	m.indent = indent
	switch rest[0] {
	case '-', '*', '+':
		if len(rest) == 1 || (rest[1] != ' ' && rest[1] != '\t') {
			return markdownListMarker{}, false
		}
		m.marker = string(rest[0])
		m.content = strings.TrimLeft(rest[1:], " \t")
		return m, true
	default:
		i := 0
		for i < len(rest) && rest[i] >= '0' && rest[i] <= '9' {
			i++
		}
		if i == 0 || i >= len(rest) {
			return markdownListMarker{}, false
		}
		if rest[i] != '.' && rest[i] != ')' {
			return markdownListMarker{}, false
		}
		if i+1 < len(rest) && rest[i+1] != ' ' && rest[i+1] != '\t' {
			return markdownListMarker{}, false
		}
		m.ordered = true
		m.number = rest[:i]
		m.marker = rest[:i+1]
		m.content = strings.TrimLeft(rest[i+1:], " \t")
		return m, true
	}
}

// renderMarkdownList consumes a list block (siblings at the same base indent,
// plus nested sub-lists and continuation paragraphs) and renders it with
// bullets, numbers, and hanging indents. It recurses for nested lists.
func renderMarkdownList(lines []string, width int) ([]string, int) {
	first, ok := markdownListItemMatch(lines[0])
	if !ok {
		return nil, 0
	}
	baseIndent := first.indent
	var rendered []string
	consumed := 0
	pendingBlank := false
	for consumed < len(lines) {
		line := lines[consumed]
		item, isItem := markdownListItemMatch(line)
		if isItem {
			if item.indent < baseIndent {
				break
			}
			if item.indent > baseIndent {
				sub, subConsumed := renderMarkdownList(lines[consumed:], width)
				if subConsumed > 0 {
					rendered = append(rendered, sub...)
					consumed += subConsumed
					pendingBlank = false
					continue
				}
			}
			if pendingBlank {
				rendered = append(rendered, "")
				pendingBlank = false
			}
			itemLines, itemConsumed := renderMarkdownListItem(lines[consumed:], width)
			rendered = append(rendered, itemLines...)
			consumed += itemConsumed
			continue
		}
		if strings.TrimSpace(line) == "" {
			pendingBlank = true
			consumed++
			continue
		}
		break
	}
	return rendered, consumed
}

// renderMarkdownListItem renders a single item: its first content line with the
// marker, wrapped continuation lines, continuation paragraphs, and any nested
// sub-list that belongs to it. It returns the rendered lines and the number of
// source lines consumed.
func renderMarkdownListItem(lines []string, width int) ([]string, int) {
	item, _ := markdownListItemMatch(lines[0])
	contentIndent := item.indent + len(item.marker) + 1
	bullet := markdownListBullet(item)
	textWidth := max(1, width-contentIndent)

	var rendered []string
	first := wrapMarkdownInline(item.content, textWidth)
	if len(first) == 0 {
		first = []string{""}
	}
	indent := strings.Repeat(" ", item.indent)
	rendered = append(rendered, indent+bullet+" "+first[0])
	contIndent := strings.Repeat(" ", contentIndent)
	for _, l := range first[1:] {
		rendered = append(rendered, contIndent+l)
	}

	consumed := 1
	for consumed < len(lines) {
		line := lines[consumed]
		if strings.TrimSpace(line) == "" {
			next := consumed + 1
			for next < len(lines) && strings.TrimSpace(lines[next]) == "" {
				next++
			}
			if next >= len(lines) {
				break
			}
			nextLine := lines[next]
			nextItem, isNextItem := markdownListItemMatch(nextLine)
			if isNextItem && nextItem.indent >= contentIndent {
				consumed = next
				sub, subConsumed := renderMarkdownList(lines[consumed:], width)
				rendered = append(rendered, sub...)
				consumed += subConsumed
				continue
			}
			if !isNextItem && indentOf(nextLine) >= contentIndent {
				consumed++ // consume the blank line
				para, paraConsumed := markdownContinuationParagraph(lines[consumed:], contentIndent, width)
				if paraConsumed > 0 {
					rendered = append(rendered, "")
					rendered = append(rendered, para...)
					consumed += paraConsumed
					continue
				}
				break
			}
			break
		}
		nested, isNested := markdownListItemMatch(line)
		if isNested && nested.indent >= contentIndent {
			sub, subConsumed := renderMarkdownList(lines[consumed:], width)
			rendered = append(rendered, sub...)
			consumed += subConsumed
			continue
		}
		if !isNested && indentOf(line) >= contentIndent {
			para, paraConsumed := markdownContinuationParagraph(lines[consumed:], contentIndent, width)
			if paraConsumed > 0 {
				rendered = append(rendered, para...)
				consumed += paraConsumed
				continue
			}
		}
		break
	}
	return rendered, consumed
}

// markdownContinuationParagraph collects a run of indented, non-blank, non-item
// lines into a wrapped paragraph indented to contentIndent.
func markdownContinuationParagraph(lines []string, contentIndent, width int) ([]string, int) {
	var para []string
	consumed := 0
	pad := strings.Repeat(" ", contentIndent)
	textWidth := max(1, width-contentIndent)
	for consumed < len(lines) {
		line := lines[consumed]
		if strings.TrimSpace(line) == "" {
			break
		}
		if _, isItem := markdownListItemMatch(line); isItem {
			break
		}
		if indentOf(line) < contentIndent {
			break
		}
		stripped := strings.TrimPrefix(line, pad)
		if len(stripped) == len(line) {
			stripped = strings.TrimLeft(line, " \t")
		}
		para = append(para, stripped)
		consumed++
	}
	if len(para) == 0 {
		return nil, 0
	}
	wrapped := wrapMarkdownInline(strings.Join(para, " "), textWidth)
	out := make([]string, len(wrapped))
	for i, l := range wrapped {
		out[i] = pad + l
	}
	return out, consumed
}

// markdownListBullet returns the bullet glyph for an item, cycling by nesting
// depth. Ordered items use their number followed by a period.
func markdownListBullet(item markdownListMarker) string {
	if item.ordered {
		return item.number + "."
	}
	depth := item.indent / 2
	switch depth % 3 {
	case 1:
		return "◦"
	case 2:
		return "▪"
	default:
		return "•"
	}
}

// indentOf reports the count of leading spaces on a line.
func indentOf(line string) int {
	n := 0
	for n < len(line) && line[n] == ' ' {
		n++
	}
	return n
}

func renderMarkdownCodeBlock(source []string, language string, width int, codeCache *map[markdownCodeBlockCacheKey]string, complete bool) []string {
	codeWidth := max(1, width-2)
	code := strings.Join(source, "\n")
	lines := wrapChatText(code, codeWidth)
	if highlighted, ok := highlightMarkdownCodeBlock(language, code, codeCache, complete); ok {
		lines = wrapHighlightedMarkdownCode(highlighted, codeWidth)
	}
	for i, wrapped := range lines {
		lines[i] = "  " + chatCodeBlockStyle.Render(wrapped)
	}
	return lines
}

func markdownCodeFenceLanguage(fence string) string {
	info := strings.TrimSpace(strings.TrimPrefix(fence, "```"))
	if info == "" {
		return ""
	}
	return strings.Fields(info)[0]
}

func highlightMarkdownCodeBlock(language, source string, codeCache *map[markdownCodeBlockCacheKey]string, complete bool) (string, bool) {
	if language == "" || source == "" || lipgloss.ColorProfile() == termenv.Ascii {
		return source, false
	}

	key := markdownCodeBlockCacheKey{
		language:  language,
		source:    source,
		formatter: markdownCodeFormatter(),
		style:     markdownCodeStyle(),
	}
	if complete && codeCache != nil && *codeCache != nil {
		if highlighted, ok := (*codeCache)[key]; ok {
			return highlighted, true
		}
	}

	var rendered strings.Builder
	if err := quick.Highlight(&rendered, source, language, key.formatter, key.style); err != nil {
		return source, false
	}
	highlighted := strings.TrimSuffix(rendered.String(), "\n")
	if complete && codeCache != nil {
		if *codeCache == nil {
			*codeCache = make(map[markdownCodeBlockCacheKey]string)
		}
		(*codeCache)[key] = highlighted
	}
	return highlighted, true
}

func wrapHighlightedMarkdownCode(highlighted string, width int) []string {
	lines := strings.Split(ansi.Hardwrap(highlighted, width, true), "\n")
	activeStyle := ""
	for i, line := range lines {
		lines[i] = activeStyle + line
		activeStyle = markdownCodeANSIStyle(activeStyle, line)
		if activeStyle != "" && i < len(lines)-1 {
			lines[i] += "\x1b[0m"
		}
	}
	return lines
}

func markdownCodeANSIStyle(activeStyle, line string) string {
	for {
		start := strings.Index(line, "\x1b[")
		if start < 0 {
			return activeStyle
		}
		line = line[start+2:]
		end := strings.IndexByte(line, 'm')
		if end < 0 {
			return activeStyle
		}
		sequence := line[:end]
		if markdownCodeANSIReset(sequence) {
			activeStyle = ""
		} else {
			activeStyle += "\x1b[" + sequence + "m"
		}
		line = line[end+1:]
	}
}

func markdownCodeANSIReset(sequence string) bool {
	if sequence == "" {
		return true
	}
	for _, parameter := range strings.Split(sequence, ";") {
		if parameter == "0" {
			return true
		}
	}
	return false
}

func markdownCodeFormatter() string {
	switch lipgloss.ColorProfile() {
	case termenv.TrueColor:
		return "terminal16m"
	case termenv.ANSI256:
		return "terminal256"
	default:
		return "terminal16"
	}
}

func markdownCodeStyle() string {
	if lipgloss.HasDarkBackground() {
		return "github-dark"
	}
	return "github"
}

func renderMarkdownTable(lines []string, width int) ([]string, int) {
	if len(lines) < 2 || !looksLikeMarkdownTableRow(lines[0]) || !isMarkdownTableSeparator(lines[1]) {
		return nil, 0
	}

	var rows [][]string
	consumed := 0
	for consumed < len(lines) && looksLikeMarkdownTableRow(lines[consumed]) {
		if consumed == 1 && isMarkdownTableSeparator(lines[consumed]) {
			consumed++
			continue
		}
		rows = append(rows, parseMarkdownTableRow(lines[consumed]))
		consumed++
	}
	if len(rows) == 0 {
		return nil, 0
	}

	columnCount := 0
	for _, row := range rows {
		columnCount = max(columnCount, len(row))
	}
	naturalWidths := make([]int, columnCount)
	for _, row := range rows {
		for i := range columnCount {
			cell := ""
			if i < len(row) {
				cell = row[i]
			}
			naturalWidths[i] = max(naturalWidths[i], markdownInlineWidth(cell))
		}
	}
	widths := markdownTableColumnWidths(naturalWidths, width)

	var rendered []string
	for rowIndex, row := range rows {
		wrappedCells := make([][]string, columnCount)
		rowHeight := 1
		for i := range columnCount {
			cell := ""
			if i < len(row) {
				cell = row[i]
			}
			wrappedCells[i] = wrapMarkdownTableCell(cell, widths[i])
			rowHeight = max(rowHeight, len(wrappedCells[i]))
		}
		for lineIndex := range rowHeight {
			cells := make([]string, columnCount)
			for i := range columnCount {
				cellLine := ""
				if lineIndex < len(wrappedCells[i]) {
					cellLine = wrappedCells[i][lineIndex]
				}
				cells[i] = padPlainLine(cellLine, widths[i])
			}
			line := strings.Join(cells, chatTableBorderStyle.Render(" | "))
			if rowIndex == 0 {
				line = chatHeaderStyle.Render(stripANSIForWidth(line))
			}
			rendered = append(rendered, line)
		}
	}
	return rendered, consumed
}

func markdownTableColumnWidths(naturalWidths []int, width int) []int {
	if len(naturalWidths) == 0 {
		return nil
	}
	separatorWidth := max(0, len(naturalWidths)-1) * lipglossWidth(" | ")
	available := max(1, width-separatorWidth)
	widths := make([]int, len(naturalWidths))
	minWidths := make([]int, len(naturalWidths))
	for i, natural := range naturalWidths {
		widths[i] = max(1, natural)
		minWidth := min(widths[i], 12)
		if i == 0 {
			minWidth = min(widths[i], 4)
		}
		minWidths[i] = max(1, minWidth)
	}

	for sumInts(widths) > available {
		index := widestShrinkableColumn(widths, minWidths)
		if index < 0 {
			break
		}
		widths[index]--
	}
	for sumInts(widths) > available {
		index := widestColumn(widths)
		if index < 0 || widths[index] <= 1 {
			break
		}
		widths[index]--
	}
	return widths
}

func widestShrinkableColumn(widths, minWidths []int) int {
	index := -1
	for i, width := range widths {
		if width <= minWidths[i] {
			continue
		}
		if index < 0 || width > widths[index] {
			index = i
		}
	}
	return index
}

func widestColumn(widths []int) int {
	index := -1
	for i, width := range widths {
		if index < 0 || width > widths[index] {
			index = i
		}
	}
	return index
}

func sumInts(values []int) int {
	sum := 0
	for _, value := range values {
		sum += value
	}
	return sum
}

func wrapMarkdownTableCell(cell string, width int) []string {
	lines := wrapInlineRunes(parseMarkdownInline(cell), max(1, width))
	if len(lines) == 0 {
		return []string{""}
	}
	return lines
}

// markdownInlineWidth reports the visible width of a cell once Markdown
// delimiters are parsed away, so columns size to rendered content.
func markdownInlineWidth(cell string) int {
	width := 0
	for _, item := range parseMarkdownInline(cell) {
		width += runewidth.RuneWidth(item.r)
	}
	return width
}

func looksLikeMarkdownTableRow(line string) bool {
	line = strings.TrimSpace(line)
	return strings.Contains(line, "|") && strings.Count(line, "|") >= 1
}

func isMarkdownTableSeparator(line string) bool {
	cells := parseMarkdownTableRow(line)
	if len(cells) == 0 {
		return false
	}
	for _, cell := range cells {
		cell = strings.TrimSpace(cell)
		cell = strings.TrimPrefix(cell, ":")
		cell = strings.TrimSuffix(cell, ":")
		if cell == "" || strings.Trim(cell, "-") != "" {
			return false
		}
	}
	return true
}

func parseMarkdownTableRow(line string) []string {
	line = strings.TrimSpace(line)
	line = strings.TrimPrefix(line, "|")
	line = strings.TrimSuffix(line, "|")
	raw := strings.Split(line, "|")
	cells := make([]string, 0, len(raw))
	for _, cell := range raw {
		cells = append(cells, strings.TrimSpace(cell))
	}
	return cells
}

func padPlainLine(line string, width int) string {
	if extra := width - lipglossWidth(line); extra > 0 {
		return line + strings.Repeat(" ", extra)
	}
	return line
}

func stripANSIForWidth(line string) string {
	return stripChatANSI(line)
}

func lipglossWidth(line string) int {
	return lipgloss.Width(line)
}
