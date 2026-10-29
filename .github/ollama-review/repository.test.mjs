import assert from 'node:assert/strict';
import { execFileSync } from 'node:child_process';
import { mkdtemp, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { test } from 'node:test';
import { repositoryTools } from './repository.mjs';
import { runReview } from './engine.mjs';

test('Pi reads unchanged source through repository tools without host filesystem access', async () => {
  const root = await mkdtemp(join(tmpdir(), 'pi-review-'));
  try {
    execFileSync('git', ['init', '--bare', '--quiet', root]);
    const source = 'export function authorize(user) {\n  return user.role === "admin";\n}\n';
    const oid = execFileSync('git', ['--git-dir', root, 'hash-object', '-w', '--stdin'], { input: source, encoding: 'utf8' }).trim();
    const head = 'b'.repeat(40);
    const repository = { git_dir: root, head, files: [{ path: 'src/auth.js', oid, size: Buffer.byteLength(source) }], omitted: 1 };
    const tools = repositoryTools(repository, head);
    const invoke = async (name, args) => JSON.parse((await tools.find(tool => tool.name === name).execute('x', args)).content[0].text);
    assert.deepEqual((await invoke('list_files', {})).paths, ['src/auth.js']);
    assert.deepEqual((await invoke('search_code', { query: 'authorize' })).matches,
      [{ path: 'src/auth.js', line: 1, text: 'export function authorize(user) {' }]);
    assert.match((await invoke('read_file', { path: 'src/auth.js', start_line: 2, end_line: 2 })).text, /2:.*admin/);
    for (const path of ['/etc/passwd', '../secret', '.env', oid]) {
      await assert.rejects(invoke('read_file', { path }), /not an available/);
    }
    assert.throws(() => repositoryTools(repository, 'c'.repeat(40)), /snapshot/);

    let requests = 0;
    const result = await runReview({ policy: 'Inspect authorization before reviewing.', budget_usd: 5, review: {
      head, files: [{ path: 'src/caller.js', patch: '+authorize(user)' }],
    } }, { apiKey: 'fake-test-key', repository, fetch: async (_url, init) => {
      requests++;
      const payload = JSON.parse(init.body);
      let delta, reason;
      if (requests === 1) {
        delta = { tool_calls: [{ index: 0, id: 'auth', type: 'function', function: {
          name: 'read_file', arguments: JSON.stringify({ path: 'src/auth.js' }),
        } }] };
        reason = 'tool_calls';
      } else {
        const evidence = payload.messages.find(message => message.role === 'tool');
        assert.match(evidence.content, /admin/);
        delta = { content: '{"findings":[]}' };
        reason = 'stop';
      }
      const chunks = [{ choices: [{ index: 0, delta, finish_reason: reason }] },
        { choices: [], usage: { prompt_tokens: 100, completion_tokens: 10 } }];
      return new Response(chunks.map(chunk => `data: ${JSON.stringify(chunk)}\n\n`).join('') + 'data: [DONE]\n\n',
        { headers: { 'content-type': 'text/event-stream' } });
    } });
    assert.equal(result.requests, 2);
    assert.equal(result.tool_calls, 1);
    assert.equal(result.files_read, 1);
    assert.deepEqual(result.findings, []);
  } finally {
    await rm(root, { recursive: true, force: true });
  }
});
