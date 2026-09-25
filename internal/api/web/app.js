/* SuperIDM UI — vanilla JS, no build step, no dependencies. */
'use strict';

const App = (() => {
  const $ = (id) => document.getElementById(id);
  const api = async (path, opts = {}) => {
    const res = await fetch(path, {
      headers: { 'Content-Type': 'application/json' },
      ...opts,
    });
    const text = await res.text();
    let data = null;
    try { data = text ? JSON.parse(text) : null; } catch { data = { raw: text }; }
    if (!res.ok) throw new Error((data && data.error) || res.statusText);
    return data;
  };

  let jobs = [];
  let media = [];
  let settings = null;
  let limits = null;
  let peak = 0;
  let filter = '';
  let inspectTarget = null;
  let sessionBytes = 0;
  let lastTotals = new Map();

  /* ------------------------------------------------------------ formatting */

  const bytes = (n) => {
    if (n === undefined || n === null || n < 0) return '—';
    if (n < 1024) return n + ' B';
    const u = ['KB', 'MB', 'GB', 'TB', 'PB'];
    let i = -1, v = n;
    do { v /= 1024; i++; } while (v >= 1024 && i < u.length - 1);
    return (v < 10 ? v.toFixed(2) : v.toFixed(1)) + ' ' + u[i];
  };
  const rate = (n) => (n > 0 ? bytes(n) + '/s' : '—');
  const eta = (s) => {
    if (s === undefined || s < 0) return '—';
    if (s < 60) return s + 's';
    if (s < 3600) return Math.floor(s / 60) + 'm ' + (s % 60) + 's';
    return Math.floor(s / 3600) + 'h ' + Math.floor((s % 3600) / 60) + 'm';
  };
  const esc = (s) => String(s === undefined || s === null ? '' : s)
    .replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;')
    .replace(/"/g, '&quot;');

  const STATE_LABEL = {
    queued: 'Queued', connecting: 'Connecting', downloading: 'Downloading',
    paused: 'Paused', verifying: 'Verifying', completed: 'Complete',
    error: 'Failed', canceled: 'Canceled',
  };

  /* ------------------------------------------------------------ rendering */

  function renderJobs() {
    const q = filter.toLowerCase();
    const list = jobs.filter((j) => !q ||
      (j.fileName || '').toLowerCase().includes(q) ||
      (j.url || '').toLowerCase().includes(q));

    const tbody = $('jobRows');
    tbody.innerHTML = list.map(rowHTML).join('');
    $('emptyJobs').classList.toggle('hide', list.length > 0);
    $('badgeJobs').textContent = jobs.length;

    let speed = 0, active = 0, conns = 0;
    for (const j of jobs) {
      if (j.state === 'downloading' || j.state === 'connecting') {
        speed += j.speed || 0;
        active++;
        conns += j.active || 0;
      }
      // Session transfer total (monotonic).
      const prev = lastTotals.get(j.id) || 0;
      if ((j.done || 0) > prev) { sessionBytes += (j.done - prev); lastTotals.set(j.id, j.done); }
    }
    if (speed > peak) peak = speed;

    $('gSpeed').textContent = bytes(speed) + '/s';
    $('gActive').textContent = active;
    $('gConns').textContent = conns;
    $('gPeak').textContent = bytes(peak) + '/s';
    $('statusRate').textContent = active ? `${active} active · ${conns} connections in use` : '';
    $('statusSaved').textContent = sessionBytes > 0 ? `transferred this session: ${bytes(sessionBytes)}` : '';

    const rows = $('jobRows').children.length;
    $('statusText').textContent = rows
      ? `${jobs.length} item${jobs.length === 1 ? '' : 's'}`
      : 'Ready';
  }

  function rowHTML(j) {
    const pct = j.total > 0 ? Math.min(100, j.percent || 0) : 0;
    const indeterminate = (!j.total || j.total <= 0) && (j.state === 'downloading' || j.state === 'connecting');
    const barCls = j.state === 'completed' ? 'done'
      : (j.state === 'error' || j.state === 'canceled') ? 'err'
      : j.state === 'paused' || j.state === 'queued' ? 'paused' : '';

    let meta;
    if (j.state === 'completed') {
      meta = `<span>${bytes(j.total)}</span><span class="dim">${j.segments || 0} segments</span>`;
      if (j.checksum) meta += `<span class="dim">checksum ${j.checksumOk ? '✓ verified' : 'checked'}</span>`;
    } else if (indeterminate) {
      meta = `<span>${bytes(j.done)}</span><span class="dim">${j.segmentsTotal ? `${j.segmentsDone || 0}/${j.segmentsTotal} segments` : 'size unknown'}</span>`;
    } else {
      meta = `<span>${bytes(j.done)} / ${bytes(j.total)}</span><span class="dim">${pct.toFixed(1)}%</span>`;
      if (j.retries) meta += `<span class="dim">${j.retries} retr${j.retries === 1 ? 'y' : 'ies'}</span>`;
    }

    let actions = '';
    const id = esc(j.id);
    if (j.state === 'downloading' || j.state === 'connecting') {
      actions += btn(id, 'pause', '❚❚', 'Pause');
    } else if (j.state === 'paused' || j.state === 'queued' || j.state === 'error') {
      actions += btn(id, 'resume', '▶', 'Resume');
    }
    if (j.state === 'completed' || j.state === 'error') {
      actions += btn(id, 'restart', '↻', 'Restart from scratch');
    }
    if (j.state === 'completed') {
      actions += btn(id, 'open', '⧉', 'Open file');
    }
    if (j.savePath) actions += btn(id, 'reveal', '🗀', 'Show in folder');
    actions += btn(id, 'copy', '⧉', 'Copy source link');

    const title = j.fileName || j.url;
    return `<tr>
      <td>
        <div class="fname">
          <b title="${esc(title)}">${esc(title)}</b>
          <span title="${esc(j.url)}">${esc(j.category ? '[' + j.category + '] ' : '')}${esc(j.url)}</span>
        </div>
      </td>
      <td class="mono">${j.total > 0 ? bytes(j.total) : '—'}</td>
      <td>
        <div class="bar ${barCls} ${indeterminate ? 'indeterminate' : ''}"><i style="width:${indeterminate ? 35 : pct}%"></i></div>
        <div class="prog-meta">${meta}</div>
      </td>
      <td class="mono">${rate(j.speed)}</td>
      <td class="mono">${j.state === 'completed' ? '—' : eta(j.eta)}</td>
      <td><span class="pill ${j.state}" title="${esc(j.error || '')}">${STATE_LABEL[j.state] || j.state}${j.connections ? ` · ${j.connections}c` : ''}</span></td>
      <td><div class="row-actions">${actions}</div></td>
    </tr>`;
  }

  const btn = (id, action, glyph, title) =>
    `<button class="icon-btn" title="${title}" onclick="App.jobAction('${id}','${action}')">${glyph}</button>`;

  function renderMedia() {
    const tbody = $('mediaRows');
    tbody.innerHTML = media.map((m) => {
      const label = m.kind === 'hls' ? 'HLS stream' : m.kind === 'dash' ? 'DASH manifest'
        : m.kind === 'audio' ? 'Audio' : m.kind === 'video' ? 'Video' : 'File';
      const quality = m.height ? `${m.height}p` : (m.width ? m.width + 'px' : '—');
      let host = '';
      try { host = new URL(m.pageUrl || m.url).hostname; } catch {}
      const title = m.title || m.url.split('/').pop() || m.url;
      const disabled = m.jobId ? 'disabled' : '';
      return `<tr>
        <td><div class="fname"><b title="${esc(title)}">${esc(title)}</b><span>${esc(m.url.slice(0, 120))}</span></div></td>
        <td>${label}${m.mime ? `<br><span class="dim" style="font-size:11px">${esc(m.mime)}</span>` : ''}</td>
        <td class="mono">${quality}${m.size ? `<br><span class="dim" style="font-size:11px">${bytes(m.size)}</span>` : ''}</td>
        <td class="dim">${esc(host)}</td>
        <td><div class="row-actions">
          <button class="btn small primary" ${disabled} onclick="App.downloadMedia('${esc(m.id)}')">
            ${m.jobId ? 'Queued' : 'Download'}</button>
          <button class="icon-btn danger" title="Remove" onclick="App.removeMedia('${esc(m.id)}')">✕</button>
        </div></td>
      </tr>`;
    }).join('');
    $('emptyMedia').classList.toggle('hide', media.length > 0);
    $('badgeMedia').textContent = media.length;
  }

  function pushLog(level, msg) {
    const view = $('logView');
    const t = new Date().toLocaleTimeString();
    const cls = level === 'error' ? 'l-err' : level === 'warn' ? 'l-warn' : 'l-info';
    view.insertAdjacentHTML('beforeend', `<span class="t">${t}</span> <span class="${cls}">${esc(msg)}</span>\n`);
    while (view.childNodes.length > 800) view.removeChild(view.firstChild);
    view.scrollTop = view.scrollHeight;
  }

  function toast(msg, kind = 'ok', ms = 4200) {
    const el = document.createElement('div');
    el.className = 'toast ' + kind;
    el.textContent = msg;
    $('toasts').appendChild(el);
    setTimeout(() => el.remove(), ms);
  }

  /* ------------------------------------------------------------ actions */

  async function jobAction(id, action) {
    if (action === 'copy') {
      const j = jobs.find((x) => x.id === id);
      if (!j) return;
      await api('/api/clipboard', { method: 'POST', body: JSON.stringify({ text: j.finalUrl || j.url }) })
        .then(() => toast('Link copied'))
        .catch(() => toast('Could not copy', 'err'));
      return;
    }
    try {
      await api(`/api/jobs/${encodeURIComponent(id)}/${action}`, { method: 'POST', body: '{}' });
      if (action === 'restart') toast('Restarting from scratch');
    } catch (e) { toast(e.message, 'err'); }
  }

  async function removeJob(id, deleteFile) {
    try {
      await api(`/api/jobs/${encodeURIComponent(id)}?delete=${deleteFile ? 1 : 0}`, { method: 'DELETE' });
    } catch (e) { toast(e.message, 'err'); }
  }

  async function downloadMedia(id) {
    try {
      await api('/api/sniff/download', {
        method: 'POST',
        body: JSON.stringify({ id, connections: settings ? settings.connections : undefined }),
      });
      toast('Added to downloads — see the Downloads tab');
      await refreshMedia();
      showTab('downloads');
    } catch (e) { toast(e.message, 'err'); }
  }

  async function removeMedia(id) {
    await api('/api/sniff/remove', { method: 'POST', body: JSON.stringify({ id }) });
    await refreshMedia();
  }

  function openAdd(prefill) {
    if (prefill) {
      $('addUrl').value = prefill.url || '';
      $('addName').value = prefill.fileName || '';
      if (prefill.referer) $('addReferer').value = prefill.referer;
    }
    $('modalAdd').classList.add('open');
    setTimeout(() => $('addUrl').focus(), 30);
  }

  async function confirmAdd() {
    const body = {
      url: $('addUrl').value.trim(),
      fileName: $('addName').value.trim(),
      dir: $('addDir').value.trim(),
      referer: $('addReferer').value.trim(),
      cookie: $('addCookie').value.trim(),
      checksum: $('addChecksum').value.trim(),
      connections: parseInt($('addConns').value, 10) || 0,
      startPaused: $('addPaused').checked,
    };
    if (!body.url) { toast('Enter a URL first', 'warn'); return; }
    try {
      await api('/api/jobs', { method: 'POST', body: JSON.stringify(body) });
      closeModals();
      toast('Download added');
      $('addUrl').value = ''; $('addName').value = ''; $('addChecksum').value = '';
    } catch (e) { toast(e.message, 'err', 7000); }
  }

  function openSettings() {
    if (!settings) return;
    $('setDir').value = settings.downloadDir || '';
    $('setConns').value = settings.connections;
    $('outConns').textContent = settings.connections;
    $('setChunk').value = settings.minChunkKB;
    $('outChunk').textContent = settings.minChunkKB + ' KB';
    $('setBuf').value = settings.bufferKB;
    $('outBuf').textContent = settings.bufferKB + ' KB';
    $('setMaxConcurrent').value = settings.maxConcurrent;
    $('setRetries').value = settings.maxRetries;
    $('setIdle').value = settings.idleTimeoutSec;
    $('setPort').value = settings.port;
    $('setCategories').checked = !!settings.useCategories;
    $('setResume').checked = !!settings.resumeOnLaunch;
    $('setTray').checked = !!settings.minimizeToTray;
    $('setAutostart').checked = !!settings.autoStart;
    $('setSniffer').checked = !!settings.enableSniffer;
    $('setAPI').checked = !!settings.enableAPI;
    $('modalSettings').classList.add('open');
  }

  async function saveSettings() {
    const next = {
      ...settings,
      downloadDir: $('setDir').value.trim(),
      connections: parseInt($('setConns').value, 10),
      minChunkKB: parseInt($('setChunk').value, 10),
      bufferKB: parseInt($('setBuf').value, 10),
      maxConcurrent: parseInt($('setMaxConcurrent').value, 10),
      maxRetries: parseInt($('setRetries').value, 10),
      idleTimeoutSec: parseInt($('setIdle').value, 10),
      port: parseInt($('setPort').value, 10),
      useCategories: $('setCategories').checked,
      resumeOnLaunch: $('setResume').checked,
      minimizeToTray: $('setTray').checked,
      autoStart: $('setAutostart').checked,
      enableSniffer: $('setSniffer').checked,
      enableAPI: $('setAPI').checked,
    };
    try {
      const saved = await api('/api/settings', { method: 'POST', body: JSON.stringify(next) });
      settings = saved;
      closeModals();
      toast('Settings saved');
      if (saved.port !== next.port) toast('Restart SuperIDM for the new port to take effect', 'warn', 7000);
    } catch (e) { toast(e.message, 'err'); }
  }

  async function runInspect() {
    const url = $('inspectUrl').value.trim();
    if (!url) return;
    $('inspectOut').textContent = 'Resolving redirects and reading headers…';
    $('btnInspectDownload').disabled = true;
    try {
      const info = await api('/api/inspect?url=' + encodeURIComponent(url));
      inspectTarget = info;
      $('inspectOut').textContent = info.error
        ? '✗ ' + info.error
        : [
            '✓ reachable',
            'final URL : ' + info.finalUrl,
            'file name : ' + info.fileName,
            'size      : ' + (info.size > 0 ? bytes(info.size) + ` (${info.size} bytes)` : 'unknown'),
            'type      : ' + (info.contentType || 'unknown'),
            'kind      : ' + info.kind,
            'resumable : ' + (info.ranges ? 'yes — parallel download available' : 'no — single connection only'),
          ].join('\n');
      $('btnInspectDownload').disabled = !!info.error;
    } catch (e) {
      $('inspectOut').textContent = '✗ ' + e.message;
    }
  }

  function closeModals() { document.querySelectorAll('.modal').forEach((m) => m.classList.remove('open')); }

  function showTab(name) {
    document.querySelectorAll('.tab').forEach((t) => t.classList.toggle('active', t.dataset.tab === name));
    document.querySelectorAll('.panel').forEach((p) => p.classList.toggle('active', p.id === 'panel-' + name));
    if (name === 'setup') loadSetupInfo();
  }

  async function refreshMedia() {
    try {
      const d = await api('/api/sniff');
      media = d.items || [];
      renderMedia();
    } catch {}
  }

  async function loadSetupInfo() {
    try {
      const d = await api('/api/settings');
      settings = d.settings; limits = d.limits;
      $('apiAddr').textContent = location.origin;
      $('cfgPort').textContent = d.port;
      $('cfgDir').textContent = d.configDir;
      $('cfgConns').textContent = settings.connections;
      $('cfgMax').textContent = settings.maxConcurrent;
      $('cfgVersion').textContent = limits.version;
      $('extStatus').textContent = `Local API is listening on port ${d.port}. The extension will connect automatically.`;
    } catch (e) {
      $('extStatus').textContent = 'Could not read settings: ' + e.message;
    }
  }

  async function openConfigFolder() {
    // The API exposes the folder through a job-less helper: reuse the shell
    // opening of the log file's directory.
    await api('/api/clipboard', { method: 'POST', body: JSON.stringify({ text: ($('cfgDir').textContent || '') }) });
    toast('Config path copied — press Win+R and paste it');
  }

  /* ------------------------------------------------------------ live feed */

  function connectEvents() {
    const es = new EventSource('/api/events');
    es.onmessage = (ev) => {
      let data;
      try { data = JSON.parse(ev.data); } catch { return; }
      if (data.type === 'jobs') { jobs = data.jobs || []; renderJobs(); }
      else if (data.type === 'log') pushLog(data.level, data.message);
      else if (data.type === 'toast') toast(data.message, data.level === 'error' ? 'err' : 'ok');
      else if (data.type === 'settings') loadSetupInfo();
    };
    es.onerror = () => {
      // EventSource retries on its own; surface a hint once.
      $('statusText').textContent = 'Reconnecting to the SuperIDM service…';
    };
  }

  /* ------------------------------------------------------------ wiring */

  function init() {
    document.querySelectorAll('.tab').forEach((t) => t.onclick = () => showTab(t.dataset.tab));
    document.querySelectorAll('[data-close]').forEach((b) => b.onclick = closeModals);
    document.querySelectorAll('.modal').forEach((m) => m.addEventListener('click', (e) => {
      if (e.target === m) closeModals();
    }));
    document.addEventListener('keydown', (e) => {
      if (e.key === 'Escape') closeModals();
      if (e.key === 'Enter' && $('modalAdd').classList.contains('open') && document.activeElement === $('addUrl')) confirmAdd();
    });

    $('btnAdd').onclick = () => openAdd();
    $('btnAddConfirm').onclick = confirmAdd;
    $('btnSettings').onclick = openSettings;
    $('btnSaveSettings').onclick = saveSettings;
    $('btnInspect').onclick = () => { $('modalInspect').classList.add('open'); $('inspectUrl').focus(); };
    $('btnInspectGo').onclick = runInspect;
    $('btnInspectDownload').onclick = () => {
      if (!inspectTarget) return;
      closeModals();
      openAdd({ url: inspectTarget.finalUrl, fileName: inspectTarget.fileName });
    };
    $('search').oninput = (e) => { filter = e.target.value; renderJobs(); };

    $('btnPauseAll').onclick = () => api('/api/jobs/actions/pause-all', { method: 'POST', body: '{}' }).catch(() => {});
    $('btnResumeAll').onclick = () => api('/api/jobs/actions/resume-all', { method: 'POST', body: '{}' }).catch(() => {});
    $('btnClearDone').onclick = () => {
      if (confirm('Remove all finished downloads from the list? The files are kept on disk.')) {
        api('/api/jobs/actions/clear-completed?delete=0', { method: 'POST', body: '{}' }).catch(() => {});
      }
    };
    $('speedLimit').onchange = async (e) => {
      try {
        await api('/api/speedlimit', { method: 'POST', body: JSON.stringify({ bps: parseInt(e.target.value, 10) }) });
        toast(e.target.value === '0' ? 'Speed limit removed' : 'Speed limited to ' + bytes(parseInt(e.target.value, 10)) + '/s');
      } catch (err) { toast(err.message, 'err'); }
    };

    $('setConns').oninput = (e) => $('outConns').textContent = e.target.value;
    $('setChunk').oninput = (e) => $('outChunk').textContent = e.target.value + ' KB';
    $('setBuf').oninput = (e) => $('outBuf').textContent = e.target.value + ' KB';

    $('btnRefreshMedia').onclick = refreshMedia;
    $('btnClearMedia').onclick = async () => { await api('/api/sniff/clear', { method: 'POST', body: '{}' }); refreshMedia(); };
    $('btnClearLog').onclick = () => { $('logView').textContent = ''; };
    $('btnOpenLogFile').onclick = () => fetch('/api/log').then((r) => r.text()).then((t) => { $('logView').textContent = t; showTab('log'); });
    $('btnOpenLog').onclick = () => fetch('/api/log').then((r) => r.text()).then((t) => { $('logView').textContent = t; showTab('log'); });
    $('btnOpenConfig').onclick = openConfigFolder;
    $('btnExtensionFolder').onclick = async () => {
      try {
        const d = await api('/api/extension-path');
        await api('/api/clipboard', { method: 'POST', body: JSON.stringify({ text: d.path }) });
        toast('Extension folder path copied: ' + d.path, 'ok', 9000);
      } catch {
        toast('Look for the chrome-extension folder next to SuperIDM.exe', 'warn', 8000);
      }
    };
    $('btnTestConn').onclick = async () => {
      try {
        const d = await api('/api/health');
        toast(`Connected to SuperIDM ${d.version}`, 'ok');
        $('extStatus').textContent = `✓ Extension API reachable — SuperIDM ${d.version} on port ${location.port}.`;
      } catch (e) { toast(e.message, 'err'); }
    };

    // Clipboard auto-detect: if the app copied a link, offer it.
    window.addEventListener('paste', (e) => {
      const text = (e.clipboardData || window.clipboardData).getData('text');
      if (text && /^https?:\/\//i.test(text.trim()) && !$('modalAdd').classList.contains('open')) {
        openAdd({ url: text.trim() });
      }
    });

    Promise.all([api('/api/settings'), api('/api/limits'), refreshMedia()])
      .then(([s, l]) => { settings = s.settings; limits = l; loadSetupInfo(); })
      .catch(() => toast('Cannot reach the SuperIDM service', 'err', 8000));

    connectEvents();
  }

  document.addEventListener('DOMContentLoaded', init);

  return {
    jobAction, removeJob, downloadMedia, removeMedia, openAdd, showTab, closeModals,
  };
})();
