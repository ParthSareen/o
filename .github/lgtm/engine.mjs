import { Agent } from '@earendil-works/pi-agent-core';
import { streamSimple } from '@earendil-works/pi-ai/api/openai-completions';
import { readFile, writeFile } from 'node:fs/promises';
import { pathToFileURL } from 'node:url';
import { repositoryTools } from './repository.mjs';
import { compactToolHistory } from './context.mjs';
import { reviewModel, limits, cost as estimateCost, ReviewError } from './config.mjs';

function providerFailure(message = '') {
  const outputLimit = message.match(/maximum output tokens \((\d+)\)/)?.[1];
  if (outputLimit) return `Ollama rejected the output allowance; this model supports at most ${outputLimit} output tokens.`;
  const status = message.match(/\b(400|401|403|408|413|422|429|5\d\d)\b/)?.[1];
  if (status) return `Ollama request failed (HTTP ${status}); check credentials, limits, or provider availability.`;
  if (/abort|timeout|timed out/i.test(message)) return 'Ollama request timed out or was cancelled.';
  return 'Ollama request failed before a complete response; no review is claimed.';
}

export async function runReview(input, { apiKey, repository, fetch = globalThis.fetch, timeoutMs = limits.inferenceMs } = {}) {
  if (!apiKey) throw new ReviewError('OLLAMA_API_KEY is required.');
  if (!input || typeof input.policy !== 'string' || !Array.isArray(input.review?.files)) {
    throw new ReviewError('Invalid review input.');
  }
  const model = reviewModel(input.model, input.thinking);
  const budget = input.budget_usd;
  const mode = input.mode ?? 'repository';
  if (!['repository', 'diff'].includes(mode)) throw new ReviewError('Invalid review mode.');
  if (!Number.isFinite(budget) || budget <= 0 || budget > limits.budget) throw new ReviewError('Invalid review cost allowance.');
  let requests = 0;
  let failure;
  let submission;
  let submissionCalls = 0;
  let peakInputTokens = 0;
  const compactedTools = new Set();
  const usage = { prompt_tokens: 0, completion_tokens: 0, cached_prompt_tokens: 0 };
  const deadline = performance.now() + timeoutMs;
  const cost = () => estimateCost(usage, model);
  const filesRead = new Set();
  let toolCalls = 0;
  const readers = (mode === 'diff' ? [] : repositoryTools(repository, input.review.head, path => filesRead.add(path))).map(tool => ({
    ...tool, execute: (...args) => { toolCalls++; return tool.execute(...args); },
  }));
  const changedPaths = new Set(input.review.files.map(file => file.path));
  const line = { anyOf: [{ type: 'integer', minimum: 1 }, { type: 'null' }] };
  const submitReview = {
    name: 'submit_review', label: 'Submit review',
    description: 'Finish this review with all supported findings. Call once, alone, after investigation. Use an empty findings array when no bug is supported. This records the result locally; it does not publish to GitHub.',
    parameters: {
      type: 'object', additionalProperties: false, required: ['findings'],
      properties: { findings: { type: 'array', items: {
        type: 'object', additionalProperties: false,
        required: ['path', 'start_line', 'end_line', 'body'],
        properties: { path: { type: 'string', minLength: 1 }, start_line: line, end_line: line,
          body: { type: 'string', minLength: 1, maxLength: 8000 } },
      } } },
    },
    execute: async (_id, args, signal) => {
      if (signal?.aborted) throw new Error('Review cancelled.');
      if (submission) throw new Error('Review already submitted.');
      const findings = args.findings.map(finding => {
        if (!changedPaths.has(finding.path)) throw new Error('Finding must refer to a supplied changed file.');
        if (!finding.body.trim()) throw new Error('Finding body must not be blank.');
        const { start_line: start, end_line: end } = finding;
        if ((start === null) !== (end === null) || (start !== null && (start > end || end - start >= 20))) {
          throw new Error('Use a valid span of at most 20 lines, or null for both line numbers.');
        }
        return { ...finding, body: finding.body.trim() };
      });
      submission = { findings };
      return { content: [{ type: 'text', text: `Recorded ${findings.length} findings. Review complete.` }],
        details: {}, terminate: true };
    },
  };
  const tools = [...readers, submitReview];
  const names = new Set(tools.map(tool => tool.name));
  const incomplete = message => new ReviewError(
    `${message} (${requests} requests attempted; uncached estimate for reported usage $${cost().toFixed(4)}.)`,
    { requests, tool_calls: toolCalls, files_read: filesRead.size, usage: { ...usage },
      peak_input_tokens: peakInputTokens, compacted_tool_results: compactedTools.size, submission_calls: submissionCalls },
  );
  const agent = new Agent({
    initialState: { model, tools, thinkingLevel: input.thinking, systemPrompt: input.policy },
    toolExecution: 'sequential',
    beforeToolCall: ({ toolCall, assistantMessage }) => {
      if (toolCall.name === 'submit_review' && assistantMessage.content.filter(block => block.type === 'toolCall').length !== 1) {
        return { block: true, reason: 'Call submit_review by itself after all investigation tools have completed.' };
      }
    },
    transformContext: async messages => {
      if (mode === 'diff') return messages;
      const result = compactToolHistory(messages);
      for (const id of result.compacted) compactedTools.add(id);
      return result.messages;
    },
    streamFn: (selectedModel, context, options) => {
      if (performance.now() >= deadline) {
        failure = 'The review exceeded its runtime allowance.';
        throw new ReviewError(failure);
      }
      if (mode === 'diff' && requests !== 0) {
        failure = 'Diff review cannot make additional model requests.';
        throw new ReviewError(failure);
      }
      let dispatched = false;
      return streamSimple(selectedModel, context, {
        ...options,
        apiKey,
        maxRetries: 0,
        timeoutMs: Math.max(1, Math.ceil(deadline - performance.now())),
        cacheRetention: 'none',
        fetch: (url, init) => {
          if (dispatched) throw new ReviewError('Unexpected transport retry.');
          dispatched = true;
          requests++;
          return fetch(url, { ...init, redirect: 'error' });
        },
        onPayload: (payload) => {
          delete payload.max_tokens;
          delete payload.max_completion_tokens;
          // UTF-8 bytes bound text tokenization; leave margin for chat/tool framing.
          const inputReserve = Buffer.byteLength(JSON.stringify(payload)) + 2048 + payload.messages.length * 128;
          const remaining = budget - cost() - inputReserve * model.cost.input / 1e6;
          const outputAllowance = model.cost.output === 0 && remaining >= 0 ? Infinity : Math.floor(remaining * 1e6 / model.cost.output);
          const maxTokens = Math.min(model.maxTokens, model.contextWindow - inputReserve, outputAllowance);
          if (maxTokens < 1) {
            failure = 'The remaining cost or context allowance cannot cover another model request; review is incomplete.';
            throw new ReviewError(failure);
          }
          payload.max_tokens = maxTokens;
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
      usage.cached_prompt_tokens += counts[1];
      peakInputTokens = Math.max(peakInputTokens, counts[0] + counts[1] + counts[2]);
      if (cost() > budget) {
        failure = 'The review exceeded its cost allowance; review is incomplete.';
        return { action: 'end' };
      }
      const calls = message.content.filter(block => block.type === 'toolCall');
      submissionCalls += calls.filter(call => call.name === 'submit_review').length;
      if (calls.some(call => !names.has(call.name))) {
        failure = 'The model requested an unavailable tool; review is incomplete.';
        return { action: 'end' };
      }
      if (submission) return { action: 'end' };
      if (mode === 'diff') return { action: 'end' };
      if (message.stopReason === 'toolUse' && message.rawStopReason === 'tool_calls' && calls.length) return;
      return { action: 'end' };
    },
  });
  const cancel = () => agent.abort();
  const timer = setTimeout(cancel, timeoutMs);
  process.once('SIGTERM', cancel);
  try {
    await agent.prompt(JSON.stringify(input.review));
    if (failure) throw incomplete(failure);
    const answer = agent.state.messages.findLast(message => message.role === 'assistant');
    if (!answer || answer.stopReason === 'error' || answer.stopReason === 'aborted') throw incomplete(providerFailure(answer?.errorMessage));
    if (!submission || answer.stopReason !== 'toolUse' || answer.rawStopReason !== 'tool_calls') {
      throw incomplete('The model ended without a valid submit_review call; review is incomplete.');
    }
    return { findings: submission.findings, model: model.id, thinking: input.thinking, requests, tool_calls: toolCalls, files_read: filesRead.size,
      finish_reason: 'submitted', usage, peak_input_tokens: peakInputTokens,
      compacted_tool_results: compactedTools.size, submission_calls: submissionCalls };
  } catch (error) {
    if (error instanceof ReviewError) throw error;
    throw incomplete(failure ?? 'Pi could not complete the review; check runtime and input.');
  } finally {
    clearTimeout(timer);
    process.removeListener('SIGTERM', cancel);
  }
}

if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) {
  try {
    const input = JSON.parse(await readFile(process.argv[2], 'utf8'));
    const repository = input.mode === 'diff' ? undefined : JSON.parse(await readFile(process.argv[4], 'utf8'));
    const result = await runReview(input, { apiKey: process.env.OLLAMA_API_KEY, repository });
    await writeFile(process.argv[3], JSON.stringify(result));
  } catch (error) {
    const reason = error instanceof ReviewError ? error.message : 'Pi could not complete the review; check runtime and input.';
    await writeFile(process.argv[3], JSON.stringify({ ...(error instanceof ReviewError ? error.metrics : {}), error: reason }));
    process.exitCode = 1;
  }
}
