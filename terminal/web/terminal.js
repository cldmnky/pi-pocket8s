/* pi-pocket terminal client: ghostty-web frontend with a WebSocket PTY bridge.
 *
 * Protocol (mirrors the ghostty-web demo framing):
 *   page -> server: raw text is shell input; {"type":"resize",...} resizes;
 *   {"type":"ping"} is a keepalive the server ignores.
 *   server -> page: raw PTY bytes; {"type":"exit","code":N} ends the shell.
 */
import { init, Terminal } from './ghostty-web.js';

var container = document.getElementById('term-container');
var statusEl = document.getElementById('term-status');
var overlay = document.getElementById('term-overlay');
var overlayText = document.getElementById('term-overlay-text');
var reconnectBtn = document.getElementById('term-reconnect');
var newBtn = document.getElementById('term-new');
var fullscreenBtn = document.getElementById('term-fullscreen');

var term = null;
var socket = null;
var keepalive = null;

function setStatus(text, kind) {
  statusEl.textContent = text;
  statusEl.className = 'term-status' + (kind ? ' ' + kind : '');
}

function showOverlay(text) {
  overlayText.textContent = text;
  overlay.hidden = false;
  newBtn.hidden = false;
}

function hideOverlay() {
  overlay.hidden = true;
  newBtn.hidden = true;
}

// No FitAddon in ghostty-web 0.4.0: measure the cell from the same font the
// terminal renders with and size the grid to the container.
function measureCell() {
  var probe = document.createElement('span');
  probe.textContent = 'MMMMMMMMMM';
  probe.style.cssText = 'position:absolute;visibility:hidden;white-space:pre;font:14px "JetBrains Mono",ui-monospace,Menlo,Consolas,monospace;';
  document.body.appendChild(probe);
  var width = probe.getBoundingClientRect().width / 10;
  var height = probe.getBoundingClientRect().height || 14 * 1.2;
  probe.remove();
  return { width: Math.max(width, 1), height: Math.max(height, 1) };
}

function fit() {
  if (term === null) {
    return;
  }
  var cell = measureCell();
  var rect = container.getBoundingClientRect();
  var cols = Math.max(2, Math.min(1000, Math.floor(rect.width / cell.width)));
  var rows = Math.max(2, Math.min(1000, Math.floor(rect.height / cell.height)));
  if (cols !== term.cols || rows !== term.rows) {
    term.resize(cols, rows);
  }
}

function sendResize() {
  if (socket !== null && socket.readyState === WebSocket.OPEN && term !== null) {
    socket.send(JSON.stringify({ type: 'resize', cols: term.cols, rows: term.rows }));
  }
}

function closeSocket() {
  if (keepalive !== null) {
    window.clearInterval(keepalive);
    keepalive = null;
  }
  if (socket !== null) {
    try {
      socket.close();
    } catch (err) {
      // Already closing or closed; the handlers report the outcome.
    }
    socket = null;
  }
}

function connect() {
  hideOverlay();
  closeSocket();
  setStatus('Connecting…');

  var protocol = window.location.protocol === 'https:' ? 'wss:' : 'ws:';
  var ws = new WebSocket(protocol + '//' + window.location.host + '/ws');
  socket = ws;
  ws.binaryType = 'arraybuffer';

  ws.addEventListener('open', function () {
    setStatus('Connected', 'ok');
    sendResize();
    // Idle WebSocket connections look dead to routers and load balancers;
    // the server ignores ping frames.
    keepalive = window.setInterval(function () {
      if (ws.readyState === WebSocket.OPEN) {
        ws.send(JSON.stringify({ type: 'ping' }));
      }
    }, 25000);
  });

  ws.addEventListener('message', function (event) {
    if (typeof event.data === 'string') {
      try {
        var msg = JSON.parse(event.data);
        if (msg !== null && typeof msg === 'object' && msg.type === 'exit') {
          setStatus('Shell exited (code ' + msg.code + ')', 'bad');
          showOverlay('Shell exited with code ' + msg.code + '.');
          closeSocket();
          return;
        }
      } catch (err) {
        // Not JSON: fall through and render it.
      }
      term.write(event.data);
      return;
    }
    term.write(new Uint8Array(event.data));
  });

  ws.addEventListener('close', function () {
    if (socket === ws) {
      socket = null;
    }
    if (keepalive !== null) {
      window.clearInterval(keepalive);
      keepalive = null;
    }
    setStatus('Disconnected', 'bad');
    showOverlay('Connection closed.');
  });

  ws.addEventListener('error', function () {
    setStatus('Connection error', 'bad');
  });
}

reconnectBtn.addEventListener('click', connect);
newBtn.addEventListener('click', connect);
fullscreenBtn.addEventListener('click', function () {
  if (document.fullscreenElement) {
    document.exitFullscreen().catch(function () {
      setStatus('Could not leave full screen', 'bad');
    });
    return;
  }
  document.documentElement.requestFullscreen().catch(function () {
    setStatus('Full screen was refused by the browser', 'bad');
  });
});

var resizeTimer = null;
window.addEventListener('resize', function () {
  if (resizeTimer !== null) {
    window.clearTimeout(resizeTimer);
  }
  resizeTimer = window.setTimeout(function () {
    fit();
    sendResize();
  }, 100);
});

document.addEventListener('fullscreenchange', function () {
  fit();
  sendResize();
});

init().then(function () {
  term = new Terminal({
    cols: 80,
    rows: 24,
    fontSize: 14,
    fontFamily: '"JetBrains Mono", ui-monospace, Menlo, Consolas, monospace',
    theme: { background: '#000000', foreground: '#a9b1d6', cursor: '#7aa2f7' },
    scrollback: 10000,
  });
  term.open(container);
  term.onData(function (data) {
    if (socket !== null && socket.readyState === WebSocket.OPEN) {
      socket.send(data);
    }
  });
  term.onResize(function () {
    sendResize();
  });
  fit();
  connect();
}).catch(function (err) {
  setStatus('Terminal failed to start: ' + (err && err.message ? err.message : err), 'bad');
});
