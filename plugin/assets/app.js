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
  if (!res.ok) {
    const err = new Error(data?.error || `HTTP ${res.status}`);
    err.status = res.status;
    throw err;
  }
  return data;
}

function renderStatus(rep) {
  const c = rep.config;
  const rows = [
    ['Enabled', c.enabled ? '<span class="on">on</span>' : '<span class="off">off</span>'],
    ['Dry-run', c.dry_run ? '<span class="on">yes</span>' : '<span class="off">no</span>'],
    ['Workspace', esc(c.workspace_id || '—')],
    ['Workspace mode', esc(c.workspace_mode || 'single')],
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
    $('cfg-workspace-mode').value = rep.config.workspace_mode || 'single';
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
    workspace_mode: $('cfg-workspace-mode').value,
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

async function purge() {
  const btn = $('purge');
  const msg = $('sync-msg');
  if (!confirm('Purge EVERYTHING this sync created — items, mappings, workspaces, team workflows — and re-import from scratch? This deletes synced items for real.')) return;
  btn.disabled = true;
  try {
    const rep = await api('POST', '/sync/purge');
    msg.textContent = `purged: ${rep.items} items, ${rep.workspaces} workspaces, ${rep.workflows} workflows`;
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

// --- dump & restore (tk-dump) ---

// Raw-body variant of api(): restore chunks are verbatim NDJSON slices, not
// JSON documents — stringifying them would corrupt the payload.
async function apiRaw(method, path, raw) {
  const res = await fetch(`${base}${path}`, {
    method,
    headers: { 'content-type': 'application/x-ndjson' },
    body: raw,
  });
  const text = await res.text();
  let data = null;
  if (text) { try { data = JSON.parse(text); } catch { data = { error: text }; } }
  if (!res.ok) throw new Error(data?.error || `HTTP ${res.status}`);
  return data;
}

function drReport(msg, err) {
  const el = $('dr-msg');
  el.textContent = msg || '';
  el.style.color = err ? '#d33' : '';
}

function renderDump(d) {
  if (!d) {
    $('dump-download').disabled = true;
    return 'No dump — start one to export the full account.';
  }
  if (d.phase === 'meta') {
    $('dump-download').disabled = true;
    return 'Dump started — it ticks itself every 10s; first lines land within seconds.';
  }
  const c = d.counts || {};
  const tally = ['members', 'teams', 'workflows', 'epic_workflows', 'projects',
    'milestones', 'epics', 'epic_comments', 'iterations', 'stories', 'comments']
    .map((k) => `${k.replace(/_/g, ' ')} ${c[k] || 0}`).join(' · ');
  const done = d.phase === 'done';
  $('dump-download').disabled = !done && !d.pieces && !d.counts?.stories;
  let html = `<dl class="grid"><dt>Dump</dt><dd>${esc(d.phase)} · ${d.pieces} pieces · file ${esc(d.filename || '—')}</dd>`
    + `<dt>Lines</dt><dd>${esc(tally)}</dd></dl>`;
  if (d.error) html += `<div class="error">dump error: ${esc(d.error)}</div>`;
  return html;
}

function renderRestore(r) {
  if (!r) return 'No restore — pick a downloaded dump file and restore it.';
  const c = r.counts || {};
  const t = r.type_counts || {};
  const lines = ['stories', 'comments', 'epics', 'epic_comments', 'iterations',
    'members', 'teams', 'workflows', 'projects'].filter((k) => t[k]);
  const done = r.phase === 'done';
  let html = `<dl class="grid"><dt>Restore</dt><dd>${esc(r.phase)} · chunk seq ${r.seq} idx ${r.chunk_idx} · file ${esc(r.filename || '—')}</dd>`
    + `<dt>Counts</dt><dd>created ${c.created || 0} · updated ${c.updated || 0} · comments ${c.comments || 0} · skipped ${c.skipped || 0} · errors ${c.errors || 0}</dd>`
    + `<dt>Lines in</dt><dd>${esc(lines.map((k) => `${k.replace(/_/g, ' ')} ${t[k]}`).join(' · ') || '—')}</dd></dl>`;
  if (r.error) html += `<div class="error">restore error: ${esc(r.error)}</div>`;
  return html;
}

// While a dump or restore is in flight the panel polls itself every 3s, so
// progress shows up without any click (the dump also ticks itself every 10s
// server-side); polling stops once both are idle.
let drPoll = null;
async function loadDumpRestore() {
  const [dump, restore] = await Promise.all([
    api('GET', '/dump/status').catch((e) => (e.status === 404 ? null : Promise.reject(e))),
    api('GET', '/restore/status').catch((e) => (e.status === 404 ? null : Promise.reject(e))),
  ]);
  $('dr-status').innerHTML = renderDump(dump) + '<hr style="opacity:0.15;margin:0.5rem 0" />' + renderRestore(restore);
  const active = (dump && dump.phase !== 'done') || (restore && restore.phase === 'stream');
  if (active && !drPoll) drPoll = setInterval(() => loadDumpRestore().catch(() => {}), 3000);
  if (!active && drPoll) { clearInterval(drPoll); drPoll = null; }
}

async function dumpAction(path, body) {
  drReport('working…');
  try {
    await api('POST', path, body);
    drReport('');
  } catch (err) {
    drReport(err.message, true);
  } finally {
    // Failed ticks still advance the cursor; the status render must not go stale.
    await loadDumpRestore().catch(() => {});
  }
}

// Download assembles the dump file client-side from the ordered pieces.
// While a dump is running, the last piece (n == d.pieces) is the open
// buffer, so a partial file downloads mid-run; the end line only exists
// once phase reaches 'done'.
async function dumpDownload() {
  drReport('downloading…');
  try {
    const d = await api('GET', '/dump/status');
    const running = d.phase !== 'done';
    const last = running ? d.pieces : d.pieces - 1; // while running, n == d.pieces is the open buffer
    const parts = [];
    for (let n = 0; n <= last; n++) {
      const res = await fetch(`${base}/dump/piece?n=${n}`);
      if (!res.ok) throw new Error(`piece ${n}: HTTP ${res.status}`);
      parts.push(await res.text());
    }
    const url = URL.createObjectURL(new Blob(parts, { type: 'application/x-ndjson' }));
    const a = Object.assign(document.createElement('a'), { href: url, download: d.filename || 'shortcut-dump.ndjson' });
    a.click();
    URL.revokeObjectURL(url);
    drReport(running
      ? `downloaded partial file (${last + 1} pieces so far, dump still running — no end line yet)`
      : `downloaded ${d.pieces} pieces`);
  } catch (err) {
    drReport(err.message, true);
  }
}

let restoreAbort = false;

// Restore streams the picked file to POST /restore/chunk: line-aligned
// ~256KiB slices, each re-posted until the plugin reports chunk_done
// (the invocation budget caps work per request), then the next seq.
// Progress and errors land in #dr-msg; a page reload cancels mid-run and
// the state resumes where the last chunk_done left it.
async function restoreRun() {
  const file = $('restore-file').files[0];
  if (!file) { drReport('pick the dump file first', true); return; }
  restoreAbort = false;
  const CHUNK = 256 * 1024;
  const dec = new TextDecoder();
  let seq = 0;
  let offset = 0;
  let carry = '';
  drReport('restoring…');
  // An end-of-file line ("type":"end") only exists in a finished dump; a
  // partial download restores fine but the tail (epic/story comments after
  // the last entity line) stays unattached — warn instead of silently finishing.
  let sawEnd = false;
  while (offset < file.size) {
    if (restoreAbort) { drReport('restore cancelled'); return; }
    const slice = dec.decode(file.slice(offset, Math.min(offset + CHUNK, file.size)), { stream: true });
    const nl = slice.lastIndexOf('\n');
    if (nl < 0) { carry += slice; break; }
    const body = carry + slice.slice(0, nl + 1);
    if (body.includes('"type":"end"')) sawEnd = true;
    carry = '';
    offset += nl + 1;
    for (;;) {
      if (restoreAbort) { drReport('restore cancelled'); return; }
      const rep = await apiRaw('POST', `/restore/chunk?seq=${seq}`, body);
      if (rep.done) {
        await loadDumpRestore();
        drReport(`restore complete — ${rep.counts?.created || 0} created, ${rep.counts?.updated || 0} updated, ${rep.counts?.errors || 0} errors`);
        return;
      }
      drReport(`restoring… chunk ${rep.next_seq} line ${rep.chunk_idx} · created ${rep.counts?.created || 0} · updated ${rep.counts?.updated || 0} · comments ${rep.counts?.comments || 0} · errors ${rep.counts?.errors || 0}`);
      if (rep.next_seq !== seq) { seq = rep.next_seq; break; }
    }
  }
  if (carry) { drReport('file does not end with a newline; trailing bytes ignored', true); }
  else if (!sawEnd) { drReport('warning: no end line — this dump file is incomplete (dump still running when downloaded?)', true); }
}

$('dump-start').addEventListener('click', () => dumpAction('/dump/start', { force: false }));
$('dump-tick').addEventListener('click', () => dumpAction('/dump/tick'));
$('dump-download').addEventListener('click', dumpDownload);
$('dump-discard').addEventListener('click', () => dumpAction('/dump/discard'));
$('restore-run').addEventListener('click', async () => {
  if (restoreRun.running) {
    restoreAbort = true;
    drReport('cancelling after the in-flight chunk…');
    return;
  }
  restoreRun.running = true;
  try { await restoreRun(); } finally { restoreRun.running = false; }
});
$('restore-discard').addEventListener('click', async () => {
  drReport('working…');
  try { await api('POST', '/restore/discard'); await loadDumpRestore(); drReport(''); }
  catch (err) { drReport(err.message, true); }
});

$('save').addEventListener('click', save);
$('tick').addEventListener('click', tick);
$('reset').addEventListener('click', reset);
$('purge').addEventListener('click', purge);

post({ type: 'plugin:ready' });
load();
loadDumpRestore().catch((err) => drReport(err.message, true));
