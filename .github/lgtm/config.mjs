export const limits = { budget: 5, files: 100, patch: 512 * 1024, context: 48 * 1024, prompt: 32 * 1024,
  input: 1024 * 1024, blob: 256 * 1024, inferenceMs: 480000 };
export const metrics = ['requests', 'tool_calls', 'files_read', 'peak_input_tokens',
  'compacted_tool_results', 'submission_calls', 'repository_omitted'];

export class ReviewError extends Error {
  constructor(message, metrics = {}) { super(message); this.metrics = metrics; }
}

export function reviewModel(value, thinking) {
  const levels = ['off', 'minimal', 'low', 'medium', 'high', 'xhigh', 'max'];
  if (!value || typeof value.id !== 'string' || !/^[a-zA-Z0-9][\w.\/:\-]{0,199}$/.test(value.id)
      || !Number.isSafeInteger(value.contextWindow) || value.contextWindow <= 0
      || !Number.isSafeInteger(value.maxTokens) || value.maxTokens <= 0 || value.maxTokens > value.contextWindow
      || typeof value.reasoning !== 'boolean' || !levels.includes(thinking)) {
    throw new ReviewError('Invalid model limits or thinking level.');
  }
  const rates = { input: value.cost?.input, output: value.cost?.output,
    cacheRead: value.cost?.cacheRead ?? value.cost?.input, cacheWrite: value.cost?.input };
  if (!Object.values(rates).every(rate => Number.isFinite(rate) && rate >= 0) || rates.cacheRead > rates.input) {
    throw new ReviewError('Model pricing must contain nonnegative input/output rates and a valid cache rate.');
  }
  const declared = value.thinkingLevelMap ?? {};
  if (typeof declared !== 'object' || Array.isArray(declared)
      || Object.entries(declared).some(([key, level]) => !levels.includes(key)
        || (level !== null && (typeof level !== 'string' || !/^[\w.-]+$/.test(level))))) {
    throw new ReviewError('Invalid model thinking-level mapping.');
  }
  const thinkingLevelMap = Object.fromEntries(levels.map(level => [level, declared[level] ?? null]));
  if ((!value.reasoning && thinking !== 'off') || (value.reasoning && typeof thinkingLevelMap[thinking] !== 'string')) {
    throw new ReviewError(`Thinking level "${thinking}" is not configured for model "${value.id}".`);
  }
  return { id: value.id, name: value.id, contextWindow: value.contextWindow, maxTokens: value.maxTokens,
    reasoning: value.reasoning, cost: rates, thinkingLevelMap,
    provider: 'ollama-cloud', api: 'openai-completions', baseUrl: 'https://ollama.com/v1', input: ['text'],
    compat: { supportsDeveloperRole: false, supportsReasoningEffort: value.reasoning,
      supportsUsageInStreaming: true, supportsFinishReason: true, supportsStore: false,
      maxTokensField: 'max_tokens', thinkingFormat: 'openai' } };
}

export function settings(config, mode) {
  const value = config[mode];
  if (!['repository', 'diff'].includes(mode) || !['general', 'security'].includes(value?.profile)
      || !Number.isFinite(value.budget_usd) || value.budget_usd <= 0 || value.budget_usd > limits.budget) {
    throw new ReviewError('Invalid review mode, profile, or budget.');
  }
  if (typeof value.model !== 'string' || !Object.hasOwn(config.models ?? {}, value.model)) {
    throw new ReviewError('Selected model must have a definition in settings.json.');
  }
  const prompt = value.prompt ?? config.prompt;
  if (typeof prompt !== 'string' || !prompt.trim()
      || Buffer.byteLength(JSON.stringify(prompt)) > limits.prompt) {
    throw new ReviewError('Review prompt must resolve to non-empty text within the 32 KiB JSON limit.');
  }
  const model = reviewModel({ ...config.models[value.model], id: value.model }, value.thinking);
  return { profile: value.profile, budget_usd: value.budget_usd, model, thinking: value.thinking, prompt };
}

export function cost(usage, model, cached = false) {
  return (usage.prompt_tokens * model.cost.input + usage.completion_tokens * model.cost.output
    - (cached ? (usage.cached_prompt_tokens ?? 0) * (model.cost.input - model.cost.cacheRead) : 0)) / 1e6;
}
