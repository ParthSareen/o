package chat

import (
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
)

func TestRenderMarkdownLinkStyling(t *testing.T) {
	rendered := renderMarkdownForView("See [the docs](https://example.com/docs) for more.", 80)
	plain := stripANSI(rendered)
	if !strings.Contains(plain, "the docs") {
		t.Fatalf("link text missing: %q", plain)
	}
	if strings.Contains(plain, "https://example.com/docs") {
		t.Fatalf("link target should not render inline: %q", plain)
	}
	if strings.Contains(plain, "[") || strings.Contains(plain, "]") {
		t.Fatalf("link delimiters should not render: %q", plain)
	}
	if !strings.Contains(rendered, chatLinkStyle.Render("the docs")) {
		t.Fatalf("link text should use link style: %q", rendered)
	}
}

func TestRenderMarkdownLinkWithInlineCodeInText(t *testing.T) {
	rendered := renderMarkdownForView("Run [use `make` to build](https://example.com) now.", 80)
	plain := stripANSI(rendered)
	// Link text is rendered verbatim (inline code inside link text is plain).
	if !strings.Contains(plain, "use `make` to build") {
		t.Fatalf("link text content lost: %q", plain)
	}
	if !strings.Contains(rendered, chatLinkStyle.Render("use `make` to build")) {
		t.Fatalf("link text should be styled: %q", rendered)
	}
}

func TestRenderMarkdownImageRendersAlt(t *testing.T) {
	rendered := renderMarkdownForView("Diagram: ![architecture](./diagram.png) end.", 80)
	plain := stripANSI(rendered)
	if !strings.Contains(plain, "architecture") {
		t.Fatalf("image alt text missing: %q", plain)
	}
	if strings.Contains(plain, "diagram.png") {
		t.Fatalf("image src should not render: %q", plain)
	}
	if !strings.Contains(rendered, chatImageStyle.Render("architecture")) {
		t.Fatalf("image alt should use image style: %q", rendered)
	}
}

func TestRenderMarkdownUnorderedList(t *testing.T) {
	markdown := strings.Join([]string{
		"- first item",
		"- second item with **bold**",
		"- third `code` item",
	}, "\n")
	rendered := renderMarkdownForView(markdown, 80)
	plain := stripANSI(rendered)
	for _, want := range []string{"first item", "second item with bold", "third code item"} {
		if !strings.Contains(plain, want) {
			t.Fatalf("list item missing %q:\n%s", want, plain)
		}
	}
	if strings.Contains(plain, "- ") {
		t.Fatalf("list markers should not render as dashes: %s", plain)
	}
	if !strings.Contains(rendered, "•") {
		t.Fatalf("list should render bullets: %q", rendered)
	}
	if !strings.Contains(rendered, chatStrongStyle.Render("bold")) {
		t.Fatalf("inline bold in list should be styled: %q", rendered)
	}
	if !strings.Contains(rendered, chatInlineCodeStyle.Render("code")) {
		t.Fatalf("inline code in list should be styled: %q", rendered)
	}
}

func TestRenderMarkdownOrderedList(t *testing.T) {
	markdown := strings.Join([]string{
		"1. first",
		"2. second",
		"10. tenth",
	}, "\n")
	rendered := renderMarkdownForView(markdown, 80)
	plain := stripANSI(rendered)
	for _, want := range []string{"1. first", "2. second", "10. tenth"} {
		if !strings.Contains(plain, want) {
			t.Fatalf("ordered item missing %q:\n%s", want, plain)
		}
	}
}

func TestRenderMarkdownListWrapsLongItems(t *testing.T) {
	item := "- " + strings.Repeat("word ", 30)
	rendered := renderMarkdownForView(item, 40)
	for _, line := range strings.Split(rendered, "\n") {
		if got := lipgloss.Width(line); got > 40 {
			t.Fatalf("list line width = %d, want <= 40: %q", got, stripANSI(line))
		}
	}
	if !strings.Contains(rendered, "•") {
		t.Fatalf("wrapped list should still show a bullet: %q", rendered)
	}
}

func TestRenderMarkdownNestedList(t *testing.T) {
	markdown := strings.Join([]string{
		"- top",
		"  - nested one",
		"  - nested two",
		"- top again",
	}, "\n")
	rendered := renderMarkdownForView(markdown, 80)
	plain := stripANSI(rendered)
	for _, want := range []string{"top", "nested one", "nested two", "top again"} {
		if !strings.Contains(plain, want) {
			t.Fatalf("nested list item missing %q:\n%s", want, plain)
		}
	}
	if !strings.Contains(rendered, "•") || !strings.Contains(rendered, "◦") {
		t.Fatalf("nested list should cycle bullet glyphs: %q", rendered)
	}
}

func TestRenderMarkdownListWithContinuation(t *testing.T) {
	markdown := strings.Join([]string{
		"- item with a",
		"  continuation line",
		"- next item",
	}, "\n")
	rendered := renderMarkdownForView(markdown, 80)
	plain := stripANSI(rendered)
	if !strings.Contains(plain, "item with a") || !strings.Contains(plain, "continuation line") || !strings.Contains(plain, "next item") {
		t.Fatalf("list continuation lost:\n%s", plain)
	}
}

func TestRenderMarkdownNotListForDecimal(t *testing.T) {
	rendered := renderMarkdownForView("The value is 1.5 GHz and 3.14.", 80)
	plain := stripANSI(rendered)
	if strings.Contains(plain, "•") {
		t.Fatalf("decimal should not render as a list: %q", plain)
	}
	if !strings.Contains(plain, "1.5 GHz") || !strings.Contains(plain, "3.14") {
		t.Fatalf("prose lost: %q", plain)
	}
}

func TestRenderMarkdownBlockquote(t *testing.T) {
	markdown := strings.Join([]string{
		"> a quoted line",
		"> with **emphasis**",
	}, "\n")
	rendered := renderMarkdownForView(markdown, 80)
	plain := stripANSI(rendered)
	if !strings.Contains(plain, "a quoted line") || !strings.Contains(plain, "with emphasis") {
		t.Fatalf("blockquote content lost:\n%s", plain)
	}
	if !strings.Contains(plain, ">") && !strings.Contains(plain, "▎") {
		t.Fatalf("blockquote should render a marker: %q", rendered)
	}
	if strings.Contains(plain, "> a quoted line") {
		t.Fatalf("raw quote marker should be stripped: %q", plain)
	}
	if !strings.Contains(rendered, chatStrongStyle.Render("emphasis")) {
		t.Fatalf("blockquote inline style lost: %q", rendered)
	}
}

func TestRenderMarkdownNestedBlockquote(t *testing.T) {
	markdown := strings.Join([]string{
		"> outer",
		"> > inner",
	}, "\n")
	plain := stripANSI(renderMarkdownForView(markdown, 80))
	if !strings.Contains(plain, "outer") || !strings.Contains(plain, "inner") {
		t.Fatalf("nested blockquote content lost:\n%s", plain)
	}
}

func TestRenderMarkdownBlockquoteWithList(t *testing.T) {
	markdown := strings.Join([]string{
		"> - one",
		"> - two",
	}, "\n")
	plain := stripANSI(renderMarkdownForView(markdown, 80))
	if !strings.Contains(plain, "one") || !strings.Contains(plain, "two") {
		t.Fatalf("list inside blockquote lost:\n%s", plain)
	}
}

func TestRenderMarkdownThematicBreak(t *testing.T) {
	for _, hr := range []string{"---", "***", "___", "- - -", "* * *"} {
		markdown := "above\n" + hr + "\nbelow"
		plain := stripANSI(renderMarkdownForView(markdown, 40))
		if !strings.Contains(plain, "above") || !strings.Contains(plain, "below") {
			t.Fatalf("thematic break %q lost surrounding text:\n%s", hr, plain)
		}
		if strings.Contains(plain, hr) {
			t.Fatalf("thematic break %q should not render literally:\n%s", hr, plain)
		}
	}
	rendered := renderMarkdownForView("---", 40)
	if !strings.Contains(rendered, "─") {
		t.Fatalf("thematic break should render a rule: %q", rendered)
	}
	for _, line := range strings.Split(rendered, "\n") {
		if got := lipgloss.Width(line); got > 40 {
			t.Fatalf("hr line width = %d, want <= 40: %q", got, stripANSI(line))
		}
	}
}

func TestRenderMarkdownThematicBreakNotSetextOrDecimal(t *testing.T) {
	// A decimal-looking line must not become a list or break.
	plain := stripANSI(renderMarkdownForView("1.5", 80))
	if strings.Contains(plain, "•") {
		t.Fatalf("1.5 rendered as list: %q", plain)
	}
}

func TestRenderMarkdownLinkRoundTripVisibleWidth(t *testing.T) {
	rendered := renderMarkdownForView("Click [here](https://example.com/a/very/long/path/that/should/not/show) please.", 120)
	for _, line := range strings.Split(rendered, "\n") {
		if got := lipgloss.Width(line); got > 120 {
			t.Fatalf("link line width = %d, want <= 120: %q", got, stripANSI(line))
		}
	}
}
