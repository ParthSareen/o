package main

// Local, simplified copies of helpers that live in ollama's cmd/cmd.go.
// showOrPullModel drops the cloud-suggestion flow for a plain pull;
// inferThinkingOption takes capabilities directly (the caller already has
// the Show response) instead of the runOptions kitchen-sink struct.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"

	"github.com/ParthSareen/o/api"
	"github.com/ParthSareen/o/internal/modelref"
	"github.com/ParthSareen/o/logutil"
	"github.com/ParthSareen/o/types/model"
	"github.com/spf13/cobra"
)

// ensureCloudStub best-effort pulls cloud stub files for explicit cloud
// source models. TEMP, matching ollama's cmd/cmd.go.
func ensureCloudStub(ctx context.Context, client *api.Client, modelName string) {
	if !modelref.HasExplicitCloudSource(modelName) {
		return
	}

	normalizedName, _, err := modelref.NormalizePullName(modelName)
	if err != nil {
		slog.Warn("failed to normalize pull name", "model", modelName, "error", err, "normalizedName", normalizedName)
		return
	}

	listResp, err := client.List(ctx)
	if err != nil {
		slog.Warn("failed to list models", "error", err)
		return
	}

	if hasListedModelName(listResp.Models, modelName) || hasListedModelName(listResp.Models, normalizedName) {
		return
	}

	logutil.Trace("pulling cloud stub", "model", modelName, "normalizedName", normalizedName)
	err = client.Pull(ctx, &api.PullRequest{
		Model: normalizedName,
	}, func(api.ProgressResponse) error {
		return nil
	})
	if err != nil {
		slog.Warn("failed to pull cloud stub", "model", modelName, "error", err)
	}
}

func hasListedModelName(models []api.ListModelResponse, name string) bool {
	for _, m := range models {
		if strings.EqualFold(m.Name, name) || strings.EqualFold(m.Model, name) {
			return true
		}
	}
	return false
}

// showOrPullModel returns model info for name, pulling the model if it isn't
// available locally. verb is the user-facing command used in hint text.
func showOrPullModel(cmd *cobra.Command, client *api.Client, name string, insecure bool, verb string) (*api.ShowResponse, string, error) {
	info, err := client.Show(cmd.Context(), &api.ShowRequest{Model: name})
	if err == nil {
		return info, name, nil
	}

	var se api.StatusError
	if !errors.As(err, &se) || se.StatusCode != http.StatusNotFound || modelref.HasExplicitCloudSource(name) {
		return nil, name, err
	}

	fmt.Fprintf(os.Stderr, "pulling %s...\n", name)
	pullReq := &api.PullRequest{Model: name, Insecure: insecure}
	lastStatus := ""
	if err := client.Pull(cmd.Context(), pullReq, func(resp api.ProgressResponse) error {
		if resp.Status != lastStatus {
			fmt.Fprintf(os.Stderr, "\r%s\033[K", resp.Status)
			lastStatus = resp.Status
		}
		return nil
	}); err != nil {
		fmt.Fprintln(os.Stderr)
		return nil, name, fmt.Errorf("failed to %s %s: %w", verb, name, err)
	}
	fmt.Fprintln(os.Stderr)

	info, err = client.Show(cmd.Context(), &api.ShowRequest{Model: name})
	return info, name, err
}

// inferThinkingOption enables thinking when the model supports it and the
// user didn't set an explicit value.
func inferThinkingOption(caps []model.Capability, think *api.ThinkValue, explicitlySetByUser bool) (*api.ThinkValue, error) {
	if explicitlySetByUser {
		return think, nil
	}
	for _, cap := range caps {
		if cap == model.CapabilityThinking {
			return &api.ThinkValue{Value: true}, nil
		}
	}
	return nil, nil
}
