/* pi-pocket portal SPA.
 *
 * The bearer token lives only in state.token below: it is never written to
 * the URL, cookies, or any browser storage. All dynamic text is rendered with
 * textContent so server data can never become HTML.
 */
(function () {
  'use strict';

  var state = {
    token: '',
    authMode: 'token',
    config: null,
    running: null,
    authorizedKeys: '',
    knownHosts: '',
    webSearchProvider: '',
    webSearchModel: '',
    ownerLoginUrl: '',
    repositoryPolicy: null,
    busy: false
  };

  var elements = {};

  var elementIds = [
    'message', 'unlock', 'token-form', 'token', 'unlock-button', 'app',
    'github-login', 'github-session', 'github-user', 'github-logout', 'github-integration', 'github-repositories', 'token-help',
    'github-policy-manager', 'github-load-repositories', 'github-repository-options', 'github-save-repositories',
    'tab-workspace', 'tab-configure', 'panel-workspace', 'panel-configure',
    'owner-card', 'owner-missing', 'owner-link', 'owner-open', 'owner-copy', 'owner-qr',
    'agent-frame', 'frame-wrap', 'workspace-fullscreen', 'workspace-external', 'workspace-terminal', 'workspace-reload',
    'status-state', 'status-ready', 'status-restarted',
    'action-start', 'action-stop', 'action-restart', 'action-refresh',
    'api-key-rows', 'authorized-keys', 'known-hosts', 'web-search-provider', 'web-search-model',
    'save-config', 'reload-config', 'pocket-link'
  ];

  function byId(id) {
    return document.getElementById(id);
  }

  function cacheElements() {
    elementIds.forEach(function (id) {
      elements[id] = byId(id);
    });
  }

  function setMessage(text, kind) {
    elements.message.textContent = text || '';
    elements.message.className = kind ? 'message ' + kind : 'message';
  }

  function setBusy(busy) {
    state.busy = busy;
    updateControls();
  }

  function updateControls() {
    var busy = state.busy;
    var running = state.running === true;
    var hasConfig = state.config !== null;
    elements['action-start'].disabled = busy || running;
    elements['action-stop'].disabled = busy || !running;
    elements['action-restart'].disabled = busy;
    elements['action-refresh'].disabled = busy;
    elements['save-config'].disabled = busy || !hasConfig;
    elements['reload-config'].disabled = busy || !hasConfig;
    elements['unlock-button'].disabled = busy || state.token !== '';
    elements.app.setAttribute('aria-busy', busy ? 'true' : 'false');
  }

  function httpError(message, status) {
    var error = new Error(message);
    error.status = status;
    return error;
  }

  async function api(method, path, body) {
    var options = {
      method: method,
      headers: { Accept: 'application/json' },
      credentials: state.authMode === 'github' ? 'same-origin' : 'omit',
      cache: 'no-store'
    };
    if (state.token !== '') {
      options.headers.Authorization = 'Bearer ' + state.token;
    }
    if (body !== undefined) {
      options.headers['Content-Type'] = 'application/json';
      options.body = JSON.stringify(body);
    }
    var response = await fetch(path, options);
    var payload = null;
    try {
      payload = await response.json();
    } catch (err) {
      payload = null;
    }
    if (!response.ok) {
      if (response.status === 401 || response.status === 503) {
        lock();
      }
      var message = (payload && typeof payload.error === 'string')
        ? payload.error
        : 'Request failed with status ' + response.status + '.';
      throw httpError(message, response.status);
    }
    return payload;
  }

  function lock() {
    state.token = '';
    state.config = null;
    state.running = null;
    state.ownerLoginUrl = '';
    elements['owner-link'].removeAttribute('href');
    var qr = elements['owner-qr'];
    qr.getContext('2d').clearRect(0, 0, qr.width, qr.height);
    elements['github-session'].hidden = true;
    elements.token.value = '';
    elements.app.hidden = true;
    elements.unlock.hidden = false;
    elements['api-key-rows'].replaceChildren();
    elements['agent-frame'].removeAttribute('src');
    updateControls();
  }

  function selectTab(name) {
    var workspace = name === 'workspace';
    elements['tab-workspace'].setAttribute('aria-selected', workspace ? 'true' : 'false');
    elements['tab-configure'].setAttribute('aria-selected', workspace ? 'false' : 'true');
    elements['panel-workspace'].hidden = !workspace;
    elements['panel-workspace'].setAttribute('aria-hidden', workspace ? 'false' : 'true');
    elements['panel-configure'].hidden = workspace;
    elements['panel-configure'].setAttribute('aria-hidden', workspace ? 'true' : 'false');
  }

  // Draw the owner sign-in link as a QR code. qrcodegen is a vendored,
  // dependency-free encoder (MIT, Project Nayuki) loaded via script tag.
  function drawQR(url) {
    var canvas = elements['owner-qr'];
    var context = canvas.getContext('2d');
    context.fillStyle = '#ffffff';
    context.fillRect(0, 0, canvas.width, canvas.height);
    var qr = qrcodegen.QrCode.encodeText(url, qrcodegen.QrCode.Ecc.MEDIUM);
    var border = 2;
    var scale = Math.floor(canvas.width / (qr.size + border * 2));
    var offset = Math.floor((canvas.width - (qr.size + border * 2) * scale) / 2);
    context.fillStyle = '#000000';
    for (var y = 0; y < qr.size; y++) {
      for (var x = 0; x < qr.size; x++) {
        if (qr.getModule(x, y)) {
          context.fillRect(offset + (x + border) * scale, offset + (y + border) * scale, scale, scale);
        }
      }
    }
  }

  function renderOwner(url) {
    state.ownerLoginUrl = (typeof url === 'string') ? url : '';
    var has = state.ownerLoginUrl !== '';
    elements['owner-card'].hidden = !has;
    elements['owner-missing'].hidden = has;
    if (!has) {
      return;
    }
    var link = elements['owner-link'];
    link.href = state.ownerLoginUrl;
    link.textContent = 'Open in new tab';
    try {
      drawQR(state.ownerLoginUrl);
    } catch (err) {
      var context = elements['owner-qr'].getContext('2d');
      context.fillStyle = '#ffffff';
      context.fillRect(0, 0, elements['owner-qr'].width, elements['owner-qr'].height);
    }
  }

  function frameURL() {
    if (state.config !== null && typeof state.config.pocketUrl === 'string') {
      return state.config.pocketUrl;
    }
    return '';
  }

  function loadFrame(url) {
    var frame = elements['agent-frame'];
    var target = (typeof url === 'string' && url !== '') ? url : frameURL();
    if (target === '') {
      setMessage('The agent URL is not configured.', 'error');
      return;
    }
    // Assigning src is permitted across origins; inspecting the embedded
    // window's location/reload method is not.
    frame.setAttribute('src', target);
  }

  // The terminal daemon authenticates with the same owner token that signs
  // into pi-pocket; the token travels in the link, never in page storage.
  function terminalURL() {
    if (state.config === null || typeof state.config.terminalUrl !== 'string' || state.config.terminalUrl === '') {
      return '';
    }
    if (typeof state.ownerLoginUrl !== 'string' || state.ownerLoginUrl === '') {
      return '';
    }
    var token = '';
    try {
      token = new URL(state.ownerLoginUrl).searchParams.get('token') || '';
    } catch (err) {
      token = '';
    }
    if (token === '') {
      return '';
    }
    return state.config.terminalUrl.replace(/\/$/, '') + '/?token=' + encodeURIComponent(token);
  }

  function openTerminal() {
    var target = terminalURL();
    if (target === '') {
      setMessage('The terminal is not available yet: it needs the terminal URL and the synced owner sign-in link.', 'error');
      return;
    }
    window.open(target, '_blank', 'noopener');
  }


  async function unlock(event) {
    event.preventDefault();
    if (state.busy) {
      return;
    }
    var token = elements.token.value.trim();
    if (token === '') {
      setMessage('Enter the portal token.', 'error');
      return;
    }
    state.token = token;
    setBusy(true);
    setMessage('Loading configuration...', '');
    try {
      await refreshConfig();
      await refreshStatus();
      elements.token.value = '';
      elements.unlock.hidden = true;
      elements.app.hidden = false;
      selectTab('workspace');
      loadFrame();
      setMessage('', '');
      elements['action-refresh'].focus();
    } catch (err) {
      lock();
      setMessage(err.message, 'error');
    } finally {
      setBusy(false);
    }
  }

  async function refreshConfig() {
    var config = await api('GET', '/api/config');
    state.config = config;
    renderConfig(config);
  }

  async function refreshStatus() {
    var status = await api('GET', '/api/status');
    renderStatus(status);
  }

  function renderConfig(config) {
    renderAPIKeys(config);
    renderOwner(config.ownerLoginUrl);
    var authorizedKeys = typeof config.authorizedKeys === 'string' ? config.authorizedKeys : '';
    var knownHosts = typeof config.knownHosts === 'string' ? config.knownHosts : '';
    elements['authorized-keys'].value = authorizedKeys;
    elements['known-hosts'].value = knownHosts;
    state.authorizedKeys = authorizedKeys;
    state.knownHosts = knownHosts;

    var webSearch = (config.webSearch && typeof config.webSearch === 'object') ? config.webSearch : {};
    var webSearchProvider = typeof webSearch.provider === 'string' ? webSearch.provider : '';
    var webSearchModel = typeof webSearch.model === 'string' ? webSearch.model : '';
    elements['web-search-provider'].value = webSearchProvider;
    elements['web-search-model'].value = webSearchModel;
    state.webSearchProvider = webSearchProvider;
    state.webSearchModel = webSearchModel;

    var link = elements['pocket-link'];
    if (typeof config.pocketUrl === 'string' && config.pocketUrl !== '') {
      link.href = config.pocketUrl;
      link.textContent = config.pocketUrl;
    } else {
      link.removeAttribute('href');
      link.textContent = 'not configured';
    }
  }

  function renderAPIKeys(config) {
    var tbody = elements['api-key-rows'];
    tbody.replaceChildren();
    var names = Array.isArray(config.allowedApiKeys) ? config.allowedApiKeys : [];
    var statuses = (config.apiKeys && typeof config.apiKeys === 'object') ? config.apiKeys : {};

    names.forEach(function (name) {
      if (typeof name !== 'string' || name === '') {
        return;
      }
      var isSet = statuses[name] === true;

      var row = document.createElement('tr');

      var nameCell = document.createElement('th');
      nameCell.scope = 'row';
      nameCell.textContent = name;

      var statusCell = document.createElement('td');
      var badge = document.createElement('span');
      badge.textContent = isSet ? 'set' : 'not set';
      badge.className = isSet ? 'badge badge-set' : 'badge badge-unset';
      statusCell.appendChild(badge);

      var valueCell = document.createElement('td');
      var inputId = 'key-value-' + name;
      var input = document.createElement('input');
      input.type = 'password';
      input.id = inputId;
      input.className = 'key-input';
      input.setAttribute('autocomplete', 'new-password');
      input.setAttribute('spellcheck', 'false');
      input.placeholder = isSet ? 'Replace value' : 'Add value';
      input.dataset.keyName = name;
      var inputLabel = document.createElement('label');
      inputLabel.className = 'sr-only';
      inputLabel.htmlFor = inputId;
      inputLabel.textContent = 'New value for ' + name;
      valueCell.appendChild(input);
      valueCell.appendChild(inputLabel);

      var removeCell = document.createElement('td');
      var removeId = 'key-remove-' + name;
      var remove = document.createElement('input');
      remove.type = 'checkbox';
      remove.id = removeId;
      remove.dataset.removeKey = name;
      remove.disabled = !isSet;
      var removeLabel = document.createElement('label');
      removeLabel.className = 'sr-only';
      removeLabel.htmlFor = removeId;
      removeLabel.textContent = 'Remove ' + name;
      removeCell.appendChild(remove);
      removeCell.appendChild(removeLabel);

      row.appendChild(nameCell);
      row.appendChild(statusCell);
      row.appendChild(valueCell);
      row.appendChild(removeCell);
      tbody.appendChild(row);
    });
  }

  function renderStatus(status) {
    var running = status.running === true;
    var desired = typeof status.desiredReplicas === 'number' ? status.desiredReplicas : 0;
    var ready = typeof status.readyReplicas === 'number' ? status.readyReplicas : 0;

    state.running = running;
    elements['status-state'].textContent = running
      ? (desired > 0 && ready >= desired ? 'Running' : 'Starting')
      : 'Stopped';
    elements['status-ready'].textContent = ready + ' of ' + desired;
    elements['status-restarted'].textContent =
      (typeof status.restartedAt === 'string' && status.restartedAt !== '') ? status.restartedAt : 'Never';
    updateControls();
  }

  function collectUpdates() {
    var update = { set: {}, remove: [] };
    elements['api-key-rows'].querySelectorAll('input[type="password"]').forEach(function (input) {
      var name = input.dataset.keyName;
      var value = input.value;
      if (value !== '') {
        if (value !== value.trim()) {
          throw new Error(name + ': remove leading or trailing whitespace.');
        }
        update.set[name] = value;
      }
    });
    elements['api-key-rows'].querySelectorAll('input[type="checkbox"]').forEach(function (input) {
      if (input.checked && input.dataset.removeKey) {
        update.remove.push(input.dataset.removeKey);
      }
    });
    update.remove.forEach(function (name) {
      if (Object.prototype.hasOwnProperty.call(update.set, name)) {
        throw new Error(name + ': cannot set and remove the same key.');
      }
    });
    return update;
  }

  function clearAPIKeyInputs() {
    elements['api-key-rows'].querySelectorAll('input[type="password"]').forEach(function (input) {
      input.value = '';
    });
    elements['api-key-rows'].querySelectorAll('input[type="checkbox"]').forEach(function (input) {
      input.checked = false;
    });
  }

  function hasUnsavedEdits() {
    if (state.config === null) {
      return false;
    }
    if (elements['authorized-keys'].value !== state.authorizedKeys) {
      return true;
    }
    if (elements['known-hosts'].value !== state.knownHosts) {
      return true;
    }
    var dirty = false;
    elements['api-key-rows'].querySelectorAll('input[type="password"]').forEach(function (input) {
      if (input.value !== '') {
        dirty = true;
      }
    });
    elements['api-key-rows'].querySelectorAll('input[type="checkbox"]').forEach(function (input) {
      if (input.checked) {
        dirty = true;
      }
    });
    return dirty;
  }

  async function saveConfig() {
    if (state.busy || state.config === null) {
      return;
    }
    var update;
    try {
      update = collectUpdates();
    } catch (err) {
      setMessage(err.message, 'error');
      return;
    }

    var body = { resourceVersion: state.config.resourceVersion };
    var hasKeyChanges = Object.keys(update.set).length > 0 || update.remove.length > 0;
    if (hasKeyChanges) {
      body.apiKeys = update;
    }
    var authorizedKeys = elements['authorized-keys'].value;
    var knownHosts = elements['known-hosts'].value;
    if (authorizedKeys !== state.authorizedKeys) {
      body.authorizedKeys = authorizedKeys;
    }
    if (knownHosts !== state.knownHosts) {
      body.knownHosts = knownHosts;
    }
    var webSearchProvider = elements['web-search-provider'].value.trim();
    var webSearchModel = elements['web-search-model'].value.trim();
    if (webSearchProvider !== state.webSearchProvider || webSearchModel !== state.webSearchModel) {
      // Both or neither: the server rejects half a choice, and an empty pair clears it.
      body.webSearch = { provider: webSearchProvider, model: webSearchModel };
    }
    if (!hasKeyChanges && body.authorizedKeys === undefined && body.knownHosts === undefined && body.webSearch === undefined) {
      setMessage('Nothing to save.', '');
      return;
    }

    setBusy(true);
    try {
      var config = await api('POST', '/api/config', body);
      state.config = config;
      renderConfig(config);
      clearAPIKeyInputs();
      setMessage('Configuration saved. Restart the agent to apply it immediately; projected secret updates can take up to a minute.', 'ok');
    } catch (err) {
      if (err.status === 409) {
        setMessage(err.message + ' Use "Reload configuration" to fetch the latest version, then re-apply your changes.', 'error');
      } else {
        setMessage(err.message, 'error');
      }
    } finally {
      setBusy(false);
    }
  }

  async function reloadConfig() {
    if (state.busy || state.config === null) {
      return;
    }
    if (hasUnsavedEdits() && !window.confirm('Reload from the cluster and discard unsaved changes?')) {
      return;
    }
    setBusy(true);
    try {
      await refreshConfig();
      setMessage('Configuration reloaded.', 'ok');
    } catch (err) {
      setMessage(err.message, 'error');
    } finally {
      setBusy(false);
    }
  }

  async function deploymentAction(action) {
    if (state.busy) {
      return;
    }
    if (action === 'stop' && !window.confirm('Stop the pi-pocket agent?')) {
      return;
    }
    if (action === 'restart' && !window.confirm('Restart the pi-pocket agent now?')) {
      return;
    }
    setBusy(true);
    try {
      var status = await api('POST', '/api/deployment/' + action);
      renderStatus(status);
      setMessage(action === 'stop' ? 'Stop requested.' : action === 'start' ? 'Start requested.' : 'Restart requested.', 'ok');
      await refreshStatus();
    } catch (err) {
      setMessage(err.message, 'error');
    } finally {
      setBusy(false);
    }
  }

  async function copyOwnerLink() {
    if (state.ownerLoginUrl === '') {
      return;
    }
    try {
      await navigator.clipboard.writeText(state.ownerLoginUrl);
      setMessage('Owner link copied. It is the owner key: keep it private.', 'ok');
    } catch (err) {
      setMessage('Copy failed; use the link in a new tab instead.', 'error');
    }
  }

  function toggleFullscreen() {
    var wrap = elements['frame-wrap'];
    if (document.fullscreenElement) {
      document.exitFullscreen();
      return;
    }
    if (typeof wrap.requestFullscreen === 'function') {
      var result = wrap.requestFullscreen();
      if (result && typeof result.catch === 'function') {
        result.catch(function () {
          setMessage('Full screen was refused by the browser.', 'error');
        });
      }
    } else {
      setMessage('Full screen is not supported here; use the new-tab view.', 'error');
    }
  }

  function renderRepositoryPolicy(integration) {
    state.repositoryPolicy = integration;
    elements['github-integration'].hidden = !integration.enabled;
    elements['github-repositories'].textContent = 'Allowed repositories: ' + ((integration.repositories || []).join(', ') || '(none — new credentials denied)');
    elements['github-policy-manager'].hidden = !integration.canManage;
  }

  async function loadRepositoryCatalog() {
    elements['github-save-repositories'].disabled = true;
    elements['github-load-repositories'].disabled = true;
    try {
      renderRepositoryPolicy(await api('GET', '/api/github/status'));
      var catalog = await api('GET', '/api/github/repositories');
      var selected = new Set(state.repositoryPolicy.repositories || []);
      // Keep stale selections visible so an owner can remove them even if the
      // App no longer exposes a previously allowed repository.
      var repos = Array.from(new Set((catalog.repositories || []).concat(Array.from(selected)))).sort();
      var container = elements['github-repository-options'];
      container.textContent = '';
      repos.forEach(function (repo) {
        var label = document.createElement('label');
        var checkbox = document.createElement('input');
        checkbox.type = 'checkbox';
        checkbox.value = repo;
        checkbox.checked = selected.has(repo);
        label.appendChild(checkbox);
        label.appendChild(document.createTextNode(' ' + repo));
        container.appendChild(label);
        container.appendChild(document.createElement('br'));
      });
      elements['github-save-repositories'].disabled = false;
      setMessage('Select repositories and save. An empty selection denies all new credentials.', '');
    } catch (err) { setMessage(err.message, 'error'); }
    finally { elements['github-load-repositories'].disabled = false; }
  }

  async function saveRepositoryPolicy() {
    if (!state.repositoryPolicy) return;
    elements['github-save-repositories'].disabled = true;
    elements['github-load-repositories'].disabled = true;
    try {
      var repos = Array.from(elements['github-repository-options'].querySelectorAll('input:checked')).map(function (input) { return input.value; });
      await api('POST', '/api/github/repositories', { resourceVersion: state.repositoryPolicy.resourceVersion, repositories: repos });
      renderRepositoryPolicy(await api('GET', '/api/github/status'));
      setMessage('Repository policy saved. New Git/gh credentials use it immediately; existing tokens may remain valid up to one hour.', 'success');
      elements['github-save-repositories'].disabled = false;
    } catch (err) { setMessage(err.message + ' Reload the repository catalog before retrying.', 'error'); }
    finally { elements['github-load-repositories'].disabled = false; }
  }

  async function initAuth() {
    elements['token-form'].hidden = true;
    try {
      var response = await fetch('/auth/session', { credentials: 'same-origin', cache: 'no-store' });
      if (!response.ok) throw new Error('Authentication unavailable');
      var session = await response.json();
      state.authMode = session.mode;
      elements['github-login'].hidden = session.mode !== 'github';
      elements['token-form'].hidden = session.mode === 'github';
      elements['token-help'].hidden = session.mode === 'github';
      if (session.mode === 'github' && session.authenticated) {
        try {
          await refreshConfig();
          await refreshStatus();
          var integration = await api('GET', '/api/github/status');
          renderRepositoryPolicy(integration);
          elements['github-user'].textContent = session.login;
          elements['github-session'].hidden = false;
          elements.unlock.hidden = true;
          elements.app.hidden = false;
          selectTab('workspace');
          loadFrame();
        } catch (err) {
          // Login succeeded; a config/render failure is not an OAuth failure.
          setMessage('Signed in, but workspace loading failed. Refresh to retry.', 'error');
        }
      }
    } catch (err) {
      setMessage('Authentication unavailable. Refresh to retry.', 'error');
    }
  }

  async function logout() {
    try {
      var response = await fetch('/auth/logout', { method: 'POST', credentials: 'same-origin', cache: 'no-store' });
      if (!response.ok) throw new Error('Logout failed');
      lock();
    } catch (err) { setMessage('Sign out failed. Refresh to retry.', 'error'); }
  }

  function init() {
    cacheElements();
    elements['github-logout'].addEventListener('click', logout);
    elements['github-load-repositories'].addEventListener('click', loadRepositoryCatalog);
    elements['github-save-repositories'].addEventListener('click', saveRepositoryPolicy);
    elements['token-form'].addEventListener('submit', unlock);
    elements['tab-workspace'].addEventListener('click', function () { selectTab('workspace'); });
    elements['tab-configure'].addEventListener('click', function () { selectTab('configure'); });
    elements['owner-open'].addEventListener('click', function () { loadFrame(state.ownerLoginUrl); });
    elements['owner-copy'].addEventListener('click', copyOwnerLink);
    elements['workspace-fullscreen'].addEventListener('click', toggleFullscreen);
    elements['workspace-external'].addEventListener('click', function () {
      var target = frameURL();
      if (target === '') {
        setMessage('The agent URL is not configured.', 'error');
        return;
      }
      window.open(target, '_blank', 'noopener');
    });
    elements['workspace-terminal'].addEventListener('click', openTerminal);
    elements['workspace-reload'].addEventListener('click', function () { loadFrame(); });
    elements['action-start'].addEventListener('click', function () { deploymentAction('start'); });
    elements['action-stop'].addEventListener('click', function () { deploymentAction('stop'); });
    elements['action-restart'].addEventListener('click', function () { deploymentAction('restart'); });
    elements['action-refresh'].addEventListener('click', async function () {
      if (state.busy) {
        return;
      }
      setBusy(true);
      try {
        await refreshStatus();
      } catch (err) {
        setMessage(err.message, 'error');
      } finally {
        setBusy(false);
      }
    });
    elements['save-config'].addEventListener('click', saveConfig);
    elements['reload-config'].addEventListener('click', reloadConfig);
    updateControls();
    initAuth();
  }

  document.addEventListener('DOMContentLoaded', init);
}());
