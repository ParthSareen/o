// Compact old tool payloads without inventing a summary or breaking tool-call pairs.
export function compactToolHistory(messages, { thresholdBytes = 256 * 1024, keepRecent = 8 } = {}) {
  if (Buffer.byteLength(JSON.stringify(messages)) <= thresholdBytes) return { messages, compacted: [] };
  const indexes = messages.flatMap((message, index) => message.role === 'toolResult' ? [index] : []);
  const old = new Set(indexes.slice(0, Math.max(0, indexes.length - keepRecent)));
  const compacted = [];
  const projected = messages.map((message, index) => {
    if (!old.has(index) || !Array.isArray(message.content)) return message;
    const replacement = { compacted: true, note: 'Earlier tool output omitted to save context. Repeat its original tool call if the evidence is needed.' };
    const content = message.content.filter(block => block.type === 'text').map(block => block.text).join('');
    try {
      const value = JSON.parse(content);
      if (value.compacted) return message;
      for (const key of ['path', 'total_lines', 'next_line', 'next_offset', 'unavailable_files']) {
        if (key in value) replacement[key] = value[key];
      }
    } catch { /* The original tool call still records the requested path/query. */ }
    const text = JSON.stringify(replacement);
    if (Buffer.byteLength(text) >= Buffer.byteLength(content)) return message;
    compacted.push(message.toolCallId);
    return { ...message, content: [{ type: 'text', text }] };
  });
  return { messages: projected, compacted };
}
