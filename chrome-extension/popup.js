/* SuperIDM Integration - popup logic. */
'use strict';

const $ = (id) => document.getElementById(id);

const send = (msg) =>
  new Promise((resolve) => {
    chrome.runtime.sendMessage(msg, (resp) => {
      if (chrome.runtime.lastError) {
        resolve({ ok: false, error: chrome.runtime.lastError.message, offline: true });
      } else {
        resolve(resp || { ok: false, error: 'no response' });
      }
    });
  });

let currentTab = null;
let online = false;

function fmtBytes(n) {
  if (!n || n < 0) return '—';
  if (n < 1024) return n + ' B';
  const u = ['KB', 'MB', 'GB', 'TB'];
  let i = -1, v = n;
  do { v /= 1024; i++; } while (v >= 1024 && i < u.length - 1);
  return (v < 10 ? v.toFixed(2) : v.toFixed(1)) + ' ' + u[i];
}

const STATE_LABEL = {
  downloading: 'downloading', connecting: 'connecting', queued: 'queued',
  paused: 'paused', completed: 'done', error: 'failed',
  canceled: 'canceled', verifying: 'verifying',
};

function toast(text, isError) {
  const old = document.querySelector('.toast');
  if (old) old.remove();
  const el = document.createElement('div');
  el.className = 'toast' + (isError ? ' err' : '');
  el.textContent = text;
  document.body.appendChild(el);
  setTimeout(() => el.remove(), 4000);
}

function esc(s) {
  return String(s ?? '').replace(/[&<>"']/g, (c) =>
    ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));
}

/* ------------------------------------------------------------- rendering */

function renderStatus() {
  const el = $('status');
  if (online) {
    el.textContent = 'connected to SuperIDM';
    el.classList.add('online');
  } else {
    el.textContent = 'not running';
    el.classList.remove('online');
  }
  $('offline').hidden = online;
}

function renderJobs(jobs) {
  const list = (jobs || []).filter((j) => j.state !== 'canceled').slice(0, 25);
  $('jobCount').textContent = list.length;
  $('jobs').innerHTML = list.map((j) => {
    const pct = j.total > 0 ? Math.min(100, j.percent) : 0;
    const tagClass = j.state === 'completed' ? 'done' : (j.state === 'error' ? 'err' : '');
    const speed = j.speed > 0 ? ` · ${fmtBytes(j.speed)}/s` : '';
    const size = j.total > 0
      ? `${fmtBytes(j.done)} / ${fmtBytes(j.total)}`
      : `${fmtBytes(j.done)}${j.segmentsTotal ? ` · ${j.segmentsDone || 0}/${j.segmentsTotal} segments` : ''}`;
    const canPause = j.state === 'downloading' || j.state === 'connecting';
    const canResume = j.state === 'paused' || j.state === 'error' || j.state === 'queued';
    return `<li class="item">
      <div class="info">
        <div class="name" title="${esc(j.url)}">${esc(j.fileName || j.url)}</div>
        <div class="meta"><span>${size}${speed}</span>
          <span class="tag ${tagClass}">${STATE_LABEL[j.state] || j.state}</span></div>
        <div class="bar"><i style="width:${pct}%"></i></div>
      </div>
      ${canPause ? `<button class="mini" data-job="${esc(j.id)}" data-act="pause">❚❚</button>` : ''}
      ${canResume ? `<button class="mini" data-job="${esc(j.id)}" data-act="resume">▶</button>` : ''}
      ${j.state === 'completed' ? `<button class="mini" data-job="${esc(j.id)}" data-act="open">⧉</button>` : ''}
    </li>`;
  }).join('');
}

function renderMedia(items, appItems) {
  const appById = new Map((appItems || []).map((i) => [i.id, i]));
  const appByUrl = new Map((appItems || []).map((i) => [i.url, i]));
  const list = items || [];
  $('mediaCount').textContent = list.length;
  $('mediaEmpty').hidden = list.length > 0;

  $('media').innerHTML = list.slice(0, 30).map((m) => {
    const appId = (appByUrl.get(m.url) || {}).id;
    const quality = m.height ? `${m.height}p` : (m.width ? `${m.width}px` : '');
    const host = (() => { try { return new URL(m.url).hostname; } catch { return ''; } })();
    const tag = m.kind === 'hls' ? 'hls' : m.kind === 'dash' ? 'dash' : m.kind;
    const done = appById.has(appId) && appById.get(appId).jobId;
    return `<li class="item">
      <div class="info">
        <div class="name" title="${esc(m.url)}">${esc(m.title || m.url.split('/').pop())}</div>
        <div class="meta">
          <span class="tag ${tag === 'hls' ? 'hls' : tag === 'video' ? 'video' : ''}">${tag}</span>
          <span>${esc(host)}</span>
          ${quality ? `<span>${quality}</span>` : ''}
          ${m.size ? `<span>${fmtBytes(m.size)}</span>` : ''}
        </div>
      </div>
      <button class="mini" data-media="${esc(appId || '')}" data-url="${esc(m.url)}"
              data-kind="${esc(m.kind)}" data-title="${esc(m.title || '')}">⬇</button>
    </li>`;
  }).join('');
}

/* ---------------------------------------------------------------- actions */

async function refresh() {
  const [tab] = await chrome.tabs.query({ active: true, currentWindow: true });
  currentTab = tab || null;

  const status = await send({ type: 'superidm:status' });
  online = !!(status && status.ok && status.online);
  renderStatus();

  if (status && status.config) {
    $('intercept').checked = !!status.config.interceptDownloads;
    $('sniff').checked = !!status.config.sniffMedia;
  }

  if (online) {
    renderJobs(status.jobs);
    const appMedia = await send({ type: 'superidm:scan', tabId: currentTab ? currentTab.id : -1 })
      .then(async () => {
        const r = await fetch(`http://127.0.0.1:${status.health ? (status.config.port) : 8765}/api/sniff`)
          .catch(() => null);
        return r ? r.json() : { items: [] };
      })
      .catch(() => ({ items: [] }));
    const local = await send({ type: 'superidm:media', tabId: currentTab ? currentTab.id : -1 });
    renderMedia((local && local.items) || [], (appMedia && appMedia.items) || []);
  } else {
    renderJobs([]);
    const local = await send({ type: 'superidm:media', tabId: currentTab ? currentTab.id : -1 });
    renderMedia((local && local.items) || [], []);
  }
}

function pageName() {
  if (!currentTab) return '';
  const t = (currentTab.title || 'page').replace(/[\\/:*?"<>|]/g, '_').slice(0, 120);
  return t + '.html';
}

document.addEventListener('click', async (ev) => {
  const t = ev.target.closest('button');
  if (!t) return;

  // Per-job controls.
  if (t.dataset.job) {
    const res = await send({ type: 'superidm:jobAction', id: t.dataset.job, action: t.dataset.act });
    if (!res.ok) toast(res.error || 'Action failed', true);
    setTimeout(refresh, 400);
    return;
  }

  // Media download.
  if (t.dataset.media || t.dataset.url) {
    let res;
    if (t.dataset.media) {
      res = await send({ type: 'superidm:downloadMedia', id: t.dataset.media });
    }
    if ((!res || !res.ok) && t.dataset.url) {
      res = await send({
        type: 'superidm:download',
        url: t.dataset.url,
        kind: t.dataset.kind,
        title: t.dataset.title,
        pageUrl: currentTab ? currentTab.url : '',
      });
    }
    toast(res && res.ok ? 'Added to SuperIDM' : ((res && res.error) || 'Could not add'), !(res && res.ok));
    setTimeout(refresh, 600);
    return;
  }

  switch (t.id) {
    case 'go': {
      const url = $('url').value.trim();
      if (!url) return;
      const res = await send({
        type: 'superidm:download',
        url,
        pageUrl: currentTab ? currentTab.url : '',
      });
      if (res.ok) {
        toast('Added to SuperIDM');
        $('url').value = '';
      } else {
        toast(res.error || 'Failed', true);
      }
      setTimeout(refresh, 600);
      break;
    }
    case 'pageFile': {
      if (!currentTab) return;
      const res = await send({
        type: 'superidm:download',
        url: currentTab.url,
        fileName: pageName(),
        pageUrl: currentTab.url,
      });
      toast(res.ok ? 'Page sent to SuperIDM' : (res.error || 'Failed'), !res.ok);
      break;
    }
    case 'allLinks': {
      if (!currentTab) return;
      const r = await send({ type: 'superidm:collect' });
      const links = (r && r.links) || [];
      if (!links.length) {
        toast('No downloadable links found');
        break;
      }
      let n = 0;
      for (const l of links.slice(0, 100)) {
        const res = await send({
          type: 'superidm:download',
          url: l.url,
          fileName: l.fileName,
          pageUrl: currentTab.url,
        });
        if (res.ok) n++;
      }
      toast(`Queued ${n} link${n === 1 ? '' : 's'}`);
      setTimeout(refresh, 600);
      break;
    }
    case 'scan': {
      if (!currentTab) return;
      const r = await send({ type: 'superidm:scan', tabId: currentTab.id });
      toast(r.ok ? 'Scan complete' : (r.error || 'Scan failed'), !r.ok);
      setTimeout(refresh, 500);
      break;
    }
    case 'pauseAll':
      await send({ type: 'superidm:bulk', action: 'pause-all' });
      setTimeout(refresh, 400);
      break;
    case 'resumeAll':
      await send({ type: 'superidm:bulk', action: 'resume-all' });
      setTimeout(refresh, 400);
      break;
    case 'openApp':
      chrome.tabs.create({ url: 'http://127.0.0.1:8765/' });
      break;
    case 'settings':
      chrome.runtime.openOptionsPage();
      break;
    case 'retry':
      refresh();
      break;
  }
});

$('url').addEventListener('keydown', (e) => {
  if (e.key === 'Enter') $('go').click();
});

$('intercept').addEventListener('change', (e) =>
  send({ type: 'superidm:config', patch: { interceptDownloads: e.target.checked } }));
$('sniff').addEventListener('change', (e) =>
  send({ type: 'superidm:config', patch: { sniffMedia: e.target.checked } }));

// Keep the panel live while it is open.
const timer = setInterval(() => {
  if (!document.hidden) refresh();
}, 1200);
window.addEventListener('unload', () => clearInterval(timer));

document.addEventListener('DOMContentLoaded', refresh);
