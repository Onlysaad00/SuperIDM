/* SuperIDM Integration - options page. */
'use strict';

const DEFAULTS = {
  port: 8765,
  interceptDownloads: true,
  sniffMedia: true,
  sendCookies: true,
  notifyOnComplete: true,
  showFloatingButton: true,
};

const $ = (id) => document.getElementById(id);

async function load() {
  const cfg = { ...DEFAULTS, ...(await chrome.storage.local.get(Object.keys(DEFAULTS))) };
  $('port').value = cfg.port;
  for (const key of Object.keys(DEFAULTS)) {
    const el = $(key);
    if (!el || el.type === 'number') continue;
    el.checked = !!cfg[key];
  }
}

async function save() {
  const patch = {
    port: Math.max(1024, Math.min(65535, parseInt($('port').value, 10) || 8765)),
    interceptDownloads: $('interceptDownloads').checked,
    sniffMedia: $('sniffMedia').checked,
    showFloatingButton: $('showFloatingButton').checked,
    sendCookies: $('sendCookies').checked,
    notifyOnComplete: $('notifyOnComplete').checked,
  };
  await chrome.storage.local.set(patch);
  const saved = $('saved');
  saved.textContent = 'Saved ✓';
  saved.className = 'result ok';
  setTimeout(() => { saved.textContent = ''; }, 1800);
  return patch;
}

$('test').addEventListener('click', async () => {
  const res = $('testResult');
  res.textContent = 'Connecting…';
  res.className = 'result';
  const port = parseInt($('port').value, 10) || 8765;
  try {
    const r = await fetch(`http://127.0.0.1:${port}/api/health`);
    const data = await r.json();
    if (data && data.ok) {
      res.textContent = `Connected ✓ SuperIDM ${data.version}`;
      res.className = 'result ok';
    } else {
      throw new Error('unexpected reply');
    }
  } catch (err) {
    res.textContent = `Not reachable on port ${port}. Is SuperIDM.exe running?`;
    res.className = 'result err';
  }
});

for (const id of ['port', 'interceptDownloads', 'sniffMedia', 'showFloatingButton', 'sendCookies', 'notifyOnComplete']) {
  $(id).addEventListener('change', save);
}

load();
