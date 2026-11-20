package chat

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"unicode"

	coreagent "github.com/ParthSareen/o/agent"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

type chatSlashCommand struct {
	name        string
	usage       string
	description string
	aliases     []string
	hidden      bool
}

type chatCompletion struct {
	value       string
	label       string
	description string
	directory   bool
}

var chatSlashCommands = []chatSlashCommand{
	{name: "/model", description: "switch models"},
	{name: "/new", description: "start a new chat"},
	{name: "/think", description: "set thinking mode"},
	{name: "/tools", description: "toggle tools on or off"},
	{name: "/system", usage: "/system [on|off]", description: "show or set the built-in system prompt"},
	{name: "/skills", usage: "/skills [import codex|claude|pi]", description: "list or import skills"},
	{name: "/side", usage: "/side [close]", description: "open a side chat forked from this conversation"},
	{name: "/compact", description: "summarize older context"},
	{name: "/copy", description: "copy last response to the clipboard"},
	{name: "/help", description: "show commands", aliases: []string{"/?"}},
	{name: "/bye", description: "exit", aliases: []string{"/exit"}},
	{name: "/prompt", description: "show full prompt, tools, and messages"},
	{name: "/save", usage: "/save <filename>", description: "save request JSON; saved as <filename>.json"},
	{name: "/sessions", description: "list and resume past sessions"},
	{name: "/resume", usage: "/resume [<id|name>]", description: "resume the most recent or a matching session"},
	{name: "/name", usage: "/name [set <text>]", description: "show or set the session name"},
	{name: "/reviewer", usage: "/reviewer [model <name>|selected]", description: "show or set the auto review model"},
}

var skillsImportCompletions = []chatCompletion{
	{value: "/skills import codex", label: "/skills import codex", description: "import from ~/.codex/skills"},
	{value: "/skills import claude", label: "/skills import claude", description: "import from ~/.claude/skills"},
	{value: "/skills import pi", label: "/skills import pi", description: "import from ~/.pi/agent/skills"},
}

// BuiltinSlashCommandNames returns the names reserved by built-in slash
// commands, including aliases.
func BuiltinSlashCommandNames() []string {
	names := make(map[string]struct{})
	for _, command := range chatSlashCommands {
		names[strings.TrimPrefix(command.name, "/")] = struct{}{}
		for _, alias := range command.aliases {
			names[strings.TrimPrefix(alias, "/")] = struct{}{}
		}
	}
	reserved := make([]string, 0, len(names))
	for name := range names {
		reserved = append(reserved, name)
	}
	sort.Strings(reserved)
	return reserved
}

func (m *chatModel) applySlashCompletion() bool {
	rawInput := string(m.input)
	input := strings.TrimSpace(rawInput)
	if !strings.HasPrefix(input, "/") {
		return m.applyTokenSlashCompletion()
	}
	if _, _, known := slashCommandInvocation(input); known && !hasSystemCommandArgument(rawInput) {
		return false
	}
	completions := m.slashCompletions()
	if len(completions) == 0 || !completionIsSelectable(completions) {
		return false
	}
	selected := completions[clamp(m.complete, 0, len(completions)-1)]
	if strings.EqualFold(selected.value, input) {
		return false
	}
	// Reset prompt-history state: Up/Down is shared between history recall and
	// slash completion, and a recalled prompt may start with "/" and trigger
	// completion. Keep the two in sync when we accept a completion.
	m.resetPromptHistoryCursor()
	m.input = []rune(selected.value)
	m.inputCursor = len(m.input)
	m.inputCursorSet = true
	m.complete = 0
	return true
}

func (m *chatModel) handleSkillsCommand(args string) (tea.Model, tea.Cmd) {
	if fields := strings.Fields(args); len(fields) == 2 && fields[0] == "import" {
		return m.handleSkillsImport(fields[1])
	} else if len(fields) != 0 {
		m.entries = append(m.entries, newChatEntry(chatEntry{role: "error", content: "usage: /skills [import codex|claude|pi]"}))
		return *m, nil
	}
	skills := m.opts.Skills.List()
	if len(skills) == 0 {
		m.entries = append(m.entries, newSlashEntry("No skills found. Add directories containing SKILL.md under "+skillsDirForDisplay(m.opts.Skills)+"."))
		return *m, nil
	}
	lines := []string{"Available skills:"}
	for _, skill := range skills {
		description := skill.Description
		if description == "" {
			description = "No description provided."
		}
		lines = append(lines, fmt.Sprintf("- `%s`: %s", skill.Name, description))
	}
	lines = append(lines, "\nType `/<name>` to load a skill into the conversation.")
	m.entries = append(m.entries, newSlashEntry(strings.Join(lines, "\n")))
	return *m, nil
}

func (m *chatModel) handleSkillsImport(source string) (tea.Model, tea.Cmd) {
	importSkills := m.opts.ImportSkills
	if importSkills == nil {
		importSkills = coreagent.ImportSkills
	}
	result, err := importSkills(source)
	if err != nil {
		m.status = "error"
		m.entries = append(m.entries, newChatEntry(chatEntry{role: "error", content: fmt.Sprintf("Could not import %s skills: %v", source, err)}))
		return *m, nil
	}

	if len(result.Imported) != 0 || len(result.Existing) != 0 {
		reload := m.opts.ReloadSkills
		if reload == nil {
			reload = func() (*coreagent.SkillCatalog, error) {
				return coreagent.LoadDefaultSkills(m.currentWorkingDir())
			}
		}
		catalog, err := reload()
		if err != nil {
			m.status = "error"
			m.entries = append(m.entries, newChatEntry(chatEntry{role: "error", content: fmt.Sprintf("%s\n\nCould not reload skills: %v", skillsImportSummary(result), err)}))
			return *m, nil
		}
		m.opts.Skills = catalog
		if m.opts.ToolRegistryForModel != nil && m.opts.Model != "" {
			m.opts.Tools = m.opts.ToolRegistryForModel(m.ctx, m.opts.Model)
		}
		if m.opts.SystemPromptForModel != nil {
			m.opts.SystemPrompt = m.opts.SystemPromptForModel(m.ctx, m.opts.Model, m.opts.Tools, m.opts.ToolsDisabled)
		}
		m.status = "skills reloaded"
	}
	m.entries = append(m.entries, newSlashEntry(skillsImportSummary(result)))
	return *m, nil
}

func skillsImportSummary(result coreagent.SkillImportResult) string {
	if len(result.Imported) == 0 && len(result.Existing) == 0 && len(result.Failures) == 0 {
		return fmt.Sprintf("No %s skills found at %s.", result.Source, result.SourceDir)
	}
	var lines []string
	if len(result.Imported) != 0 {
		lines = append(lines, fmt.Sprintf("Imported %d skill%s from %s.", len(result.Imported), pluralSuffix(len(result.Imported)), result.SourceDir))
	}
	if len(result.Existing) != 0 {
		lines = append(lines, "Already present (left unchanged): "+strings.Join(result.Existing, ", ")+".")
	}
	for _, failure := range result.Failures {
		lines = append(lines, fmt.Sprintf("Skipped %s: %v.", failure.Name, failure.Err))
	}
	return strings.Join(lines, "\n")
}

func skillsDirForDisplay(catalog *coreagent.SkillCatalog) string {
	if catalog != nil && catalog.Dir() != "" {
		return catalog.Dir()
	}
	dir, err := coreagent.SkillsDir()
	if err != nil {
		return "the Ollama skills directory"
	}
	return dir
}

// skillSlashInvocation parses "/<skill-name>" or "/<skill-name> <prompt>".
// It returns the skill name, any trailing prompt, and ok when the first token is
// a catalog skill. Built-in slash commands take precedence over same-named
// skills, so they are never claimed here.
func (m *chatModel) skillSlashInvocation(input string) (name, prompt string, ok bool) {
	input = strings.TrimSpace(input)
	fields := strings.Fields(input)
	for i, field := range fields {
		if !strings.HasPrefix(field, "/") {
			continue
		}
		name = strings.TrimPrefix(field, "/")
		if name == "" {
			continue
		}
		if _, _, known := slashCommandInvocation(field); known {
			continue // built-in command wins
		}
		if m.opts.Skills == nil {
			return "", "", false
		}
		if _, err := m.opts.Skills.Load(name); err != nil {
			continue
		}
		// invocation prompt = the rest of the line with the /token removed
		rest := slices.Delete(slices.Clone(fields), i, i+1)
		return name, strings.TrimSpace(strings.Join(rest, " ")), true
	}
	return "", "", false
}

func (m *chatModel) handleToolsCommand(args string) (tea.Model, tea.Cmd) {
	if strings.TrimSpace(args) != "" {
		m.status = "error"
		m.entries = append(m.entries, newChatEntry(chatEntry{role: "error", content: "usage: /tools"}))
		return *m, nil
	}
	if m.opts.ToolsDisabled {
		m.opts.ToolsDisabled = false
		if m.opts.ToolRegistryForModel != nil {
			m.opts.Tools = m.opts.ToolRegistryForModel(m.ctx, m.opts.Model)
		}
		m.status = "tools on"
	} else {
		m.opts.ToolsDisabled = true
		m.status = "tools off"
	}
	if m.opts.SystemPromptForModel != nil {
		m.opts.SystemPrompt = m.opts.SystemPromptForModel(m.ctx, m.opts.Model, m.opts.Tools, m.opts.ToolsDisabled)
	}
	return *m, nil
}

// handleNameCommand implements /name: with no args it shows the current
// session name; "/name set <text>" sets (or clears with empty text) it.
func (m *chatModel) handleNameCommand(args string) (tea.Model, tea.Cmd) {
	args = strings.TrimSpace(args)
	if args == "" {
		label := m.chatName
		if label == "" {
			label = "(unnamed)"
		}
		m.entries = append(m.entries, newSlashEntry("Session name: "+label+"\n\nUsage: /name set <text>"))
		m.status = "ready"
		return *m, nil
	}
	// Accept "/name set <text>" or, for brevity, "/name <text>".
	setPrefix := "set "
	if strings.HasPrefix(args, setPrefix) {
		args = strings.TrimSpace(strings.TrimPrefix(args, setPrefix))
	} else if strings.EqualFold(args, "set") {
		args = ""
	}
	name := strings.TrimSpace(args)
	if m.store == nil || m.chatID == "" {
		m.chatName = name
		m.opts.Name = name
		m.status = "name set (unsaved)"
		m.entries = append(m.entries, newSlashEntry("Session name set to: "+displayName(name)))
		return *m, nil
	}
	if err := m.store.SetName(m.chatID, name); err != nil {
		m.status = "error"
		m.entries = append(m.entries, newChatEntry(chatEntry{role: "error", content: "Could not set session name: " + err.Error()}))
		return *m, nil
	}
	m.chatName = name
	m.opts.Name = name
	m.status = "name set"
	m.entries = append(m.entries, newSlashEntry("Session name set to: "+displayName(name)))
	return *m, nil
}

func displayName(name string) string {
	if strings.TrimSpace(name) == "" {
		return "(none)"
	}
	return name
}

func (m *chatModel) handleSystemCommand(args string) (tea.Model, tea.Cmd) {
	switch strings.ToLower(strings.TrimSpace(args)) {
	case "":
		m.entries = append(m.entries, newSlashEntry(m.systemCommandOutput()))
	case "on":
		m.systemPromptDisabled = false
		m.status = "system prompt on"
		m.entries = append(m.entries, newSlashEntry(m.systemCommandOutput()))
	case "off":
		m.systemPromptDisabled = true
		m.status = "system prompt off"
		m.entries = append(m.entries, newSlashEntry(m.systemCommandOutput()))
	default:
		m.status = "error"
		m.entries = append(m.entries, newChatEntry(chatEntry{role: "error", content: "usage: /system [on|off]"}))
	}
	return *m, nil
}

func (m chatModel) systemPromptState() string {
	if m.systemPromptDisabled {
		return "off"
	}
	return "on"
}

func (m chatModel) systemCommandOutput() string {
	prompt := strings.TrimSpace(m.opts.SystemPrompt)
	if prompt == "" {
		prompt = "(empty)"
	}
	return "Built-in system prompt is " + m.systemPromptState() + ".\n\n" + prompt + "\n\nWarning: Changing the system prompt during a session breaks the prompt cache."
}

func (m chatModel) slashCommandLines(width int) []string {
	return m.renderCompletions(m.slashCompletions(), width)
}

func (m chatModel) completionLines(width int) []string {
	return m.renderCompletions(m.completions(), width)
}

func (m chatModel) renderCompletions(completions []chatCompletion, width int) []string {
	if len(completions) == 0 {
		return nil
	}
	selected := clamp(m.complete, 0, len(completions)-1)
	start, end := completionWindow(len(completions), selected, m.completionVisibleLimit(len(completions)))
	completions = completions[start:end]

	nameWidth := 0
	for _, completion := range completions {
		nameWidth = max(nameWidth, lipgloss.Width(completion.label))
	}

	lines := make([]string, 0, len(completions))
	for i, completion := range completions {
		marker := "  "
		if start+i == selected {
			marker = "› "
		}
		name := chatCommandNameStyle.Render(completion.label)
		padding := strings.Repeat(" ", max(1, nameWidth-lipgloss.Width(completion.label)+2))
		line := marker + name + padding + chatMetaStyle.Render(completion.description)
		lines = append(lines, truncateRenderedLine(line, width))
	}
	return lines
}

func (m chatModel) completionVisibleLimit(total int) int {
	if strings.HasPrefix(strings.TrimSpace(string(m.input)), "/") {
		return min(maxSlashCompletions, total)
	}
	return total
}

func completionWindow(total, selected, limit int) (int, int) {
	if total <= 0 || limit <= 0 || limit >= total {
		return 0, total
	}
	selected = clamp(selected, 0, total-1)
	start := selected - limit + 1
	if start < 0 {
		start = 0
	}
	end := start + limit
	if end > total {
		end = total
		start = max(0, end-limit)
	}
	return start, end
}

func (m chatModel) completions() []chatCompletion {
	if completions := m.slashCompletions(); len(completions) > 0 {
		return completions
	}
	// mid-line: complete the token at the cursor when it is a "/..." token
	// with matches (bare unmatched tokens just stay text — no dropdown)
	if start, token, ok := activeSlashToken(m.input, m.normalizedInputCursor()); ok && start > 0 {
		if matches := m.slashNameCompletions(token); len(matches) > 0 {
			return matches
		}
	}
	return m.mentionCompletions()
}

func (m chatModel) slashCompletions() []chatCompletion {
	rawInput := string(m.input)
	input := strings.TrimLeftFunc(rawInput, unicode.IsSpace)
	if !strings.HasPrefix(input, "/") {
		return nil
	}
	if m.pane {
		return m.paneSlashCompletions(input)
	}
	if argument, ok := systemCommandArgument(rawInput); ok {
		return systemCommandCompletions(argument)
	}
	if m.skillSlashPromptStarted(rawInput) {
		return nil
	}
	if completions := matchingSkillsImportCompletions(input); completions != nil {
		return completions
	}
	if completions := reviewerCommandCompletions(input); completions != nil {
		return completions
	}

	completions := m.slashNameCompletions(input)
	if len(completions) == 0 {
		return []chatCompletion{{label: "No matching commands"}}
	}
	return completions
}

func matchingSkillsImportCompletions(input string) []chatCompletion {
	const importCommand = "/skills import"
	lower := strings.ToLower(input)
	if lower == "/skills" {
		return nil // Preserve Enter on /skills as the listing command.
	}
	if !strings.HasPrefix(lower, "/skills ") {
		return nil
	}
	if strings.HasPrefix(importCommand, lower) {
		return []chatCompletion{{
			value:       importCommand,
			label:       importCommand,
			description: "import skills from Codex, Claude, or Pi",
		}}
	}
	if !strings.HasPrefix(lower, importCommand) {
		return nil
	}
	prefix := strings.TrimSpace(strings.TrimPrefix(lower, importCommand))
	completions := make([]chatCompletion, 0, len(skillsImportCompletions))
	for _, completion := range skillsImportCompletions {
		if strings.HasPrefix(strings.TrimPrefix(completion.value, importCommand+" "), prefix) {
			completions = append(completions, completion)
		}
	}
	if len(completions) == 0 {
		return []chatCompletion{{label: "No matching skill sources"}}
	}
	return completions
}

// reviewerCommandCompletions fills the argument position of /reviewer. Returning
// nil means the input is not a /reviewer argument (fall through to the next
// completion source); an empty slice hides the menu so a model name can be
// typed freely after "model".
func reviewerCommandCompletions(input string) []chatCompletion {
	lower := strings.ToLower(input)
	if lower == "/reviewer" {
		return nil // Enter on /reviewer shows the current setting.
	}
	if !strings.HasPrefix(lower, "/reviewer ") {
		return nil
	}
	// After "model" the rest is a free-form model name; nothing matches it.
	if strings.HasPrefix(lower, "/reviewer model") {
		return []chatCompletion{}
	}
	prefix := strings.TrimPrefix(lower, "/reviewer ")
	options := []chatCompletion{
		{value: "/reviewer model", label: "/reviewer model <name>", description: "set the model grading tool calls in auto mode"},
		{value: "/reviewer selected", label: "/reviewer selected", description: "grade with the session model again"},
	}
	completions := make([]chatCompletion, 0, len(options))
	for _, option := range options {
		if strings.HasPrefix(strings.TrimPrefix(option.value, "/reviewer "), prefix) {
			completions = append(completions, option)
		}
	}
	if len(completions) == 0 {
		return []chatCompletion{}
	}
	return completions
}

func hasSystemCommandArgument(input string) bool {
	_, ok := systemCommandArgument(input)
	return ok
}

func systemCommandArgument(input string) (string, bool) {
	input = strings.TrimLeftFunc(input, unicode.IsSpace)
	end := strings.IndexFunc(input, unicode.IsSpace)
	if end < 0 {
		return "", false
	}
	command, _, known := slashCommandInvocation(input[:end])
	if !known || command != "/system" {
		return "", false
	}
	return strings.TrimSpace(input[end:]), true
}

func systemCommandCompletions(argument string) []chatCompletion {
	argument = strings.ToLower(argument)
	options := []chatCompletion{
		{value: "/system on", label: "on", description: "enable the built-in system prompt"},
		{value: "/system off", label: "off", description: "disable the built-in system prompt"},
	}
	completions := make([]chatCompletion, 0, len(options))
	for _, option := range options {
		if strings.HasPrefix(option.label, argument) {
			completions = append(completions, option)
		}
	}
	if len(completions) == 0 {
		return []chatCompletion{{label: "No matching options"}}
	}
	return completions
}

func (m chatModel) skillSlashPromptStarted(input string) bool {
	input = strings.TrimLeftFunc(input, unicode.IsSpace)
	end := strings.IndexFunc(input, unicode.IsSpace)
	if end < 0 {
		return false
	}
	_, _, ok := m.skillSlashInvocation(input[:end])
	return ok
}

func matchingSlashCommands(input string) []chatSlashCommand {
	prefix := strings.ToLower(strings.TrimSpace(input))
	if prefix == "" {
		return nil
	}

	var commands []chatSlashCommand
	for _, command := range chatSlashCommands {
		if command.hidden {
			continue
		}
		if command.matchesPrefix(prefix) {
			commands = append(commands, command)
		}
	}
	return commands
}

func (c chatSlashCommand) matchesPrefix(prefix string) bool {
	if strings.HasPrefix(c.name, prefix) {
		return true
	}
	for _, alias := range c.aliases {
		if strings.HasPrefix(alias, prefix) {
			return true
		}
	}
	return false
}

func slashCommandInvocation(input string) (string, string, bool) {
	input = strings.TrimSpace(input)
	if !strings.HasPrefix(input, "/") {
		return "", "", false
	}
	token, args, _ := strings.Cut(input, " ")
	token = strings.ToLower(token)
	for _, command := range chatSlashCommands {
		if command.name == token || slices.Contains(command.aliases, token) {
			return command.name, strings.TrimSpace(args), true
		}
	}
	return "", "", false
}

func (m chatModel) mentionCompletions() []chatCompletion {
	_, query, ok := activeMentionToken(m.input, m.normalizedInputCursor())
	if !ok {
		return nil
	}

	workingDir := m.currentWorkingDir()
	if strings.TrimSpace(workingDir) == "" {
		var err error
		workingDir, err = os.Getwd()
		if err != nil {
			return []chatCompletion{{label: "No working directory"}}
		}
	}

	dirPart, prefix := splitMentionQuery(query)
	dir, err := resolveCompletionDir(workingDir, dirPart)
	if err != nil {
		return []chatCompletion{{label: "No matching files"}}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return []chatCompletion{{label: "No matching files"}}
	}
	sort.SliceStable(entries, func(i, j int) bool {
		if entries[i].IsDir() != entries[j].IsDir() {
			return entries[i].IsDir()
		}
		return strings.ToLower(entries[i].Name()) < strings.ToLower(entries[j].Name())
	})

	includeHidden := strings.HasPrefix(prefix, ".")
	completions := make([]chatCompletion, 0, 8)
	for _, entry := range entries {
		name := entry.Name()
		if !includeHidden && strings.HasPrefix(name, ".") {
			continue
		}
		if !strings.HasPrefix(strings.ToLower(name), strings.ToLower(prefix)) {
			continue
		}
		value := filepath.ToSlash(filepath.Join(dirPart, name))
		label := "@" + value
		description := "file"
		if entry.IsDir() {
			value += "/"
			label += "/"
			description = "directory"
		}
		completions = append(completions, chatCompletion{
			value:       value,
			label:       label,
			description: description,
			directory:   entry.IsDir(),
		})
		if len(completions) >= 8 {
			break
		}
	}
	if len(completions) == 0 {
		return []chatCompletion{{label: "No matching files"}}
	}
	return completions
}

func activeMentionToken(input []rune, cursor int) (int, string, bool) {
	cursor = clamp(cursor, 0, len(input))
	start := cursor
	for start > 0 && !unicode.IsSpace(input[start-1]) {
		start--
	}
	token := string(input[start:cursor])
	if !strings.HasPrefix(token, "@") {
		return 0, "", false
	}
	return start, token[1:], true
}

func splitMentionQuery(query string) (string, string) {
	query = filepath.ToSlash(query)
	index := strings.LastIndex(query, "/")
	if index < 0 {
		return ".", query
	}
	return query[:index+1], query[index+1:]
}

func resolveCompletionDir(workingDir, dir string) (string, error) {
	if filepath.IsAbs(dir) {
		return "", fmt.Errorf("absolute paths are not allowed")
	}
	base := workingDir
	if base == "" {
		base = "."
	}
	base, err := filepath.Abs(base)
	if err != nil {
		return "", err
	}
	resolved := filepath.Clean(filepath.Join(base, dir))
	rel, err := filepath.Rel(base, resolved)
	if err != nil {
		return "", err
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path escapes working directory")
	}
	return resolved, nil
}

func (m *chatModel) moveCompletion(delta int) bool {
	completions := m.completions()
	if len(completions) == 0 || !completionIsSelectable(completions) {
		return false
	}
	m.complete = (m.complete + delta) % len(completions)
	if m.complete < 0 {
		m.complete += len(completions)
	}
	return true
}

func (m *chatModel) applyCompletion() bool {
	completions := m.completions()
	if len(completions) == 0 || !completionIsSelectable(completions) {
		return false
	}
	m.resetPromptHistoryCursor()
	selected := completions[clamp(m.complete, 0, len(completions)-1)]
	cursor := m.normalizedInputCursor()
	input := string(m.input)
	if strings.HasPrefix(strings.TrimSpace(input), "/") {
		m.input = []rune(selected.value)
		m.inputCursor = len(m.input)
		m.inputCursorSet = true
		m.complete = 0
		return true
	}

	start, _, ok := activeMentionToken(m.input, cursor)
	if !ok {
		return false
	}
	completed := []rune("@" + selected.value)
	if !selected.directory && (cursor == len(m.input) || !unicode.IsSpace(m.input[cursor])) {
		completed = append(completed, ' ')
	}
	next := make([]rune, 0, len(m.input)-cursor+start+len(completed))
	next = append(next, m.input[:start]...)
	next = append(next, completed...)
	next = append(next, m.input[cursor:]...)
	m.input = next
	m.inputCursor = start + len(completed)
	if !selected.directory && m.inputCursor < len(m.input) && unicode.IsSpace(m.input[m.inputCursor]) {
		m.inputCursor++
	}
	m.inputCursorSet = true
	m.complete = 0
	return true
}

func (m *chatModel) applyMentionCompletion() bool {
	if strings.HasPrefix(strings.TrimSpace(string(m.input)), "/") {
		return false
	}
	return m.applyCompletion()
}

func completionIsSelectable(completions []chatCompletion) bool {
	return len(completions) > 0 && completions[0].value != ""
}

func (m chatModel) helpSummary() string {
	lines := []string{
		"**Commands**",
		"",
	}
	for _, command := range chatSlashCommands {
		if command.hidden || strings.TrimSpace(command.description) == "" {
			continue
		}
		usage := command.name
		if command.usage != "" {
			usage = command.usage
		}
		lines = append(lines, fmt.Sprintf("- `%s`: %s", usage, command.description))
	}
	lines = append(lines,
		"",
		"**Shortcuts**",
		"",
		"- `shift+enter`: insert a newline",
		"- `shift+tab`: toggle permission mode",
		"- `↑/↓`: previous or next prompt",
		"- `ctrl+a/e`: move to line start or end",
		"- `opt+]` (or `ctrl+]`): open the side chat or switch focus",
	)
	return strings.Join(lines, "\n")
}

// activeSlashToken returns the whitespace-delimited token ending at the cursor
// when it starts with "/": its start index, the token text up to the cursor,
// and ok. Cursor-positioned anywhere-in-input slash detection is what powers
// mid-line skill completion.
func activeSlashToken(input []rune, cursor int) (int, string, bool) {
	cursor = clamp(cursor, 0, len(input))
	start := cursor
	for start > 0 && !unicode.IsSpace(input[start-1]) {
		start--
	}
	token := string(input[start:cursor])
	if !strings.HasPrefix(token, "/") {
		return 0, "", false
	}
	return start, token, true
}

// slashNameCompletions matches built-in commands and catalog skills against a
// "/prefix" token, without placeholders. Shared by the line-start completion
// path (which adds its no-match placeholder) and the mid-line token path
// (which shows nothing when there is no match).
func (m chatModel) slashNameCompletions(prefix string) []chatCompletion {
	commands := matchingSlashCommands(prefix)
	completions := make([]chatCompletion, 0, len(commands))
	for _, command := range commands {
		completions = append(completions, chatCompletion{
			value:       command.name,
			label:       command.name,
			description: command.description,
		})
	}
	if strings.EqualFold(prefix, "/skills") {
		completions = append(completions, chatCompletion{
			value:       "/skills import",
			label:       "/skills import",
			description: "import skills from Codex, Claude, or Pi",
		})
	}
	if m.opts.Skills != nil {
		lower := strings.ToLower(prefix)
		for _, skill := range m.opts.Skills.List() {
			name := "/" + skill.Name
			if !strings.HasPrefix(name, lower) {
				continue
			}
			if _, _, known := slashCommandInvocation(name); known {
				continue // built-in command wins; don't shadow it
			}
			description := skill.Description
			if description == "" {
				description = "No description provided."
			}
			completions = append(completions, chatCompletion{
				value:       name,
				label:       name,
				description: description,
			})
		}
	}
	return completions
}

// applyTokenSlashCompletion replaces the slash token at the cursor with the
// selected completion, preserving the rest of the line.
func (m *chatModel) applyTokenSlashCompletion() bool {
	start, token, ok := activeSlashToken(m.input, m.normalizedInputCursor())
	if !ok {
		return false
	}
	matches := m.slashNameCompletions(token)
	if len(matches) == 0 || !completionIsSelectable(matches) {
		return false
	}
	selected := matches[clamp(m.complete, 0, len(matches)-1)]
	if strings.EqualFold(selected.value, token) {
		return false
	}
	cursor := m.normalizedInputCursor()
	next := make([]rune, 0, len(m.input)+len(selected.value))
	next = append(next, m.input[:start]...)
	next = append(next, []rune(selected.value)...)
	insertEnd := len(next)
	next = append(next, m.input[cursor:]...)
	m.input = next
	m.inputCursor = insertEnd
	m.inputCursorSet = true
	m.resetPromptHistoryCursor()
	m.complete = 0
	return true
}
