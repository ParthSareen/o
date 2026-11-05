package chat

import (
	"context"
	"fmt"
	"slices"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	coreagent "github.com/ParthSareen/o/agent"
	apptui "github.com/ParthSareen/o/cmd/tui"
)

type chatModelPicker = apptui.SelectorModel

func (m *chatModel) openModelPicker(filter string) (tea.Model, tea.Cmd) {
	if m.opts.ModelOptions == nil {
		m.entries = append(m.entries, newChatEntry(chatEntry{role: "error", content: "Model picker is unavailable.", err: "Model picker is unavailable."}))
		m.status = "error"
		return *m, nil
	}

	ctx := m.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	models, err := m.opts.ModelOptions(ctx)
	if err != nil {
		m.entries = append(m.entries, newChatEntry(chatEntry{role: "error", content: fmt.Sprintf("Could not list models: %v", err), err: err.Error()}))
		m.status = "error"
		return *m, nil
	}
	models = normalizeModelOptions(models)
	if len(models) == 0 {
		m.entries = append(m.entries, newChatEntry(chatEntry{role: "system", content: "No models available."}))
		m.status = "ready"
		return *m, nil
	}

	items := modelSelectorItems(models, m.opts.Model)
	current := m.opts.Model
	if !m.openModelOnInit {
		items = compactModelSelectorItems(models, m.opts.Model)
		current = ""
	}
	picker := apptui.NewModelSelectorModel("Select model", items, current, filter)
	picker.SetHelpText("↑/↓ navigate • enter select • type search • esc cancel")
	m.modelPicker = &picker
	m.modelPickerModels = models
	m.status = "model"
	return *m, nil
}

func normalizeModelOptions(models []ModelOption) []ModelOption {
	seen := make(map[string]struct{}, len(models))
	out := make([]ModelOption, 0, len(models))
	for _, model := range models {
		model.Name = strings.TrimSpace(model.Name)
		model.Description = strings.TrimSpace(model.Description)
		if model.Name == "" {
			continue
		}
		key := strings.ToLower(model.Name)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, model)
	}
	slices.SortStableFunc(out, func(a, b ModelOption) int {
		if a.Recommended == b.Recommended {
			return 0
		}
		if a.Recommended {
			return -1
		}
		return 1
	})
	return out
}

func modelSelectorItems(models []ModelOption, current string) []apptui.SelectItem {
	return modelSelectorItemsWithCurrentPriority(models, current, true)
}

func compactModelSelectorItems(models []ModelOption, current string) []apptui.SelectItem {
	return modelSelectorItemsWithCurrentPriority(models, current, false)
}

func modelSelectorItemsWithCurrentPriority(models []ModelOption, current string, pinCurrent bool) []apptui.SelectItem {
	ordered := slices.Clone(models)
	slices.SortStableFunc(ordered, func(a, b ModelOption) int {
		if cmp := compareModelPickerGroup(modelPickerGroup(a, current, pinCurrent), modelPickerGroup(b, current, pinCurrent)); cmp != 0 {
			return cmp
		}
		return 0
	})

	items := make([]apptui.SelectItem, 0, len(ordered))
	for _, model := range ordered {
		items = append(items, apptui.SelectItem{
			Name:              model.Name,
			Description:       modelOptionMeta(model),
			Recommended:       model.Name == current || !model.Cloud || model.Recommended,
			AvailabilityBadge: model.AvailabilityBadge,
		})
	}
	return items
}

func modelPickerGroup(model ModelOption, current string, pinCurrent bool) int {
	if pinCurrent && model.Name == current {
		return 0
	}
	if model.Recommended {
		return 1
	}
	if model.Name == current {
		return 2
	}
	if !model.Cloud {
		return 3
	}
	return 4
}

func compareModelPickerGroup(a, b int) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	default:
		return 0
	}
}

func (m chatModel) updateModelPicker(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.modelPicker == nil {
		return m, nil
	}
	switch msg.Type {
	case tea.KeyCtrlC, tea.KeyEsc:
		m.modelPicker = nil
		m.modelPickerModels = nil
		m.openModelOnInit = false
		m.status = "ready"
		return m, nil
	case tea.KeyEnter:
		return m.selectModel()
	default:
		m.modelPicker.UpdateNavigation(msg)
	}
	return m, nil
}

func (m chatModel) selectModel() (tea.Model, tea.Cmd) {
	if m.modelPicker == nil {
		return m, nil
	}
	if m.pane {
		return m.selectPaneModel()
	}
	selectedItem, ok := m.modelPicker.SelectedItem()
	if !ok {
		return m, nil
	}
	selected, ok := m.modelOptionForSelection(selectedItem.Name)
	if !ok {
		return m, nil
	}

	// Cloud models need auth + plan check before switching. If we already
	// know the badge state from the model list, go directly to the right
	// prompt — no "checking" spinner.
	if selected.Cloud && m.opts.CheckCloudModel != nil {
		switch selected.AvailabilityBadge {
		case "Sign in required":
			return m.startCloudAuthSignIn(selected.Name, selected.RequiredPlan, selected.SignInURL)
		case "Upgrade required":
			return m.startCloudAuthUpgrade(selected.Name, selected.RequiredPlan)
		}
		// Badge is empty — auth is satisfied (confirmed via Whoami when the
		// list was built). Apply directly.
	}

	m.modelPicker = nil
	m.modelPickerModels = nil
	m.openModelOnInit = false
	cmd, err := m.applyModelSelectionWith(selected.Name, modelSelectionConfig{persist: true, notify: true, preload: true})
	if err != nil {
		m.entries = append(m.entries, newChatEntry(chatEntry{role: "error", content: fmt.Sprintf("Could not switch model: %v", err), err: err.Error()}))
		m.status = "error"
		return m, nil
	}
	m.status = "ready"
	return m, tea.Batch(cmd, cloudModelPreflightCmd(m.ctx, m.opts, selected.Name, selected.RequiredPlan))
}

func (m chatModel) modelOptionForSelection(name string) (ModelOption, bool) {
	for _, model := range m.modelPickerModels {
		if model.Name == name {
			return model, true
		}
	}
	return ModelOption{}, false
}

// modelSelectionConfig controls how a model switch applies to a chat or a
// side pane.
type modelSelectionConfig struct {
	// registry rebuilds tools for the model; nil uses opts.ToolRegistryForModel.
	registry func(context.Context, string) *coreagent.Registry
	// closePreviousTools closes the registry being replaced.
	closePreviousTools bool
	// disableToolsWhenMissing marks tools disabled when no registry is built.
	disableToolsWhenMissing bool
	// persist saves the selected model through OnModelSelected.
	persist bool
	// notify calls OnModelSelected; persist implies the save itself.
	notify bool
	// preload starts background model preload.
	preload bool
}

func (m *chatModel) applyModelSelectionWith(modelName string, cfg modelSelectionConfig) (tea.Cmd, error) {
	modelName = strings.TrimSpace(modelName)
	if modelName == "" {
		return nil, nil
	}
	m.opts.Model = modelName
	m.opts.ContextWindowTokens = 0

	registryForModel := cfg.registry
	if registryForModel == nil {
		registryForModel = m.opts.ToolRegistryForModel
	}
	var previous *coreagent.Registry
	if cfg.closePreviousTools {
		previous = m.opts.Tools
	}
	if registryForModel != nil {
		m.opts.Tools = registryForModel(m.ctx, modelName)
	}
	if cfg.closePreviousTools && previous != nil && previous != m.opts.Tools {
		_ = previous.Close()
	}
	if cfg.disableToolsWhenMissing && (m.opts.ToolsDisabled || m.opts.Tools == nil) {
		m.opts.ToolsDisabled = true
	}
	if m.opts.SystemPromptForModel != nil {
		m.opts.SystemPrompt = m.opts.SystemPromptForModel(m.ctx, modelName, m.opts.Tools, m.opts.ToolsDisabled)
	}
	if m.opts.MultiModalForModel != nil {
		ctx := m.ctx
		if ctx == nil {
			ctx = context.Background()
		}
		m.opts.MultiModal = m.opts.MultiModalForModel(ctx, modelName)
	}
	m.refreshContextWindowTokens(modelName)
	m.contextTokens = m.estimatePromptTokens(m.messages, "")
	m.contextEstimate = true
	var cmd tea.Cmd
	if cfg.preload {
		cmd = m.startModelPreload(modelName)
	}
	if cfg.persist && cfg.notify && m.opts.OnModelSelected != nil {
		ctx := m.ctx
		if ctx == nil {
			ctx = context.Background()
		}
		if err := m.opts.OnModelSelected(ctx, modelName); err != nil {
			return cmd, err
		}
	}
	return cmd, nil
}

func (m *chatModel) applyModelSelection(modelName string, persist bool) error {
	_, err := m.applyModelSelectionWith(modelName, modelSelectionConfig{persist: persist, notify: persist})
	return err
}

func (m *chatModel) startModelPreload(modelName string) tea.Cmd {
	modelName = strings.TrimSpace(modelName)
	if m == nil || modelName == "" || m.opts.PreloadModel == nil {
		return nil
	}
	m.preloadingModel = modelName
	m.spinner = 0
	return tea.Batch(preloadModelCmd(m.ctx, m.opts.PreloadModel, modelName, m.opts.Think), m.scheduleTick())
}

func (m chatModel) renderModelPicker(width int) string {
	return m.modelPicker.RenderContent()
}

func (m chatModel) renderInlineModelPicker(width int) []string {
	rendered := m.modelPicker.RenderCompactContent(maxInlineModelPickerItems)
	lines := strings.Split(strings.TrimRight(rendered, "\n"), "\n")
	for i := range lines {
		lines[i] = truncateRenderedLine(lines[i], width)
	}
	return lines
}

func modelOptionMeta(model ModelOption) string {
	return strings.TrimSpace(model.Description)
}
