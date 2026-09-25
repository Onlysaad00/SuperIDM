/*
 * SuperIDM Integration - background service worker.
 *
 * Responsibilities:
 *   1. Talk to the local SuperIDM app (default http://127.0.0.1:8765).
 *   2. Add context-menu entries for links, media and pages.
 *   3. Optionally take over Chrome's own downloads and route them to SuperIDM.
 *   4. Sniff media streams (HLS .m3u8, DASH .mpd, progressive video/audio) and
 *      publish them to the app so they can be saved as a single file.
 *   5. Keep the toolbar badge in sync with what is available on the page.
 */

'use strict';

/* ------------------------------------------------------------------ config */

const DEFAULTS = {
  port: 8765,
  interceptDownloads: true,
  sniffMedia: true,
  sendCookies: true,
  notifyOnComplete: true,
  showFloatingButton: true,
  autoRename: true,
};

async function getConfig() {
  const stored = await chrome.storage.local.get(Object.keys(DEFAULTS));
  return { ...DEFAULTS, ...stored };
}

async function setConfig(patch) {
  await chrome.storage.local.set(patch);
}

/* ------------------------------------------------------------- local API */

class ApiError extends Error {
  constructor(message, offline) {
    super(message);
    this.offline = !!offline;
  }
}

async function api(path, options = {}) {
  const { port } = await getConfig();
  const url = `http://127.0.0.1:${port}${path}`;
  let res;
  try {
    res = await fetch(url, {
      headers: { 'Content-Type': 'application/json' },
      ...options,
    });
  } catch (err) {
    throw new ApiError(
      `SuperIDM is not running (tried ${url}). Start SuperIDM.exe and try again.`,
      true
    );
  }
  const text = await res.text();
  let data = null;
  if (text) {
    try { data = JSON.parse(text); } catch { data = { raw: text }; }
  }
  if (!res.ok) {
    throw new ApiError((data && data.error) || `HTTP ${res.status}`, false);
  }
  return data;
}

async function health() {
  try {
    return await api('/api/health');
  } catch {
    return null;
  }
}

/* ------------------------------------------------------------------ state */

/** tabId -> Map(url -> media item) */
const mediaByTab = new Map();
const MAX_ITEMS_PER_TAB = 60;

function rememberMedia(tabId, item) {
  if (tabId == null || tabId < 0) return false;
  if (!/^https?:/i.test(item.url || '')) return false;
  let map = mediaByTab.get(tabId);
  if (!map) {
    map = new Map();
    mediaByTab.set(tabId, map);
  }
  if (map.has(item.url)) {
    // Enrich the existing entry instead of duplicating it.
    const prev = map.get(item.url);
    map.set(item.url, { ...prev, ...item, title: item.title || prev.title });
    return false;
  }
  if (map.size >= MAX_ITEMS_PER_TAB) return false;
  map.set(item.url, item);
  return true;
}

function tabMedia(tabId) {
  const map = mediaByTab.get(tabId);
  return map ? [...map.values()] : [];
}

function mediaCount(tabId) {
  const map = mediaByTab.get(tabId);
  return map ? map.size : 0;
}

function updateBadge(tabId) {
  const n = mediaCount(tabId);
  const text = n > 0 ? String(Math.min(n, 99)) : '';
  chrome.action.setBadgeText({ tabId, text }).catch(() => {});
  if (n > 0) {
    chrome.action.setBadgeBackgroundColor({ tabId, color: '#4f8cff' }).catch(() => {});
    chrome.action.setTitle({
      tabId,
      title: `SuperIDM - ${n} media link${n === 1 ? '' : 's'} detected on this page`,
    }).catch(() => {});
  } else {
    chrome.action.setTitle({ tabId, title: 'SuperIDM' }).catch(() => {});
  }
}

/* ------------------------------------------------------------------ media */

const HLS_RE = /\.m3u8(\?|#|$)/i;
const DASH_RE = /\.mpd(\?|#|$)/i;
const VIDEO_RE = /\.(mp4|webm|mkv|mov|avi|flv|m4v|m2ts|ts|ogv|3gp)(\?|#|$)/i;
const AUDIO_RE = /\.(mp3|m4a|aac|flac|ogg|opus|wav|wma|mka)(\?|#|$)/i;
const FILE_RE = /\.(zip|rar|7z|tar|gz|bz2|xz|iso|exe|msi|apk|dmg|deb|rpm|pdf|docx?|xlsx?|pptx?|epub|mobi|csv|txt|svg|png|jpe?g|gif|webp|psd|ai|jar|crx|xpi)(\?|#|$)/i;

function classify(url, mime) {
  const m = (mime || '').toLowerCase();
  if (m.includes('mpegurl')) return 'hls';
  if (m.includes('dash')) return 'dash';
  if (m.startsWith('video/')) return 'video';
  if (m.startsWith('audio/')) return 'audio';
  if (HLS_RE.test(url)) return 'hls';
  if (DASH_RE.test(url)) return 'dash';
  if (VIDEO_RE.test(url)) return 'video';
  if (AUDIO_RE.test(url)) return 'audio';
  return 'file';
}

/* --------------------------------------------------------- cookie support */

async function cookiesFor(url) {
  const cfg = await getConfig();
  if (!cfg.sendCookies) return '';
  try {
    const jar = await chrome.cookies.getAll({ url });
    return jar.map((c) => `${c.name}=${c.value}`).join('; ');
  } catch {
    return '';
  }
}

/* ------------------------------------------------------- queuing downloads */

async function sendDownload(url, opts = {}) {
  const cfg = await getConfig();
  const payload = {
    url,
    fileName: opts.fileName || '',
    kind: opts.kind || '',
    mediaTitle: opts.title || '',
    pageUrl: opts.pageUrl || '',
    referer: opts.referer || opts.pageUrl || '',
    cookie: opts.cookie !== undefined ? opts.cookie : await cookiesFor(url),
    connections: opts.connections || 0,
    silent: opts.silent !== false,
  };
  const snap = await api('/api/jobs', { method: 'POST', body: JSON.stringify(payload) });
  if (!cfg.notifyOnComplete && opts.silent === false) {
    notify('SuperIDM', `Added ${snap.fileName || snap.url}`);
  }
  return snap;
}

function notify(title, message) {
  try {
    chrome.notifications.create('', {
      type: 'basic',
      iconUrl: 'icons/icon128.png',
      title,
      message,
      priority: 0,
    });
  } catch { /* notifications may be disabled */ }
}

/* ------------------------------------------------------- context menus */

const MENU = {
  link: 'superidm-link',
  media: 'superidm-media',
  page: 'superidm-page',
  links: 'superidm-page-links',
  streams: 'superidm-page-streams',
  select: 'superidm-selection',
  settings: 'superidm-options',
};

function buildMenus() {
  chrome.contextMenus.removeAll(() => {
    chrome.contextMenus.create({
      id: MENU.link,
      title: 'Download with SuperIDM',
      contexts: ['link'],
    });
    chrome.contextMenus.create({
      id: MENU.media,
      title: 'Download this media with SuperIDM',
      contexts: ['video', 'audio', 'image'],
    });
    chrome.contextMenus.create({
      id: MENU.select,
      title: 'Download selected text as a link',
      contexts: ['selection'],
    });
    chrome.contextMenus.create({ id: 'sep1', type: 'separator', contexts: ['page'] });
    chrome.contextMenus.create({
      id: MENU.page,
      title: 'Download this page with SuperIDM',
      contexts: ['page'],
    });
    chrome.contextMenus.create({
      id: MENU.links,
      title: 'Download all links on this page',
      contexts: ['page'],
    });
    chrome.contextMenus.create({
      id: MENU.streams,
      title: 'Send detected streams to SuperIDM',
      contexts: ['page'],
    });
    chrome.contextMenus.create({ id: 'sep2', type: 'separator', contexts: ['page'] });
    chrome.contextMenus.create({
      id: MENU.settings,
      title: 'SuperIDM settings…',
      contexts: ['page', 'action'],
    });
  });
}

chrome.runtime.onInstalled.addListener(buildMenus);
chrome.runtime.onStartup.addListener(buildMenus);

chrome.contextMenus.onClicked.addListener(async (info, tab) => {
  const pageUrl = (tab && tab.url) || info.pageUrl || '';
  try {
    switch (info.menuItemId) {
      case MENU.link:
        await sendDownload(info.linkUrl, { pageUrl, silent: false });
        notify('SuperIDM', 'Link sent to the download queue.');
        break;

      case MENU.media: {
        const media = tabMedia(tab.id).find((m) => m.url === info.srcUrl);
        if (media) {
          await api('/api/sniff/download', {
            method: 'POST',
            body: JSON.stringify({ id: media.id }),
          });
        } else {
          await sendDownload(info.srcUrl, {
            pageUrl,
            kind: classify(info.srcUrl, ''),
            silent: false,
          });
        }
        notify('SuperIDM', 'Media download started.');
        break;
      }

      case MENU.select: {
        const sel = (info.selectionText || '').trim();
        if (/^https?:\/\//i.test(sel)) {
          await sendDownload(sel, { pageUrl, silent: false });
        } else {
          notify('SuperIDM', 'The selected text is not a link.');
        }
        break;
      }

      case MENU.page:
        await sendDownload(pageUrl, { pageUrl, fileName: suggestPageName(tab), silent: false });
        break;

      case MENU.links:
        await downloadAllLinks(tab);
        break;

      case MENU.streams:
        await pushMedia(tab.id, /* force */ true);
        break;

      case MENU.settings:
        chrome.runtime.openOptionsPage();
        break;
    }
  } catch (err) {
    notify('SuperIDM error', err.message);
  }
});

function suggestPageName(tab) {
  if (!tab || !tab.title) return '';
  return tab.title.replace(/[\\/:*?"<>|]/g, '_').slice(0, 120) + '.html';
}

/** Ask the content script for every downloadable link on the page. */
async function downloadAllLinks(tab) {
  const pageUrl = tab.url;
  const resp = await chrome.tabs.sendMessage(tab.id, { type: 'superidm:collectLinks' })
    .catch(() => null);
  const links = (resp && resp.links) || [];
  if (!links.length) {
    notify('SuperIDM', 'No downloadable links found on this page.');
    return;
  }
  let queued = 0;
  const cookie = await cookiesFor(pageUrl);
  for (const item of links.slice(0, 100)) {
    try {
      await api('/api/jobs', {
        method: 'POST',
        body: JSON.stringify({
          url: item.url,
          fileName: item.fileName || '',
          referer: pageUrl,
          cookie,
          silent: true,
        }),
      });
      queued++;
    } catch { /* skip individual failures */ }
  }
  notify('SuperIDM', `Queued ${queued} link${queued === 1 ? '' : 's'}.`);
}

/** Collect media for a tab, publish it to the app, refresh the badge. */
async function pushMedia(tabId, force = false) {
  const cfg = await getConfig();
  if (!cfg.sniffMedia && !force) return 0;

  // Merge in anything the page's own DOM exposes (covers sources that never
  // produced a network request we could observe, e.g. late-loaded players).
  const dom = await chrome.tabs.sendMessage(tabId, { type: 'superidm:scanMedia' })
    .catch(() => null);
  if (dom && Array.isArray(dom.items)) {
    for (const item of dom.items) rememberMedia(tabId, item);
  }

  const items = tabMedia(tabId);
  updateBadge(tabId);
  if (!items.length) return 0;

  try {
    let tabTitle = '';
    try {
      const tab = await chrome.tabs.get(tabId);
      tabTitle = tab.title || '';
    } catch { /* tab may be gone */ }

    const payload = {
      items: items.map((m) => ({
        url: m.url,
        kind: m.kind,
        mime: m.mime,
        title: m.title || tabTitle,
        pageUrl: m.pageUrl,
        referer: m.pageUrl,
        size: m.size,
        width: m.width,
        height: m.height,
        duration: m.duration,
      })),
    };
    const res = await api('/api/sniff', { method: 'POST', body: JSON.stringify(payload) });
    // The app assigns ids; mirror them back so the popup can trigger a download.
    const appItems = await api('/api/sniff').catch(() => null);
    if (appItems && appItems.items) {
      const byUrl = new Map(appItems.items.map((i) => [i.url, i.id]));
      for (const [url, item] of mediaByTab.get(tabId) || []) {
        const id = byUrl.get(url);
        if (id) item.id = id;
      }
    }
    return res.added || 0;
  } catch {
    return 0; // the app is not running right now
  }
}

/* ------------------------------------------- taking over Chrome downloads */

chrome.downloads.onCreated.addListener(async (item) => {
  const cfg = await getConfig();
  if (!cfg.interceptDownloads) return;
  if (!item || !item.url || !/^https?:/i.test(item.url)) return;
  if (item.byExtensionId === chrome.runtime.id) return; // our own doing

  const isMedia = ['hls', 'dash', 'video', 'audio'].includes(classify(item.url, item.mime));
  // Leave tiny navigations and blob payloads to the browser.
  if (item.url.startsWith('blob:') || item.url.startsWith('data:')) return;

  const online = await health();
  if (!online) return;

  try {
    await chrome.downloads.cancel(item.id);
    await chrome.downloads.erase({ id: item.id }).catch(() => {});
  } catch { /* may already be gone */ }

  try {
    await sendDownload(item.url, {
      fileName: cfg.autoRename ? (item.filename || '').split(/[\\/]/).pop() : '',
      kind: isMedia ? classify(item.url, item.mime) : '',
      pageUrl: item.referrer || '',
      silent: true,
    });
    notify('SuperIDM', 'Download taken over by SuperIDM.');
  } catch (err) {
    notify('SuperIDM', `Could not hand over the download: ${err.message}`);
  }
});

/* ------------------------------------------------------- media sniffing */

function extraHeader(headers, name) {
  if (!headers) return '';
  const h = headers.find((x) => x.name.toLowerCase() === name);
  return h ? h.value : '';
}

const SNIFF_FILTER = {
  urls: [
    '*://*/*.m3u8*', '*://*/*.mpd*', '*://*/*.mp4*', '*://*/*.webm*',
    '*://*/*.mkv*', '*://*/*.m4s*', '*://*/*.m4v*', '*://*/*.mov*',
    '*://*/*.mp3*', '*://*/*.m4a*', '*://*/*.aac*', '*://*/*.flac*',
    '*://*/*.ogg*', '*://*/*.opus*', '*://*/*.ts',
  ],
  types: ['media', 'xmlhttprequest', 'object', 'other', 'main_frame', 'sub_frame'],
};

let sniffTimer = null;
const pendingTabs = new Set();

chrome.webRequest.onResponseStarted.addListener((details) => {
  if (details.tabId < 0) return;
  const cfg = { sniffMedia: true };
  const mime = extraHeader(details.responseHeaders, 'content-type');
  const len = parseInt(extraHeader(details.responseHeaders, 'content-length') || '0', 10) || 0;
  const kind = classify(details.url, mime);
  if (kind === 'file') return; // never auto-capture ordinary files

  const added = rememberMedia(details.tabId, {
    url: details.url,
    kind,
    mime,
    size: len,
    pageUrl: details.initiator || '',
    width: 0,
    height: 0,
    duration: 0,
  });
  if (added) {
    updateBadge(details.tabId);
    pendingTabs.add(details.tabId);
    schedulePush();
  }
}, SNIFF_FILTER, ['responseHeaders']);

function schedulePush() {
  clearTimeout(sniffTimer);
  sniffTimer = setTimeout(async () => {
    const cfg = await getConfig();
    const tabs = [...pendingTabs];
    pendingTabs.clear();
    if (cfg.sniffMedia) {
      for (const tabId of tabs) await pushMedia(tabId);
    }
  }, 700);
}

/* Clear per-tab state on navigation. */
chrome.tabs.onUpdated.addListener((tabId, changeInfo) => {
  if (changeInfo.status === 'loading' && changeInfo.url) {
    mediaByTab.delete(tabId);
    updateBadge(tabId);
  }
});
chrome.tabs.onRemoved.addListener((tabId) => mediaByTab.delete(tabId));

/* ---------------------------------------------------------- the popup API */

chrome.runtime.onMessage.addListener((msg, sender, sendResponse) => {
  (async () => {
    try {
      switch (msg.type) {
        case 'superidm:status': {
          const h = await health();
          const cfg = await getConfig();
          let jobs = [];
          if (h) {
            const data = await api('/api/jobs').catch(() => null);
            jobs = (data && data.jobs) || [];
          }
          sendResponse({ ok: true, online: !!h, health: h, config: cfg, jobs });
          break;
        }

        case 'superidm:download':
          sendResponse({
            ok: true,
            job: await sendDownload(msg.url, {
              fileName: msg.fileName,
              pageUrl: msg.pageUrl,
              referer: msg.pageUrl,
              kind: msg.kind,
              title: msg.title,
              silent: false,
            }),
          });
          break;

        case 'superidm:media':
          sendResponse({ ok: true, items: tabMedia(msg.tabId) });
          break;

        case 'superidm:scan':
          sendResponse({ ok: true, added: await pushMedia(msg.tabId, true) });
          break;

        case 'superidm:downloadMedia':
          sendResponse({
            ok: true,
            job: await api('/api/sniff/download', {
              method: 'POST',
              body: JSON.stringify({
                id: msg.id,
                connections: msg.connections || 0,
                dir: msg.dir || '',
              }),
            }),
          });
          break;

        case 'superidm:syncMedia': {
          // The popup knows the real app-side id of a captured item; keep it.
          const map = mediaByTab.get(msg.tabId);
          if (map && map.has(msg.url)) map.get(msg.url).id = msg.id;
          sendResponse({ ok: true });
          break;
        }

        case 'superidm:bulk':
          await api(`/api/jobs/actions/${msg.action}`, { method: 'POST', body: '{}' });
          sendResponse({ ok: true });
          break;

        case 'superidm:jobAction':
          await api(`/api/jobs/${encodeURIComponent(msg.id)}/${msg.action}`, {
            method: 'POST',
            body: '{}',
          });
          sendResponse({ ok: true });
          break;

        case 'superidm:collect':
          sendResponse({ ok: true, links: await collectLinks(sender.tab) });
          break;

        case 'superidm:pageMedia': {
          // Content script found media: remember it and refresh the badge.
          const tabId = sender.tab ? sender.tab.id : msg.tabId;
          let n = 0;
          for (const it of msg.items || []) {
            if (rememberMedia(tabId, it)) n++;
          }
          if (n) {
            updateBadge(tabId);
            pendingTabs.add(tabId);
            schedulePush();
          }
          sendResponse({ ok: true, added: n });
          break;
        }

        case 'superidm:config':
          await setConfig(msg.patch || {});
          sendResponse({ ok: true, config: await getConfig() });
          break;

        default:
          sendResponse({ ok: false, error: 'unknown message' });
      }
    } catch (err) {
      sendResponse({ ok: false, error: err.message, offline: !!err.offline });
    }
  })();
  return true; // keep the message channel open for the async reply
});

async function collectLinks(tab) {
  if (!tab) return [];
  const resp = await chrome.tabs.sendMessage(tab.id, { type: 'superidm:collectLinks' })
    .catch(() => null);
  return (resp && resp.links) || [];
}

/* ------------------------------------------------------------- shortcuts */

chrome.commands.onCommand.addListener(async (command) => {
  if (command !== 'download-with-superidm') return;
  const [tab] = await chrome.tabs.query({ active: true, currentWindow: true });
  if (!tab) return;

  // Prefer a detected stream, then the page itself.
  await pushMedia(tab.id, true);
  const items = tabMedia(tab.id);
  try {
    if (items.length) {
      const best = items.find((m) => m.kind === 'hls') || items[0];
      if (best.id) {
        await api('/api/sniff/download', { method: 'POST', body: JSON.stringify({ id: best.id }) });
      } else {
        await sendDownload(best.url, { pageUrl: tab.url, kind: best.kind, silent: false });
      }
      notify('SuperIDM', 'Downloading the main media on this page.');
    } else {
      await sendDownload(tab.url, { pageUrl: tab.url, fileName: suggestPageName(tab), silent: false });
      notify('SuperIDM', 'Page sent to SuperIDM.');
    }
  } catch (err) {
    notify('SuperIDM error', err.message);
  }
});

/* ------------------------------------------------ lightweight job updates */

// Poll only while the popup is open: the popup asks for status itself, so all
// the worker needs to do is keep the badge text fresh when media appears.
chrome.tabs.onActivated.addListener(({ tabId }) => {
  updateBadge(tabId);
  pushMedia(tabId).catch(() => {});
});

buildMenus();
