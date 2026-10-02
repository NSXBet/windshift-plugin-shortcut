// Shortcut plugin admin-tab script. Loaded as an EXTERNAL module (app.js)
// because the core serves plugin HTML under the app CSP, whose script-src
// ('self' + per-response nonce) blocks inline scripts.

// Page lives at /api/plugins/<name>/assets/index.html. A document-relative
// path would resolve ./status to /assets/status (404); derive the plugin
// API root from the URL instead.
const base = `/api/plugins/${window.location.pathname.split('/')[3]}`;

const show = (e) => { document.getElementById('error').textContent = String(e.message || e); };

// Windshift's IframePluginLoader keeps the iframe at opacity:0 behind a
// spinner until it receives {type: 'plugin:ready'} via postMessage.
const post = (msg) => window.parent.postMessage(msg, window.location.origin);
const reportHeight = () =>
  post({ type: 'plugin:resize', height: Math.ceil(document.body.scrollHeight) });

try {
  const res = await fetch(`${base}/status`);
  const data = await res.json().catch(() => ({}));
  if (!res.ok) throw new Error(data.error || `${res.status} ${res.statusText}`);
  document.getElementById('status').textContent =
    `v${data.version} — KV host function ${data.kv ? 'reachable' : 'FAILED'}`;
} catch (e) { show(e); } finally {
  reportHeight();
  post({ type: 'plugin:ready' });
}
