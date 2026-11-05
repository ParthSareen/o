import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { test } from 'node:test';
import { cost, settings } from './config.mjs';
import { testConfig } from './test-model.mjs';

test('deployment settings resolve for both modes', () => {
  const config = JSON.parse(readFileSync(new URL('./settings.json', import.meta.url), 'utf8'));
  for (const mode of ['repository', 'diff']) assert.doesNotThrow(() => settings(config, mode));
});

test('modes select independent models and thinking levels with their own prices', () => {
  const config = structuredClone(testConfig);
  config.models['custom-review'] = { contextWindow: 32768, maxTokens: 4096,
    reasoning: false, cost: { input: 2, output: 4, cacheRead: 0.5 } };
  Object.assign(config.repository, { thinking: 'high' });
  Object.assign(config.diff, { model: 'custom-review', thinking: 'off' });
  const repository = settings(config, 'repository'), diff = settings(config, 'diff');
  assert.equal(repository.model.id, 'test-model');
  assert.equal(repository.thinking, 'high');
  assert.equal(diff.model.id, 'custom-review');
  assert.equal(diff.thinking, 'off');
  const usage = { prompt_tokens: 1000, completion_tokens: 100, cached_prompt_tokens: 500 };
  assert.equal(cost(usage, diff.model), 0.0024);
  assert.equal(cost(usage, diff.model, true), 0.00165);
  config.models['custom-review'].cost.input = 99;
  assert.equal(diff.model.cost.input, 2, 'resolved configuration is a snapshot');
});

test('invalid model metadata and unsupported thinking fail before a review can start', () => {
  const invalid = [
    config => { config.repository.model = 'unknown'; },
    config => { delete config.repository.thinking; },
    config => { config.repository.thinking = 'typo'; },
    config => { config.repository.thinking = 'xhigh'; },
    config => { config.models[config.repository.model].maxTokens = 2000000; },
    config => { config.models[config.repository.model].contextWindow = -1; },
    config => { config.models[config.repository.model].cost.output = -1; },
    config => { config.models[config.repository.model].cost.input = NaN; },
    config => { config.models[config.repository.model].reasoning = false; },
    config => { config.models[config.repository.model].thinkingLevelMap.low = null; },
  ];
  for (const change of invalid) {
    const config = structuredClone(testConfig);
    change(config);
    assert.throws(() => settings(config, 'repository'));
  }
});

test('prompts inherit shared guidance or use a validated per-mode override', () => {
  const config = structuredClone(testConfig);
  assert.equal(settings(config, 'repository').prompt, config.prompt);
  config.repository.prompt = null;
  assert.equal(settings(config, 'repository').prompt, config.prompt);
  config.repository.prompt = 'Focus on data loss.\nCheck failure paths.';
  assert.equal(settings(config, 'repository').prompt, config.repository.prompt);
  assert.equal(settings(config, 'diff').prompt, config.prompt);
  config.repository.prompt = 'x'.repeat(32 * 1024 - 2);
  assert.equal(settings(config, 'repository').prompt, config.repository.prompt);
  delete config.repository.prompt;
  const invalid = ['', ' \n ', {}, 42, false, 'x'.repeat(32 * 1024 - 1), '\n'.repeat(16 * 1024)];
  for (const target of [config, config.repository]) {
    for (const prompt of target === config ? [...invalid, null, undefined] : invalid) {
      target.prompt = prompt;
      assert.throws(() => settings(config, 'repository'), /Review prompt/);
    }
    target.prompt = testConfig.prompt;
  }
});
