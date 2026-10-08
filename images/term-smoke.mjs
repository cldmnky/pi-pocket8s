// CI smoke prober for the pi-terminal WebSocket: connects with the owner
// token from SMOKE_TOKEN, runs one command through a real PTY, and expects
// its echo. Any failure exits nonzero with the transcript on stderr.
const token = process.env.SMOKE_TOKEN;
if (!token) {
  console.error('SMOKE_TOKEN is not set');
  process.exit(1);
}
const ws = new WebSocket('ws://127.0.0.1:8081/ws?token=' + encodeURIComponent(token));
let out = '';
let done = false;
const timer = setTimeout(() => {
  console.error('terminal echo timed out, got: ' + JSON.stringify(out));
  process.exit(1);
}, 30000);
ws.onopen = () => ws.send('echo smoke-term-ok\n');
ws.onmessage = async (e) => {
  if (typeof e.data === 'string') {
    out += e.data;
  } else if (e.data instanceof ArrayBuffer) {
    out += Buffer.from(e.data).toString();
  } else if (e.data && typeof e.data.arrayBuffer === 'function') {
    // Node's WebSocket delivers binary frames as Blob.
    out += Buffer.from(await e.data.arrayBuffer()).toString();
  }
  if (out.includes('smoke-term-ok')) {
    done = true;
    clearTimeout(timer);
    ws.close();
  }
};
ws.onclose = () => {
  if (done) {
    console.log('terminal echo ok');
  } else {
    console.error('terminal closed before echo: ' + JSON.stringify(out));
    process.exitCode = 1;
  }
};
ws.onerror = () => {
  console.error('terminal websocket error');
  process.exit(1);
};
