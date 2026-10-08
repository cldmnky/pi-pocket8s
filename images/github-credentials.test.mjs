import test from 'node:test';
import assert from 'node:assert/strict';
import { mkdtempSync, writeFileSync, readFileSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { execFileSync, spawn } from 'node:child_process';
import { createServer } from 'node:https';
import { credential, repository, fromRemote, selectRepository } from './github-credentials.mjs';

 test('repository selection is explicit, scoped and github.com-only', () => {
  assert.equal(repository('Example/Repo.git'), 'example/repo');
  assert.equal(fromRemote('https://github.com/example/repo.git'), 'example/repo');
  assert.equal(fromRemote('git@github.com:example/repo.git'), 'example/repo');
  assert.throws(() => fromRemote('https://evil.example/example/repo.git'));
  assert.throws(() => fromRemote('https://github.com:444/example/repo'));
  for (const bad of ['*', 'repo', 'org/repo/path', 'org/repo\npassword=leak']) assert.throws(() => repository(bad));
  assert.equal(selectRepository(['run', 'list', '-R', 'example/repo'], {}, () => ''), 'example/repo');
  assert.equal(selectRepository(['repo', 'clone', 'example/repo'], {}, () => ''), 'example/repo');
  assert.equal(selectRepository(['api', 'repos/example/repo/actions/runs'], {}, () => ''), 'example/repo');
  assert.equal(selectRepository(['run', 'list'], { GH_REPO: 'example/repo' }, () => ''), 'example/repo');
  assert.throws(() => selectRepository(['api', 'https://evil.example'], {}, () => ''));
  assert.throws(() => selectRepository(['api', '--hostname', 'evil.example', 'user'], {}, () => ''));
  assert.throws(() => selectRepository([], { GH_HOST: 'evil.example' }, () => ''));
 });

test('TLS broker, rotating SA credentials, Git helper and no persistent GitHub tokens', async () => {
 const dir = mkdtempSync(join(tmpdir(), 'pocket-github-test-'));
 const key = join(dir, 'key.pem'), cert = join(dir, 'cert.pem'), tokenFile = join(dir, 'sa-token');
 execFileSync('openssl', ['req', '-x509', '-newkey', 'rsa:2048', '-nodes', '-keyout', key, '-out', cert, '-days', '1', '-subj', '/CN=localhost', '-addext', 'subjectAltName=DNS:localhost'], { stdio: 'ignore' });
 let expectedSA = 'first-sa-token', calls = 0;
 let expires = () => new Date(Date.now() + 3600000).toISOString();
 const server = createServer({ key: readFileSync(key), cert: readFileSync(cert) }, async (req, res) => {
  let body = ''; for await (const chunk of req) body += chunk;
  calls++;
  assert.equal(req.url, '/api/github/credentials');
  if (req.headers.authorization !== 'Bearer ' + expectedSA) { res.writeHead(401); res.end('never expose upstream secret detail'); return; }
  if (JSON.parse(body).repository !== 'example/repo') { res.writeHead(403); res.end('out of scope'); return; }
  res.setHeader('Content-Type', 'application/json');
  res.end(JSON.stringify({ token: 'short-lived-bot-token', expires_at: expires() }));
 });
 await new Promise(resolve => server.listen(0, 'localhost', resolve));
 const env = { ...process.env, GITHUB_BROKER_URL: 'https://localhost:' + server.address().port + '/api/github/credentials', GITHUB_BROKER_TOKEN_FILE: tokenFile, GITHUB_BROKER_CA_FILE: cert };
 try {
  writeFileSync(tokenFile, expectedSA);
  assert.equal(await credential('example/repo', env), 'short-lived-bot-token');
  expectedSA = 'second-sa-token'; writeFileSync(tokenFile, expectedSA);
  assert.equal(await credential('example/repo', env), 'short-lived-bot-token');
  assert.equal(calls, 2);
  await assert.rejects(credential('other/repo', env), /denied or unavailable/);
  await assert.rejects(credential('example/repo', { ...env, GITHUB_BROKER_CA_FILE: '' }), /unavailable/);
  await assert.rejects(credential('example/repo', { ...env, GITHUB_BROKER_URL: env.GITHUB_BROKER_URL.replace('https:', 'http:') }), /HTTPS/);
  expires = () => '2000-01-01T00:00:00Z'; await assert.rejects(credential('example/repo', env), /expired/); expires = () => new Date(Date.now() + 3600000).toISOString();
  const child = spawn(process.execPath, [new URL('./github-credentials.mjs', import.meta.url).pathname, '--git', 'get'], { env, stdio: ['pipe', 'pipe', 'pipe'] });
  let stdout = '', stderr = ''; child.stdout.on('data', data => stdout += data); child.stderr.on('data', data => stderr += data);
  child.stdin.end('protocol=https\nhost=github.com\npath=example/repo.git\n\n');
  const code = await new Promise(resolve => child.on('exit', resolve));
  assert.equal(code, 0, stderr); assert.equal(stdout, 'username=x-access-token\npassword=short-lived-bot-token\n\n'); assert.equal(stderr, '');
 } finally { await new Promise(resolve => server.close(resolve)); rmSync(dir, { recursive: true, force: true }); }
});
