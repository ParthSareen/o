package agent

import "testing"

func TestRequiredStringArg(t *testing.T) {
	tests := []struct {
		name    string
		args    map[string]any
		want    string
		wantErr string
	}{
		{name: "present", args: map[string]any{"key": "value"}, want: "value"},
		{name: "untrimmed", args: map[string]any{"key": "  value  "}, want: "  value  "},
		{name: "missing", args: map[string]any{}, wantErr: "key parameter is required"},
		{name: "wrong type", args: map[string]any{"key": 3}, wantErr: "key parameter is required"},
		{name: "empty", args: map[string]any{"key": ""}, wantErr: "key parameter is required"},
		{name: "whitespace", args: map[string]any{"key": " \t\n "}, wantErr: "key parameter is required"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := RequiredStringArg(tt.args, "key")
			if tt.wantErr != "" {
				if err == nil || err.Error() != tt.wantErr {
					t.Fatalf("RequiredStringArg() error = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("RequiredStringArg() unexpected error: %v", err)
			}
			if got != tt.want {
				t.Fatalf("RequiredStringArg() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestOptionalIntArg(t *testing.T) {
	tests := []struct {
		name    string
		args    map[string]any
		want    int
		wantOK  bool
		wantErr string
	}{
		{name: "missing", args: map[string]any{}, wantOK: false},
		{name: "int", args: map[string]any{"key": 5}, want: 5, wantOK: true},
		{name: "int64", args: map[string]any{"key": int64(7)}, want: 7, wantOK: true},
		{name: "whole float", args: map[string]any{"key": float64(42)}, want: 42, wantOK: true},
		{name: "fractional float", args: map[string]any{"key": 1.5}, wantOK: true, wantErr: "key must be a whole number"},
		{name: "numeric string", args: map[string]any{"key": " 12 "}, want: 12, wantOK: true},
		{name: "empty string", args: map[string]any{"key": "  "}, wantOK: false},
		{name: "non-numeric string", args: map[string]any{"key": "twelve"}, wantOK: true, wantErr: "key must be a whole number"},
		{name: "wrong type", args: map[string]any{"key": true}, wantOK: true, wantErr: "key must be a whole number"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok, err := OptionalIntArg(tt.args, "key")
			if ok != tt.wantOK {
				t.Fatalf("OptionalIntArg() ok = %v, want %v", ok, tt.wantOK)
			}
			if tt.wantErr != "" {
				if err == nil || err.Error() != tt.wantErr {
					t.Fatalf("OptionalIntArg() error = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("OptionalIntArg() unexpected error: %v", err)
			}
			if got != tt.want {
				t.Fatalf("OptionalIntArg() = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestOptionalBoolArg(t *testing.T) {
	tests := []struct {
		name   string
		args   map[string]any
		want   bool
		wantOK bool
	}{
		{name: "missing", args: map[string]any{}, wantOK: false},
		{name: "true", args: map[string]any{"key": true}, want: true, wantOK: true},
		{name: "false", args: map[string]any{"key": false}, want: false, wantOK: true},
		{name: "string true", args: map[string]any{"key": "true"}, wantOK: false},
		{name: "wrong type", args: map[string]any{"key": 1}, wantOK: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := OptionalBoolArg(tt.args, "key")
			if ok != tt.wantOK || got != tt.want {
				t.Fatalf("OptionalBoolArg() = (%v, %v), want (%v, %v)", got, ok, tt.want, tt.wantOK)
			}
		})
	}
}
