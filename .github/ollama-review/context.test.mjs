import assert from 'node:assert/strict';
import { test } from 'node:test';
import { compactToolHistory } from './context.mjs';

test('compaction preserves the diff, assistant evidence, call pairs and recent results', () => {
  const messages = [{ role: 'user', content: 'Original diff and nearby source' }];
  for (let i = 0; i < 6; i++) {
    messages.push({ role: 'assistant', content: [{ type: 'text', text: `Candidate finding ${i}` },
      { type: 'toolCall', id: `call-${i}`, name: 'read_file', arguments: { path: `file-${i}.py` } }] });
    messages.push({ role: 'toolResult', toolCallId: `call-${i}`, toolName: 'read_file',
      content: [{ type: 'text', text: JSON.stringify({ path: `file-${i}.py`, text: 'source '.repeat(1000) }) }] });
  }
  const before = JSON.stringify(messages);
  const result = compactToolHistory(messages, { thresholdBytes: 100, keepRecent: 2 });
  assert.equal(JSON.stringify(messages), before, 'the original transcript must not be mutated');
  assert.equal(result.messages[0], messages[0]);
  assert.equal(result.messages.length, messages.length);
  assert.deepEqual(result.compacted, ['call-0', 'call-1', 'call-2', 'call-3']);
  for (let i = 0; i < 6; i++) {
    assert.equal(result.messages[1 + i * 2], messages[1 + i * 2]);
    assert.equal(result.messages[2 + i * 2].toolCallId, `call-${i}`);
  }
  assert.equal(result.messages.at(-1), messages.at(-1));
  assert.equal(result.messages.at(-3), messages.at(-3));
  const compacted = JSON.parse(result.messages[2].content[0].text);
  assert.equal(compacted.path, 'file-0.py');
  assert.equal(compacted.compacted, true);
  assert.match(compacted.note, /Repeat its original tool call/);
});

test('small requests are unchanged regardless of cumulative usage', () => {
  const messages = [{ role: 'user', content: 'small diff' }];
  const result = compactToolHistory(messages);
  assert.equal(result.messages, messages);
  assert.deepEqual(result.compacted, []);
});
