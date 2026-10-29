import { Agent } from '@earendil-works/pi-agent-core';
import { streamSimple } from '@earendil-works/pi-ai/api/openai-completions';
import { readFile, writeFile } from 'node:fs/promises';
import { pathToFileURL } from 'node:url';
import { repositoryTools } from './repository.mjs';

const model = {
  id: 'deepseek-v4.1-flash:1017',
  name: 'DeepSeek V4.1 Flash 1017',
  provider: 'ollama-cloud',
  api: 'openai-completions',
  baseUrl: 'https://ollama.com/v1',
  reasoning: true,
  input: ['text'],
  contextWindow: 1048576,
  maxTokens: 1048576,
  cost: { input: 0.30, output: 1.20, cacheRead: 0.30, cacheWrite: 0.30 },
  thinkingLevelMap: { low: 'low' },
  compat: {
    supportsDeveloperRole: false,
    supportsReasoningEffort: true,
    supportsUsageInStreaming: true,
    supportsFinishReason: true,
    supportsStore: false,
    maxTokensField: 'max_tokens',
    thinkingFormat: 'openai',
  },
};

class ReviewError extends Error {}

function providerFailure(message = '') {
  const status = message.match(/\b(400|401|403|408|413|422|429|5\d\d)\b/)?.[1];
  if (status) return `Ollama request failed (HTTP ${status}); check credentials, limits, or provider availability.`;
  if (/abort|timeout|timed out/i.test(message)) return 'Ollama request timed out or was cancelled.';
  return 'Ollama request failed before a complete response; no review is claimed.';
}

export async function runReview(input, { apiKey, repository, fetch = globalThis.fetch, timeoutMs = 480000 } = {}) {
  if (!apiKey) throw new ReviewError('OLLAMA_API_KEY is required.');
  if (!input || typeof input.policy !== 'string' || !Array.isArray(input.review?.files)) {
    throw new ReviewError('Invalid review input.');
  }
  let requests = 0;
  let payloads = 0;
  let failure;
  const usage = { prompt_tokens: 0, completion_tokens: 0 };
  const cost = () => (usage.prompt_tokens * model.cost.input + usage.completion_tokens * model.cost.output) / 1e6;
  const reservation = model.contextWindow * (model.cost.input + model.cost.output) / 1e6;
  const filesRead = new Set();
  let toolCalls = 0;
  const tools = repositoryTools(repository, input.review.head, path => filesRead.add(path)).map(tool => ({
    ...tool, execute: (...args) => { toolCalls++; return tool.execute(...args); },
  }));
  const names = new Set(tools.map(tool => tool.name));
  const agent = new Agent({
    initialState: { model, tools, thinkingLevel: 'low', systemPrompt: input.policy },
    toolExecution: 'sequential',
    streamFn: (selectedModel, context, options) => {
      if (cost() + reservation > 2) {
        failure = 'The remaining $2 review allowance cannot cover another model request; review is incomplete.';
        throw new ReviewError(failure);
      }
      requests++;
      return streamSimple(selectedModel, context, {
        ...options,
        apiKey,
        maxRetries: 0,
        timeoutMs,
        cacheRetention: 'none',
        fetch: (url, init) => fetch(url, { ...init, redirect: 'error' }),
        onPayload: (payload) => {
          if (++payloads !== requests || payload.model !== model.id
              || payload.reasoning_effort !== 'low'
              || payload.tools?.length !== tools.length
              || payload.tools.some(tool => !names.has(tool.function?.name))) {
            throw new Error('Unexpected model request configuration.');
          }
          // Calibration run: omit client limits and observe the provider's own ceiling.
          delete payload.max_tokens;
          delete payload.max_completion_tokens;
          return payload;
        },
      });
    },
    finishTurn: ({ message }) => {
      if (message.stopReason === 'error' || message.stopReason === 'aborted') return { action: 'end' };
      const metering = message.usage;
      const counts = [metering?.input, metering?.cacheRead, metering?.cacheWrite, metering?.output];
      if (!counts.every(value => Number.isInteger(value) && value >= 0)
          || counts[0] + counts[1] + counts[2] <= 0 || counts[3] <= 0) {
        failure ??= 'Token usage was absent; review is incomplete.';
        return { action: 'end' };
      }
      usage.prompt_tokens += counts[0] + counts[1] + counts[2];
      usage.completion_tokens += counts[3];
      if (cost() > 2) {
        failure = 'The review exceeded its $2 allowance; review is incomplete.';
        return { action: 'end' };
      }
      const calls = message.content.filter(block => block.type === 'toolCall');
      if (calls.some(call => !names.has(call.name))) {
        failure = 'The model requested an unavailable tool; review is incomplete.';
        return { action: 'end' };
      }
      if (message.stopReason === 'toolUse' && message.rawStopReason === 'tool_calls' && calls.length) return;
      return { action: 'end' };
    },
  });
  const cancel = () => agent.abort();
  const timer = setTimeout(cancel, timeoutMs);
  process.once('SIGTERM', cancel);
  try {
    await agent.prompt(JSON.stringify(input.review));
  } catch (error) {
    if (failure) throw new ReviewError(failure);
    throw error;
  } finally {
    clearTimeout(timer);
    process.removeListener('SIGTERM', cancel);
  }
  if (failure) throw new ReviewError(failure);
  const answer = agent.state.messages.findLast(message => message.role === 'assistant');
  if (!answer || answer.stopReason === 'error' || answer.stopReason === 'aborted') {
    throw new ReviewError(providerFailure(answer?.errorMessage));
  }
  if (answer.stopReason !== 'stop' || answer.rawStopReason !== 'stop') {
    throw new ReviewError(`Ollama returned an incomplete response (${answer.stopReason}, ${answer.usage?.output ?? 0} output tokens); no review is claimed.`);
  }
  const text = answer.content.filter(block => block.type === 'text').map(block => block.text).join('');
  let parsed;
  try {
    parsed = JSON.parse(text.trim().replace(/^```(?:json)?\s*\n([\s\S]*?)\n```$/, '$1'));
  } catch {
    throw new ReviewError(`Pi received invalid review JSON (${text.length} answer characters); no review is claimed.`);
  }
  if (!parsed || !Array.isArray(parsed.findings)) {
    throw new ReviewError('Pi received invalid findings; no review is claimed.');
  }
  return {
    findings: parsed.findings,
    requests,
    tool_calls: toolCalls,
    files_read: filesRead.size,
    finish_reason: answer.rawStopReason,
    usage,
  };
}

if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) {
  try {
    const input = JSON.parse(await readFile(process.argv[2], 'utf8'));
    const repository = JSON.parse(await readFile(process.argv[4], 'utf8'));
    const result = await runReview(input, { apiKey: process.env.OLLAMA_API_KEY, repository });
    await writeFile(process.argv[3], JSON.stringify(result));
  } catch (error) {
    const reason = error instanceof ReviewError ? error.message : 'Pi could not complete the review; check runtime and input.';
    await writeFile(process.argv[3], JSON.stringify({ error: reason }));
    process.exitCode = 1;
  }
}
