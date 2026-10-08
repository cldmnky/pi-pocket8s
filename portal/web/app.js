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
    config: null,
    running: null,
    authorizedKeys: '',
    knownHosts: '',
    ownerLoginUrl: '',
    busy: false
  };

  var elements = {};

  var elementIds = [
    'message', 'unlock', 'token-form', 'token', 'unlock-button', 'app',
    'tab-workspace', 'tab-configure', 'panel-workspace', 'panel-configure',
    'owner-card', 'owner-missing', 'owner-link', 'owner-open', 'owner-copy', 'owner-qr',
    'agent-frame', 'frame-wrap', 'workspace-fullscreen', 'workspace-external', 'workspace-reload',
    'status-state', 'status-ready', 'status-restarted',
    'action-start', 'action-stop', 'action-restart', 'action-refresh',
    'api-key-rows', 'authorized-keys', 'known-hosts',
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
      credentials: 'omit',
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
    if (frame.getAttribute('src') !== target) {
      frame.setAttribute('src', target);
    } else {
      frame.contentWindow.location.reload();
    }
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
    if (!hasKeyChanges && body.authorizedKeys === undefined && body.knownHosts === undefined) {
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

  function init() {
    cacheElements();
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
  }

  document.addEventListener('DOMContentLoaded', init);
}());
