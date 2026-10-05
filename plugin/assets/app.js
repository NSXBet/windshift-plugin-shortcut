// Shortcut migration admin panel (tk-swm).
//
// Talks only to this plugin's own routes (same-origin, the plugin base URL
// is the page URL): GET /config for the status report, POST /config for the
// form, POST /sync/tick and POST /sync/reset for operator actions.
// Secrets are write-only: blank inputs are omitted so handleSaveConfig's
// nil-keep pointer fields preserve the stored token/webhook secret.

// Page lives at /api/plugins/<name>/assets/index.html; a document-relative
// path would resolve ./config to /assets/config (404), so the plugin API
// root is derived from the URL (pattern from the original skeleton).
const base = `/api/plugins/${window.location.pathname.split('/')[3]}`;

const $ = (id) => document.getElementById(id);

// Windshift's IframePluginLoader keeps the iframe at opacity:0 behind a
// spinner until it receives {type: 'plugin:ready'} via postMessage.
const post = (msg) => window.parent.postMessage(msg, window.location.origin);
const resize = () =>
  post({ type: 'plugin:resize', height: Math.ceil(document.body.scrollHeight) });

function esc(s) {
  return String(s).replace(/[&<>"']/g, (c) =>
    ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' })[c]);
}

async function api(method, path, body) {
  const res = await fetch(`${base}${path}`, {
    method,
    headers: body !== undefined ? { 'content-type': 'application/json' } : undefined,
    body: body !== undefined ? JSON.stringify(body) : undefined,
  });
  const text = await res.text();
  let data = null;
  if (text) { try { data = JSON.parse(text); } catch { data = { error: text }; } }
  if (!res.ok) throw new Error(data?.error || `HTTP ${res.status}`);
  return data;
}

function renderStatus(rep) {
  const c = rep.config;
  const rows = [
    ['Enabled', c.enabled ? '<span class="on">on</span>' : '<span class="off">off</span>'],
    ['Dry-run', c.dry_run ? '<span class="on">yes</span>' : '<span class="off">no</span>'],
    ['Workspace', esc(c.workspace_id || '—')],
    ['Projects', esc((c.project_ids || []).join(', ') || '—')],
    ['Label mode', esc(c.label_mode)],
    ['Actor', c.actor_user_id ? `#${c.actor_user_id}` : '—'],
    ['API token', c.token_set ? '<span class="on">set</span>' : '<span class="off">unset</span>'],
    ['Webhook secret', c.webhook_secret_set ? '<span class="on">set</span>' : '<span class="off">unset</span>'],
    ['Backfill', `${c.backfill_days} days`],
  ];
  let html = `<dl class="grid">${rows.map(([k, v]) => `<dt>${k}</dt><dd>${v}</dd>`).join('')}</dl>`;
  if (rep.state) {
    const s = rep.state;
    const w = s.story_window_start ? `${s.story_window_start.slice(0, 10)} → ${s.story_window_end.slice(0, 10)}` : '—';
    const cursors = [
      ['Phase', esc(s.phase)],
      ['Window', esc(w)],
      ['Last window end', esc(s.last_window_end || '—')],
      ['Cursors', `story ${s.story_idx || 0} · comment ${s.comment_idx || 0} · sweep ${s.story_sweep_idx || 0} · catalog ${s.catalog_epic_idx || 0}`],
      ['Counts', `created ${s.counts?.created || 0} · updated ${s.counts?.updated || 0} · comments ${s.counts?.comments || 0} · skipped ${s.counts?.skipped || 0} · errors ${s.counts?.errors || 0}`],
    ];
    html += `<h2 style="margin-top:0.9rem">Sync state</h2><dl class="grid">${cursors.map(([k, v]) => `<dt>${k}</dt><dd>${v}</dd>`).join('')}</dl>`;
    if (s.last_errors?.length) {
      html += `<div class="error errlist" style="margin-top:0.5rem">Last errors</div><pre>${esc(s.last_errors.join('\n'))}</pre>`;
    }
  } else {
    html += '<p class="sub" style="margin-top:0.5rem">No sync state yet — save the config, then tick.</p>';
  }
  $('status').innerHTML = html;
}

async function load() {
  try {
    const rep = await api('GET', '/config');
    if (!rep.config) throw new Error('not configured yet');
    renderStatus(rep);
    $('cfg-workspace').value = rep.config.workspace_id || '';
    $('cfg-projects').value = (rep.config.project_ids || []).join(', ');
    $('cfg-label-mode').value = rep.config.label_mode || 'merge';
    $('cfg-actor').value = rep.config.actor_user_id || '';
    $('cfg-backfill').value = rep.config.backfill_days || 30;
    $('cfg-enabled').checked = rep.config.enabled;
    $('cfg-dryrun').checked = rep.config.dry_run;
  } catch (err) {
    $('status').innerHTML = `<span class="error">${esc(err.message)}</span>`;
  }
  resize();
}

async function save() {
  const btn = $('save');
  const msg = $('save-msg');
  msg.textContent = '';
  msg.style.color = '';
  const ids = $('cfg-projects').value.split(',').map((s) => s.trim()).filter(Boolean)
    .map((s) => Number(s));
  if (ids.some((n) => !Number.isInteger(n) || n <= 0)) {
    msg.textContent = 'project ids must be comma-separated integers';
    msg.style.color = '#d33';
    return;
  }
  const upd = {
    enabled: $('cfg-enabled').checked,
    dry_run: $('cfg-dryrun').checked,
    workspace_id: $('cfg-workspace').value.trim(),
    project_ids: ids,
    label_mode: $('cfg-label-mode').value,
    actor_user_id: Number($('cfg-actor').value) || 0,
    backfill_days: Number($('cfg-backfill').value) || 0,
  };
  const token = $('cfg-token').value;
  if (token) upd.token = token;
  const secret = $('cfg-webhook').value;
  if (secret) upd.webhook_secret = secret;
  btn.disabled = true;
  try {
    await api('POST', '/config', upd);
    msg.textContent = 'saved';
    msg.style.color = '#2a2';
    $('cfg-token').value = '';
    $('cfg-webhook').value = '';
    await load();
  } catch (err) {
    msg.textContent = err.message;
    msg.style.color = '#d33';
  } finally {
    btn.disabled = false;
    resize();
  }
}

async function tick() {
  const btn = $('tick');
  const msg = $('sync-msg');
  const out = $('tick-out');
  msg.textContent = 'ticking…';
  msg.style.color = '';
  out.hidden = true;
  btn.disabled = true;
  try {
    const r = await api('POST', '/sync/tick');
    msg.textContent = `done: ${r.counts?.created || 0} created, ${r.counts?.updated || 0} updated, ${r.counts?.comments || 0} comments, ${r.counts?.skipped || 0} skipped, ${r.counts?.errors || 0} errors`;
    if (r.errors?.length) {
      out.textContent = r.errors.join('\n');
      out.hidden = false;
    }
    await load();
  } catch (err) {
    msg.textContent = err.message;
    msg.style.color = '#d33';
  } finally {
    btn.disabled = false;
    resize();
  }
}

async function reset() {
  const btn = $('reset');
  const msg = $('sync-msg');
  if (!confirm('Reset sync state? Already-imported items stay, cursors and the window restart from the watermark.')) return;
  btn.disabled = true;
  try {
    await api('POST', '/sync/reset');
    msg.textContent = 'state reset';
    msg.style.color = '#2a2';
    await load();
  } catch (err) {
    msg.textContent = err.message;
    msg.style.color = '#d33';
  } finally {
    btn.disabled = false;
    resize();
  }
}

$('save').addEventListener('click', save);
$('tick').addEventListener('click', tick);
$('reset').addEventListener('click', reset);

post({ type: 'plugin:ready' });
load();
