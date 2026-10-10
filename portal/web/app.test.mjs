import { test } from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';

// Every static element dereferenced through the cache must be registered AND
// exist in the embedded page. This catches post-login render failures like the
// missing web-search input cache entries without a browser dependency.
test('the admin panel refreshes itself while it is open', () => {
  const js = readFileSync(new URL('./app.js', import.meta.url), 'utf8');
  // A panel that only refreshed on user action would show a stale countdown and
  // stale session state; the controller rewrites observations continuously.
  assert.match(js, /var ADMIN_REFRESH_MS = \d+;/);
  assert.match(js, /function startAdminRefresh\(\)/);
  assert.match(js, /function stopAdminRefresh\(\)/);
  assert.match(js, /state\.adminRefresh = setInterval/);
  assert.match(js, /stopAdminRefresh\(\);\n    clearAdminAccess\(\);/);
  assert.match(js, /if \(admin\) \{\n      startAdminRefresh\(\);/);
});

test('all cached UI elements are registered and present', () => {
  const js = readFileSync(new URL('./app.js', import.meta.url), 'utf8');
  const html = readFileSync(new URL('./index.html', import.meta.url), 'utf8');
  const list = js.match(/var elementIds = \[([\s\S]*?)\];/)[1];
  const ids = new Set([...list.matchAll(/'([^']+)'/g)].map(m => m[1]));
  const references = [...js.matchAll(/elements(?:\['([^']+)'\]|\.([A-Za-z]\w*))/g)];
  for (const [, bracket, dot] of references) {
    const id = bracket || dot;
    assert.ok(ids.has(id), `cache entry missing: ${id}`);
  }
  for (const id of ids) assert.ok(html.includes(`id="${id}"`), `HTML element missing: ${id}`);
});

// --- Cluster administration panel behavioural tests -------------------------
//
// The SPA is one dependency-free IIFE that reaches for a handful of globals.
// These tests evaluate the real app.js against a minimal in-memory DOM, a
// scripted fetch router, and a controllable clock so the admin panel's
// render, capability gating, countdown and isolation contracts can be checked
// without a browser or a new dependency.

const BASE_NOW = 1_700_000_000_000; // fixed epoch for deterministic timestamps

function isoAt(offsetMs) {
  return new Date(BASE_NOW + offsetMs).toISOString();
}

function makeHarness(handlers) {
  const elements = new Map();
  const calls = [];
  const opened = [];
  const intervals = new Map();
  let intervalId = 0;
  let now = BASE_NOW;

  // Seed each element's initial hidden flag from the shipped markup so the
  // harness matches the real page's default (tab/panel hidden, etc.).
  const html = readFileSync(new URL('./index.html', import.meta.url), 'utf8');
  const hiddenByDefault = new Set();
  for (const match of html.matchAll(/<[^>]*>/g)) {
    const tag = match[0];
    const id = tag.match(/\bid="([^"]+)"/);
    if (id && /\shidden(\s|>)/.test(tag)) hiddenByDefault.add(id[1]);
  }

  function makeElement(id) {
    const element = {
      id,
      hidden: hiddenByDefault.has(id),
      disabled: false,
      value: '',
      textContent: '',
      className: '',
      dataset: {},
      options: [],
      children: [],
      _attrs: {},
      _listeners: {},
      setAttribute(key, value) {
        element._attrs[key] = String(value);
        if (key === 'hidden') element.hidden = true;
      },
      getAttribute(key) {
        return Object.prototype.hasOwnProperty.call(element._attrs, key) ? element._attrs[key] : null;
      },
      removeAttribute(key) {
        delete element._attrs[key];
        if (key === 'hidden') element.hidden = false;
      },
      addEventListener(type, fn) {
        (element._listeners[type] = element._listeners[type] || []).push(fn);
      },
      appendChild(child) {
        element.children.push(child);
        return child;
      },
      replaceChildren() {
        element.children = [];
      },
      querySelectorAll() {
        return [];
      },
      focus() {},
      getContext() {
        return { clearRect() {}, fillRect() {}, fillStyle: '' };
      },
      dispatch(type) {
        (element._listeners[type] || []).forEach(fn => fn({ preventDefault() {} }));
      },
    };
    return element;
  }

  function getElement(id) {
    if (!elements.has(id)) elements.set(id, makeElement(id));
    return elements.get(id);
  }

  const document = {
    _domReady: null,
    getElementById: getElement,
    createElement: () => makeElement(''),
    addEventListener(type, fn) {
      if (type === 'DOMContentLoaded') document._domReady = fn;
    },
    fullscreenElement: null,
    exitFullscreen() {},
  };

  const window = {
    confirm: () => true,
    open(url, target, features) {
      opened.push([url, target, features]);
      return null;
    },
  };

  const navigator = { clipboard: { writeText: async () => {} } };

  function mkResponse(status, payload) {
    return {
      ok: status >= 200 && status < 300,
      status,
      json: async () => payload,
    };
  }

  async function fetchStub(path, options) {
    const method = (options && options.method) || 'GET';
    calls.push({ method, path, body: options && options.body ? JSON.parse(options.body) : null });
    const handler = handlers[method + ' ' + path];
    if (!handler) return mkResponse(404, { error: 'not found' });
    const result = handler();
    if (result instanceof Error) throw result;
    return mkResponse(result.status || 200, result.payload);
  }

  function setIntervalStub(fn) {
    const id = ++intervalId;
    intervals.set(id, fn);
    return id;
  }
  function clearIntervalStub(id) {
    intervals.delete(id);
  }
  function tickIntervals() {
    [...intervals.values()].forEach(fn => fn());
  }

  const FakeDate = {
    now: () => now,
    parse: Date.parse,
  };

  const source = readFileSync(new URL('./app.js', import.meta.url), 'utf8');
  const factory = new Function(
    'document', 'window', 'navigator', 'fetch', 'setInterval', 'clearInterval', 'qrcodegen', 'Date',
    source
  );
  factory(document, window, navigator, fetchStub, setIntervalStub, clearIntervalStub, { QrCode: {} }, FakeDate);

  return {
    elements,
    calls,
    opened,
    tickIntervals,
    advance(ms) { now += ms; },
    start() { document._domReady(); },
  };
}

async function waitFor(predicate, label) {
  const deadline = Date.now() + 2000;
  while (Date.now() < deadline) {
    if (predicate()) return;
    await new Promise(resolve => setTimeout(resolve, 5));
  }
  throw new Error('timed out waiting for ' + label);
}

const AUTH = { 'GET /auth/session': () => ({ payload: { mode: 'github', authenticated: true, login: 'alice' } }) };
const CONFIG = { 'GET /api/config': () => ({ payload: { resourceVersion: '1', allowedApiKeys: [], apiKeys: {}, authorizedKeys: '', knownHosts: '', pocketUrl: 'https://pocket.example', ownerLoginUrl: '', webSearch: null } }) };
const DEPLOY = { 'GET /api/status': () => ({ payload: { running: true, desiredReplicas: 1, readyReplicas: 1 } }) };
const GITHUB = { 'GET /api/github/status': () => ({ payload: { enabled: false, repositories: [], canManage: false } }) };

const IDLE_ADMIN = {
  enabled: true, namespace: 'admin-pocket', deployment: 'pi-pocket-admin', pocketUrl: 'https://admin.example',
  terminalUrl: 'https://admin-terminal.example', isOperator: true, canActivate: true, canRevoke: false, canOpen: false,
  resourceVersion: '9', phase: 'Idle', observedAt: isoAt(-1000), fresh: true, failureCode: '',
  needsRecentLogin: false, session: null, workspace: { ready: false, podUID: '', observedAt: isoAt(-1000) }, grant: { observed: false },
};

function activeAdmin(overrides) {
  return Object.assign({
    enabled: true, namespace: 'admin-pocket', deployment: 'pi-pocket-admin', pocketUrl: 'https://admin.example',
    terminalUrl: 'https://admin-terminal.example', isOperator: true, canActivate: false, canRevoke: true, canOpen: true,
    resourceVersion: '10', phase: 'Active', observedAt: isoAt(-2000), fresh: true, failureCode: '',
    needsRecentLogin: false,
    session: { sessionID: 's1', ownerUserID: 42, owner: 'alice', reason: 'Investigate upgrade', approvedAt: isoAt(-30000), expiresAt: isoAt(300000), secondsRemaining: 300 },
    workspace: { ready: true, podUID: 'pod-1', observedAt: isoAt(-2000) }, grant: { observed: true },
  }, overrides || {});
}

function baseHandlers(adminStatus) {
  return Object.assign({}, AUTH, CONFIG, DEPLOY, GITHUB, { 'GET /api/admin/status': () => ({ payload: adminStatus }) });
}

test('admin panel stays hidden and the tab is not shown when the feature is disabled', async () => {
  const harness = makeHarness(baseHandlers({ enabled: false }));
  harness.start();
  await waitFor(() => harness.elements.get('github-session').hidden === false, 'session render');
  assert.equal(harness.elements.get('tab-admin').hidden, true);
  assert.equal(harness.elements.get('panel-admin').hidden, true);
  assert.equal(harness.elements.get('app').hidden, false, 'normal panel still visible');
});

test('disabled admin status never breaks the normal workspace panel when the fetch fails', async () => {
  const handlers = Object.assign(baseHandlers({ enabled: false }), {
    'GET /api/admin/status': () => new Error('boom'),
  });
  const harness = makeHarness(handlers);
  harness.start();
  await waitFor(() => harness.elements.get('github-session').hidden === false, 'session render');
  assert.equal(harness.elements.get('app').hidden, false);
  assert.equal(harness.elements.get('github-session').hidden, false);
  assert.equal(harness.elements.get('tab-admin').hidden, true);
});

test('active view renders owner, grant, readiness, freshness and capability-gated controls', async () => {
  const harness = makeHarness(baseHandlers(activeAdmin()));
  harness.start();
  await waitFor(() => harness.elements.get('admin-active').hidden === false, 'active view');
  const el = id => harness.elements.get(id);
  assert.equal(el('tab-admin').hidden, false);
  assert.equal(el('admin-idle').hidden, true);
  assert.equal(el('admin-owner').textContent, 'alice');
  assert.equal(el('admin-reason-display').textContent, 'Investigate upgrade');
  assert.equal(el('admin-countdown').textContent, '05:00');
  assert.equal(el('admin-grant').textContent, 'Grant observed');
  assert.equal(el('admin-workspace-ready').textContent, 'Ready');
  assert.match(el('admin-freshness').textContent, /^observed \d+s ago$/);
  assert.equal(el('admin-stale-warning').hidden, true);
  assert.equal(el('admin-revoke').hidden, false);
  assert.equal(el('admin-open-workspace').hidden, false);
  assert.equal(el('admin-open-terminal').hidden, false);
  assert.equal(el('admin-access-button').hidden, false);
  harness.advance(60000);
  harness.tickIntervals();
  assert.equal(el('admin-countdown').textContent, '04:00');
  harness.advance(10 * 60000);
  harness.tickIntervals();
  assert.equal(el('admin-countdown').textContent, '00:00');
  assert.equal(el('admin-expired-warning').hidden, false);
});

test('CleanupRequired and unknown phases render the failure view, never a green off state', async () => {
  const cleanup = makeHarness(baseHandlers(activeAdmin({ phase: 'CleanupRequired', failureCode: 'GrantRemovalFailed' })));
  cleanup.start();
  await waitFor(() => cleanup.elements.get('admin-failure').hidden === false, 'cleanup failure view');
  assert.equal(cleanup.elements.get('admin-active').hidden, true);
  assert.equal(cleanup.elements.get('admin-failure-heading').textContent, 'Cleanup incomplete');
  assert.equal(cleanup.elements.get('admin-failure-code').hidden, false);
  assert.equal(cleanup.elements.get('admin-failure-code').textContent, 'failure code: GrantRemovalFailed');
  assert.equal(cleanup.elements.get('admin-access').hidden, true);

  const unknown = makeHarness(baseHandlers(activeAdmin({ phase: '', session: null })));
  unknown.start();
  await waitFor(() => unknown.elements.get('admin-failure').hidden === false, 'unknown failure view');
  assert.equal(unknown.elements.get('admin-failure-heading').textContent, 'Access state unknown');
});

test('stale observation warns and clears any admin access link', async () => {
  let adminCall = 0;
  const handlers = baseHandlers(activeAdmin());
  handlers['GET /api/admin/status'] = () => {
    adminCall += 1;
    return { payload: adminCall === 1 ? activeAdmin() : activeAdmin({ fresh: false }) };
  };
  handlers['POST /api/admin/access'] = () => ({ payload: { url: 'https://admin.example/?token=secret' } });
  const harness = makeHarness(handlers);
  harness.start();
  await waitFor(() => harness.elements.get('admin-active').hidden === false, 'active view');

  harness.elements.get('admin-access-button').dispatch('click');
  await waitFor(() => harness.elements.get('admin-access-url').value !== '', 'access url');
  assert.equal(harness.elements.get('admin-access').hidden, false);

  // A refresh that observes the controller as stale must clear the credential
  // and warn, even though it was valid a moment before.
  harness.elements.get('tab-admin').dispatch('click');
  await waitFor(() => harness.elements.get('admin-stale-warning').hidden === false, 'stale warning');
  assert.equal(harness.elements.get('admin-stale-warning').hidden, false);
  assert.equal(harness.elements.get('admin-access').hidden, true, 'stale state clears the link');
  assert.equal(harness.elements.get('admin-access-url').value, '');
});

test('access link is fetched on demand, shown, opened with noopener, and cleared on revoke', async () => {
  let adminCall = 0;
  const handlers = baseHandlers(activeAdmin());
  handlers['POST /api/admin/access'] = () => ({ payload: { url: 'https://admin.example/?token=secret' } });
  handlers['GET /api/admin/status'] = () => {
    adminCall += 1;
    return { payload: adminCall === 1 ? activeAdmin() : activeAdmin({ phase: 'Revoking', session: null, canRevoke: false, canOpen: false }) };
  };
  handlers['POST /api/admin/revoke'] = () => ({ status: 202, payload: { phase: 'Revoking' } });
  const harness = makeHarness(handlers);
  harness.start();
  await waitFor(() => harness.elements.get('admin-active').hidden === false, 'active view');

  harness.elements.get('admin-access-button').dispatch('click');
  await waitFor(() => harness.elements.get('admin-access-url').value !== '', 'access url');
  const url = harness.elements.get('admin-access-url').value;
  assert.equal(url, 'https://admin.example/?token=secret');
  assert.equal(harness.elements.get('admin-access').hidden, false);

  harness.elements.get('admin-access-open').dispatch('click');
  assert.deepEqual(harness.opened.at(-1), [url, '_blank', 'noopener']);

  harness.elements.get('admin-open-workspace').dispatch('click');
  assert.deepEqual(harness.opened.at(-1), ['https://admin.example', '_blank', 'noopener']);

  harness.elements.get('admin-revoke').dispatch('click');
  await waitFor(() => harness.calls.filter(c => c.path === '/api/admin/status').length >= 2, 'post-revoke refresh');
  assert.equal(harness.elements.get('admin-access').hidden, true, 'revoke clears the link');
  assert.equal(harness.elements.get('admin-access-url').value, '');
});

test('Activate is enabled only for an authorized operator with a reason and exact confirmation', async () => {
  const harness = makeHarness(baseHandlers(IDLE_ADMIN));
  harness.start();
  await waitFor(() => harness.elements.get('tab-admin').hidden === false, 'idle view');
  const el = id => harness.elements.get(id);
  assert.equal(el('admin-activate').disabled, true);
  el('admin-reason').value = 'audit';
  el('admin-reason').dispatch('input');
  assert.equal(el('admin-activate').disabled, true, 'reason alone is not enough');
  el('admin-confirmation').value = 'cluster-admin ';
  el('admin-confirmation').dispatch('input');
  assert.equal(el('admin-activate').disabled, true, 'confirmation must match exactly');
  el('admin-confirmation').value = 'cluster-admin';
  el('admin-confirmation').dispatch('input');
  assert.equal(el('admin-activate').disabled, false);

  el('admin-activate').dispatch('click');
  await waitFor(() => harness.calls.some(c => c.path === '/api/admin/activate'), 'activate request');
  const activate = harness.calls.find(c => c.path === '/api/admin/activate');
  assert.deepEqual(activate.body, { resourceVersion: '9', reason: 'audit', durationSeconds: 900, confirmation: 'cluster-admin' });
});
