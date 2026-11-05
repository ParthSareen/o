import { createHash } from 'node:crypto';
import { cost, metrics, reviewModel, ReviewError } from './config.mjs';
import { prefix } from './github.mjs';

export function usageMetrics(raw, model) {
  const result = Object.fromEntries(metrics.filter(key => Number.isSafeInteger(raw[key]) && raw[key] >= 0).map(key => [key, raw[key]]));
  const usage = raw.usage;
  if (usage && ['prompt_tokens', 'completion_tokens'].every(key => Number.isSafeInteger(usage[key]) && usage[key] >= 0)) {
    result.usage = { prompt_tokens: usage.prompt_tokens, completion_tokens: usage.completion_tokens };
    result.estimated_cost_usd = cost(usage, model);
    if (Number.isSafeInteger(usage.cached_prompt_tokens) && usage.cached_prompt_tokens >= 0 && usage.cached_prompt_tokens <= usage.prompt_tokens) {
      result.usage.cached_prompt_tokens = usage.cached_prompt_tokens;
      result.cache_adjusted_cost_usd = cost(usage, model, true);
    }
  }
  return result;
}

export function normalize(raw, request) {
  const model = reviewModel(request.model, request.thinking);
  const measured = usageMetrics(raw, model);
  if ((raw.model !== undefined && raw.model !== model.id) || (raw.thinking !== undefined && raw.thinking !== request.thinking)) {
    throw new ReviewError('Review result uses a different model or thinking level.', measured);
  }
  if (raw.finish_reason !== 'submitted' || !Number.isSafeInteger(raw.requests) || raw.requests < 1
      || !Number.isSafeInteger(raw.submission_calls) || raw.submission_calls < 1 || !Array.isArray(raw.findings)) {
    throw new ReviewError('Review did not complete through submit_review.', measured);
  }
  if (!measured.usage || measured.estimated_cost_usd > request.budget_usd
      || (raw.usage.cached_prompt_tokens !== undefined && measured.usage.cached_prompt_tokens === undefined)
      || metrics.some(key => raw[key] !== undefined && (!Number.isSafeInteger(raw[key]) || raw[key] < 0))) {
    throw new ReviewError('Invalid or over-budget review usage.', measured);
  }
  if (request.mode === 'diff' && (raw.requests !== 1 || raw.submission_calls !== 1 || raw.tool_calls || raw.files_read)) {
    throw new ReviewError('Diff review must use one request with only submit_review.', measured);
  }
  const seen = new Set();
  const findings = [];
  for (const finding of raw.findings) {
    const { path, body, start_line: start, end_line: end } = finding ?? {};
    if (typeof path !== 'string' || !Object.hasOwn(request.eligible, path) || typeof body !== 'string' || !body.trim() || body.length > 8000) {
      throw new ReviewError('Invalid finding or path outside the reviewed changes.', measured);
    }
    const fingerprint = createHash('sha256').update(path + '\n' + body.trim().split(/\s+/).join(' ')).digest('hex');
    if (seen.has(fingerprint)) continue;
    seen.add(fingerprint);
    const visible = request.eligible[path];
    const anchored = Number.isSafeInteger(start) && Number.isSafeInteger(end) && start > 0 && end >= start && end - start < 20
      && Array.from({ length: end - start + 1 }, (_, i) => start + i).every(line => visible.includes(line));
    findings.push({ path, body: body.trim(), start_line: anchored ? start : null, end_line: anchored ? end : null, fingerprint });
  }
  return { ...measured, status: 'complete', finish_reason: 'submitted', findings,
    base: request.base, head: request.head, mode: request.mode, profile: request.profile, model: model.id, thinking: request.thinking };
}

export const safeText = value => String(value).replaceAll('&', '&amp;').replaceAll('<', '&lt;').replaceAll('>', '&gt;')
  .replaceAll('"', '&quot;').replaceAll("'", '&#x27;').replaceAll('@', '@\u200b').replaceAll('![', '\\![');
export const marker = request => `<!-- ${prefix(request.mode)}-status run=${request.run_id} attempt=${request.attempt} -->`;

export function render(request, state, result = {}) {
  const titles = { queued: 'Queued', running: 'Running', complete: 'Complete', incomplete: 'Could not complete', cancelled: 'Cancelled', stale: 'Superseded' };
  const name = request.mode === 'diff' ? 'Ollama diff review' : 'Ollama review';
  const lines = [marker(request), `### ${name} — ${titles[state]}`, '',
    `[View run](https://github.com/${request.repository}/actions/runs/${request.run_id}) · Head \`${request.head.slice(0, 12)}\` · \`${safeText(request.model.id)}\` · Thinking \`${safeText(request.thinking)}\` · Profile \`${request.profile}\``, ''];
  if (state === 'queued') lines.push('Acknowledged. Your review request was accepted.');
  if (state === 'running') lines.push('The pipeline has started. Preparing the runtime and analyzing the change.');
  if (state === 'cancelled') lines.push('This run was cancelled. A newer request may be running.');
  if (state === 'stale') lines.push('The PR changed during review. Request a new review for the current revision.');
  if (state === 'incomplete') lines.push(safeText(result.reason ?? 'Review did not complete. See the run for details.'));
  if (state === 'complete') {
    if (!result.findings.length) lines.push('No actionable findings in the analyzed changes. This does not establish correctness.');
    for (const [i, finding] of result.findings.entries()) {
      lines.push(`#### ${i + 1}. <code>${safeText(finding.path)}${finding.start_line ? ':' + finding.start_line : ' (no inline location)'}</code>`, '', safeText(finding.body), '');
    }
    lines.push('Automated findings need verification. This review does not approve the PR.');
  }
  for (const [key, label] of [['omitted', 'Omitted from diff'], ['context_limited', 'Surrounding context unavailable']]) {
    if (result[key]?.length) lines.push(`${label}: ${result[key].map(value => `<code>${safeText(value)}</code>`).join(', ')}`);
  }
  if (result.usage) {
    lines.push('', `${result.requests ?? 0} model requests · ${result.tool_calls ?? 0} repository tool calls · ${result.files_read ?? 0} distinct files read`,
      `${result.usage.prompt_tokens.toLocaleString('en-US')} cumulative input tokens · ${result.usage.completion_tokens.toLocaleString('en-US')} output tokens · uncached estimate $${result.estimated_cost_usd.toFixed(4)}`,
      `Largest prompt: ${result.peak_input_tokens ?? 0} tokens · ${result.compacted_tool_results ?? 0} tool payloads compacted · ${result.submission_calls ?? 0} submission calls`);
    if (result.cache_adjusted_cost_usd !== undefined) lines.push(`Cache-adjusted estimate $${result.cache_adjusted_cost_usd.toFixed(4)}; actual billing may differ.`);
  }
  if (result.note) lines.push('', safeText(result.note));
  return lines.join('\n');
}
