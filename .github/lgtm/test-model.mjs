// Fixed mock-model metadata keeps tests independent of deployment settings.
export const testConfig = {
  prompt: 'Review the change for concrete, actionable regressions.',
  models: {
    'test-model': { contextWindow: 1048576, maxTokens: 393216,
      cost: { input: 0.30, output: 1.20, cacheRead: 0.006 }, reasoning: true,
      thinkingLevelMap: { off: 'none', low: 'low', medium: 'medium', high: 'high' } },
  },
  repository: { model: 'test-model', thinking: 'low', profile: 'general', budget_usd: 5 },
  diff: { model: 'test-model', thinking: 'low', profile: 'general', budget_usd: 5 },
};
