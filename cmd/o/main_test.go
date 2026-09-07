
package main

import (
	"reflect"
	"testing"
)

func TestSplitModelPrompt(t *testing.T) {
	tests := []struct {
		args        []string
		model, want string
	}{
		{nil, "", ""},
		{[]string{"glm-5.3:cloud"}, "glm-5.3:cloud", ""},
		{[]string{"glm-5.3:cloud", "fix", "the", "tests"}, "glm-5.3:cloud", "fix the tests"},
		{[]string{"fix the tests"}, "", "fix the tests"},
		{[]string{"follow-up with spaces"}, "", "follow-up with spaces"},
	}
	for _, tt := range tests {
		model, prompt := splitModelPrompt(tt.args)
		if model != tt.model || prompt != tt.want {
			t.Errorf("splitModelPrompt(%q) = (%q, %q), want (%q, %q)", tt.args, model, prompt, tt.model, tt.want)
		}
	}
}

func TestSplitModelPromptAllArgsArePrompt(t *testing.T) {
	model, prompt := splitModelPrompt([]string{"two words", "more"})
	if model != "" {
		t.Fatalf("model = %q, want empty", model)
	}
	if !reflect.DeepEqual(prompt, "two words more") {
		t.Fatalf("prompt = %q", prompt)
	}
}
