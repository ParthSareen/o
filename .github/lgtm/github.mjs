import { execFile } from 'node:child_process';
import { promisify } from 'node:util';
import { join } from 'node:path';
import { limits, ReviewError } from './config.mjs';

const execute = promisify(execFile);
const patterns = ['vendor/**', '**/vendor/**', 'node_modules/**', '**/node_modules/**',
  '*.lock', 'go.sum', '*.pb.go', '*.generated.*', '*.min.js', '*.map', '*.png', '*.jpg',
  '*.jpeg', '*.gif', '*.webp', '*.ico', '*.pdf', '*.zip', '*.gz', '*.woff*', '*.ttf',
  '.env*', '**/.env*', '*.pem', '*.key'];
const excludedPatterns = patterns.map(pattern => new RegExp('^' + pattern.replace(/[.+^${}()|[\]\\]/g, '\\$&').replaceAll('*', '.*') + '$'));
export const excluded = path => excludedPatterns.some(pattern => pattern.test(path));
export const prefix = mode => mode === 'diff' ? 'ollama-review-diff' : 'ollama-review';

export function revision(pr) {
  const value = { base: pr.base?.sha, head: pr.head?.sha };
  if (!Object.values(value).every(sha => /^[a-f0-9]{40}$/.test(sha ?? ''))) throw new ReviewError('Invalid PR revision.');
  return value;
}

export function sameRevision(pr, request) {
  const current = revision(pr);
  return pr.state === 'open' && current.base === request.base && current.head === request.head;
}

export function rightLines(patch) {
  let line;
  const lines = [];
  for (const text of patch.split('\n')) {
    const match = /^@@ -\d+(?:,\d+)? \+(\d+)(?:,\d+)? @@/.exec(text);
    if (match) line = Number(match[1]);
    else if (line !== undefined && /^[ +]/.test(text)) lines.push(line++);
  }
  return lines;
}

export class GitHub {
  constructor(repository, token, fetch = globalThis.fetch) {
    if (!/^[\w.-]+\/[\w.-]+$/.test(repository ?? '') || !token) throw new ReviewError('GitHub repository and token are required.');
    Object.assign(this, { repository, token, fetch, trees: new Map() });
  }
  async request(method, path, body) {
    try {
      const response = await this.fetch(`https://api.github.com/repos/${this.repository}/${path}`, {
        method, redirect: 'error', signal: AbortSignal.timeout(30000),
        headers: { Authorization: `Bearer ${this.token}`, Accept: 'application/vnd.github+json',
          'Content-Type': 'application/json', 'X-GitHub-Api-Version': '2022-11-28' },
        ...(body === undefined ? {} : { body: JSON.stringify(body) }),
      });
      if (!response.ok) throw new ReviewError(`GitHub request failed (HTTP ${response.status}).`);
      return await response.json();
    } catch (error) {
      throw error instanceof ReviewError ? error : new ReviewError('GitHub request failed or returned invalid JSON.');
    }
  }
  get(path) { return this.request('GET', path); }
  async pages(path) {
    const all = [];
    for (let page = 1; page <= 100; page++) {
      const values = await this.get(`${path}?per_page=100&page=${page}`);
      if (!Array.isArray(values)) throw new ReviewError('Invalid GitHub list.');
      all.push(...values);
      if (values.length < 100) return all;
    }
    throw new ReviewError('GitHub list exceeds the pagination limit.');
  }
  pull(number) { return this.get(`pulls/${number}`); }
  async permitted(login) {
    if (!/^[\w-]+$/.test(login ?? '')) return false;
    return ['admin', 'maintain', 'write'].includes((await this.get(`collaborators/${encodeURIComponent(login)}/permission`)).permission);
  }
  async files(pr) {
    if (!Number.isInteger(pr.changed_files) || pr.changed_files < 0 || pr.changed_files > limits.files) throw new ReviewError('PR exceeds the 100-file limit.');
    const files = await this.get(`pulls/${pr.number}/files?per_page=100`);
    if (!Array.isArray(files) || files.length !== pr.changed_files) throw new ReviewError('Incomplete changed-file list.');
    return files;
  }
  async source(path, sha) {
    if (excluded(path)) return null;
    if (!this.trees.has(sha)) {
      const tree = await this.get(`git/trees/${sha}?recursive=1`);
      this.trees.set(sha, new Map((tree.truncated ? [] : tree.tree ?? [])
        .filter(entry => entry.type === 'blob' && ['100644', '100755'].includes(entry.mode))
        .map(entry => [entry.path, entry.sha])));
    }
    const oid = this.trees.get(sha).get(path);
    if (!/^[a-f0-9]{40}$/.test(oid ?? '')) return null;
    const data = await this.get(`git/blobs/${oid}`);
    if (data.encoding !== 'base64' || data.size > limits.blob) return null;
    const bytes = Buffer.from(data.content, 'base64');
    if (bytes.length > limits.blob) return null;
    const text = new TextDecoder('utf-8', { fatal: true }).decode(bytes);
    return text.includes('\0') ? null : text;
  }
}

export async function snapshot(request, directory, github, run = execute) {
  const root = join(directory, 'repository.git');
  const env = { PATH: process.env.PATH, HOME: directory, GIT_CONFIG_NOSYSTEM: '1',
    GIT_CONFIG_GLOBAL: '/dev/null', GIT_TERMINAL_PROMPT: '0' };
  const git = async (args, authenticate = false) => {
    try {
      const { stdout } = await run('git', ['--no-replace-objects', ...args], {
        env: authenticate ? { ...env, GIT_CONFIG_COUNT: '1',
          GIT_CONFIG_KEY_0: 'http.https://github.com/.extraheader',
          GIT_CONFIG_VALUE_0: `AUTHORIZATION: basic ${Buffer.from(`x-access-token:${github.token}`).toString('base64')}` } : env,
        encoding: 'buffer', timeout: 60000, maxBuffer: 8 * 1024 * 1024,
      });
      return stdout;
    } catch { throw new ReviewError('Could not prepare the read-only repository snapshot.'); }
  };
  await git(['init', '--bare', '--quiet', root]);
  const args = ['--git-dir', root, '-c', 'core.hooksPath=/dev/null'];
  await git([...args, 'fetch', '--quiet', '--depth=1', '--no-tags', '--no-recurse-submodules',
    `https://github.com/${github.repository}.git`, `refs/pull/${request.number}/head`], true);
  if ((await git([...args, 'rev-parse', 'FETCH_HEAD'])).toString().trim() !== request.head) throw new ReviewError('PR changed while fetching the snapshot.');
  const tree = await git([...args, 'ls-tree', '-r', '-l', '-z', request.head]);
  const files = [];
  let omitted = 0;
  for (const record of tree.toString('utf8').split('\0').filter(Boolean)) {
    const tab = record.indexOf('\t');
    const [mode, kind, oid, size] = record.slice(0, tab).trim().split(/\s+/);
    const path = record.slice(tab + 1);
    if (kind !== 'blob' || !['100644', '100755'].includes(mode) || excluded(path)
        || path.includes('\ufffd') || Number(size) > limits.blob) { omitted++; continue; }
    files.push({ path, oid, size: Number(size) });
  }
  return { git_dir: root, head: request.head, files, omitted };
}
