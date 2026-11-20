package agent

import (
	"fmt"
	"strconv"
	"strings"
)

// RequiredStringArg returns the string argument at key. A missing, non-string,
// or whitespace-only value is an error. The value is not trimmed.
func RequiredStringArg(args map[string]any, key string) (string, error) {
	value, ok := args[key].(string)
	if !ok || strings.TrimSpace(value) == "" {
		return "", fmt.Errorf("%s parameter is required", key)
	}
	return value, nil
}

// OptionalIntArg returns the integer argument at key and whether it was
// supplied. JSON numbers arrive as float64 and are accepted when whole;
// numeric strings are parsed after trimming. A supplied value that cannot be
// read as an integer is an error.
func OptionalIntArg(args map[string]any, key string) (int, bool, error) {
	value, ok := args[key]
	if !ok {
		return 0, false, nil
	}
	switch v := value.(type) {
	case int:
		return v, true, nil
	case int64:
		return int(v), true, nil
	case float64:
		if v != float64(int(v)) {
			return 0, true, fmt.Errorf("%s must be a whole number", key)
		}
		return int(v), true, nil
	case string:
		v = strings.TrimSpace(v)
		if v == "" {
			return 0, false, nil
		}
		n, err := strconv.Atoi(v)
		if err != nil {
			return 0, true, fmt.Errorf("%s must be a whole number", key)
		}
		return n, true, nil
	default:
		return 0, true, fmt.Errorf("%s must be a whole number", key)
	}
}

// OptionalBoolArg returns the boolean argument at key and whether it was
// supplied as a bool; a missing or non-boolean value yields false.
func OptionalBoolArg(args map[string]any, key string) (bool, bool) {
	value, ok := args[key].(bool)
	return value, ok
}
