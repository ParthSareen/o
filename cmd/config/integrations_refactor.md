# Integrations Refactor Notes

## OLLAMA_HOST Handling

### Current State
Each integration file independently imports `envconfig` and calls `envconfig.Host().String()`:

| File | Line | Usage |
|------|------|-------|
| `cline.go` | 85 | `baseURL := envconfig.Host().String()` |
| `claude.go` | 63 | `"ANTHROPIC_BASE_URL="+envconfig.Host().String()` |
| `droid.go` | 143 | `BaseURL: envconfig.Host().String() + "/v1"` |
| `opencode.go` | 127 | `"baseURL": envconfig.Host().String() + "/v1"` |
| `openclaw.go` | 161 | `ollama["baseUrl"] = envconfig.Host().String() + "/v1"` |
| `pi.go` | 91 | `"baseUrl": envconfig.Host().String() + "/v1"` |

### Observation
`integrations.go` is the central registry and orchestration layer for all integrations. It would be cleaner to centralize OLLAMA_HOST handling there rather than having each integration fetch it independently.

### Potential Approaches

1. **Add helpers in `integrations.go`:**
   ```go
   func OllamaBaseURL() string {
       return envconfig.Host().String()
   }

   func OllamaAPIBaseURL() string {
       return envconfig.Host().String() + "/v1"
   }
   ```

2. **Pass host as parameter** to integration methods instead of having them fetch it

3. **Add to integration struct types** as a method they can call

### Benefits
- Single source of truth for host configuration
- Reduced duplication across 6 files
- Easier to modify URL logic in one place
- Better separation of concerns (integrations.go handles config, individual integrations use it)
