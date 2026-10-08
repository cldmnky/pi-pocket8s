#!/usr/bin/env node
// Read a rotating, audience-bound SA token for every operation. Never persist a
// GitHub token, put one in a remote URL, or inherit personal GH_TOKEN settings.
import { readFileSync } from 'node:fs';
import { spawn, execFileSync } from 'node:child_process';
import { request } from 'node:https';
import { fileURLToPath } from 'node:url';

export function repository(value) {
  const name = String(value || '').replace(/\.git$/, '').toLowerCase();
  if (!/^[a-z0-9][a-z0-9_.-]*\/[a-z0-9][a-z0-9_.-]*$/.test(name)) throw new Error('Select a GitHub repository as owner/name');
  return name;
}

export function fromRemote(remote) {
  const ssh = /^git@github\.com:([^\s]+)$/.exec(remote);
  if (ssh) return repository(ssh[1]);
  const url = new URL(remote);
  if (url.protocol !== 'https:' || url.hostname !== 'github.com' || url.port || url.search || url.hash) throw new Error('Only github.com repositories are supported');
  return repository(url.pathname.slice(1));
}

export function selectRepository(args, env, remote) {
  if (env.GH_HOST && env.GH_HOST !== 'github.com') throw new Error('Only github.com is supported');
  for (let i = 0; i < args.length; i++) {
    if (args[i] === '--hostname' && args[i + 1] !== 'github.com') throw new Error('Only github.com is supported');
    if (args[i].startsWith('--hostname=') && args[i] !== '--hostname=github.com') throw new Error('Only github.com is supported');
  }
  if (args[0] === 'api') {
    if (args.some(a => /^https?:\/\//.test(a))) throw new Error('Absolute gh api URLs are not allowed');
    const apiRepo = args.map(a => /^(?:\/)?repos\/([^/]+\/[^/]+)/.exec(a)).find(Boolean);
    if (apiRepo) return repository(apiRepo[1]);
  }
  for (let i = 0; i < args.length; i++) {
    if (args[i] === '-R' || args[i] === '--repo') return repository(args[i + 1]);
    if (args[i].startsWith('--repo=')) return repository(args[i].slice(7));
  }
  // gh repo clone OWNER/NAME has no existing Git remote yet.
  if (args[0] === 'repo' && args[1] === 'clone') return args[2]?.startsWith('https://') ? fromRemote(args[2]) : repository(args[2]);
  if (env.GH_REPO) return repository(env.GH_REPO);
  return fromRemote(remote());
}

export async function credential(repo, env = process.env) {
  const url = new URL(env.GITHUB_BROKER_URL || '');
  if (url.protocol !== 'https:' || url.username || url.password || url.search || url.hash || url.pathname !== '/api/github/credentials') throw new Error('Configure a trusted HTTPS GitHub broker URL');
  const token = readFileSync(env.GITHUB_BROKER_TOKEN_FILE || '/run/github-broker/token', 'utf8').trim();
  if (!token || /\s/.test(token)) throw new Error('Workspace broker token unavailable');
  const body = JSON.stringify({ repository: repository(repo) });
  const ca = env.GITHUB_BROKER_CA_FILE ? readFileSync(env.GITHUB_BROKER_CA_FILE) : undefined;
  return await new Promise((resolve, reject) => {
    const req = request(url, { method: 'POST', ca, headers: { Authorization: 'Bearer ' + token, 'Content-Type': 'application/json', 'Content-Length': Buffer.byteLength(body) } }, res => {
      let data = '';
      res.on('data', chunk => { data += chunk; if (data.length > 65536) req.destroy(new Error('Oversized broker response')); });
      res.on('end', () => {
        try {
          if (res.statusCode !== 200) throw new Error('GitHub credentials denied or unavailable');
          const result = JSON.parse(data);
          if (typeof result.token !== 'string' || !result.token || /\s/.test(result.token) || !(Date.parse(result.expires_at) > Date.now() + 30000)) throw new Error('Invalid or expired GitHub credentials');
          resolve(result.token);
        } catch (err) { reject(err); }
      });
    });
    req.setTimeout(15000, () => req.destroy(new Error('GitHub credential broker timed out')));
    req.on('error', () => reject(new Error('GitHub credential broker unavailable (check HTTPS trust and configuration)')));
    req.end(body);
  });
}

export async function run(args) {
  if (args[0] !== '--git' && (!process.env.GITHUB_BROKER_URL || args.includes('--version') || args.includes('--help') || args[0] === 'help')) {
    const child = spawn('/usr/local/libexec/gh', args, { stdio: 'inherit', env: process.env });
    child.on('error', () => { console.error('GitHub CLI unavailable'); process.exitCode = 1; });
    await new Promise(resolve => child.on('exit', code => { process.exitCode = code ?? 1; resolve(); })); return;
  }
  if (args[0] === '--git') {
    if (args[1] !== 'get') return; // never store/erase credentials on disk
    let input = ''; for await (const chunk of process.stdin) { input += chunk; if (input.length > 8192) throw new Error('Invalid Git credential request'); }
    const fields = Object.fromEntries(input.trim().split('\n').map(line => { const i = line.indexOf('='); return [line.slice(0, i), line.slice(i + 1)]; }));
    if (fields.protocol !== 'https' || fields.host !== 'github.com') return;
    const token = await credential(fields.path);
    process.stdout.write('username=x-access-token\npassword=' + token + '\n\n'); return;
  }
  const repo = selectRepository(args, process.env, () => execFileSync('git', ['remote', 'get-url', 'origin'], { encoding: 'utf8', stdio: ['ignore', 'pipe', 'ignore'] }).trim());
  const token = await credential(repo);
  const env = { ...process.env, GH_TOKEN: token, GH_REPO: repo, GH_HOST: 'github.com' }; delete env.GITHUB_TOKEN;
  const child = spawn('/usr/local/libexec/gh', args, { stdio: 'inherit', env });
  for (const signal of ['SIGTERM', 'SIGINT', 'SIGHUP']) process.on(signal, () => child.kill(signal));
  child.on('error', () => { console.error('GitHub CLI unavailable'); process.exitCode = 1; });
  await new Promise(resolve => child.on('exit', (code, signal) => { process.exitCode = code ?? (signal ? 128 : 1); resolve(); }));
}

if (process.argv[1] === fileURLToPath(import.meta.url)) run(process.argv.slice(2)).catch(() => { console.error('GitHub credentials unavailable: configure an allowed repository, HTTPS broker, and projected service-account token'); process.exitCode = 1; });
