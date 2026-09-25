/*
 * SuperIDM Integration - content script.
 *
 * Injected into every frame. It does three things:
 *   1. Reports media it can see in the DOM (video/audio/source/embed) and in
 *      the performance timeline, so the extension can offer it.
 *   2. Shows a small "SuperIDM" pill over a playing video or audio element after
 *      a short hover - the same affordance Internet Download Manager users
 *      expect.
 *   3. Answers link-collection requests for "download all links on this page".
 */

(() => {
  'use strict';

  if (window.__superidmInjected) return;
  window.__superidmInjected = true;

  const MEDIA_EXT = /\.(m3u8|mpd|mp4|webm|mkv|mov|m4v|m4s|ts|flv|ogv|mp3|m4a|aac|flac|ogg|opus|wav)(\?|#|$)/i;
  const FILE_EXT = /\.(zip|rar|7z|tar|gz|tgz|bz2|xz|zst|iso|exe|msi|apk|dmg|deb|rpm|pdf|docx?|xlsx?|pptx?|epub|mobi|csv|txt|rtf|svg|png|jpe?g|gif|webp|bmp|ico|psd|jar|crx|xpi|mp3|mp4|mkv|avi|mov|flac|wav)(\?|#|$)/i;

  const absolute = (u) => {
    try {
      return new URL(u, location.href).href;
    } catch {
      return '';
    }
  };

  const classify = (url, mime = '') => {
    const m = mime.toLowerCase();
    if (m.includes('mpegurl')) return 'hls';
    if (m.includes('dash')) return 'dash';
    if (m.startsWith('video/')) return 'video';
    if (m.startsWith('audio/')) return 'audio';
    const u = url.toLowerCase();
    if (/\.m3u8(\?|#|$)/.test(u)) return 'hls';
    if (/\.mpd(\?|#|$)/.test(u)) return 'dash';
    if (/\.(mp4|webm|mkv|mov|m4v|m4s|ts|flv|ogv)(\?|#|$)/.test(u)) return 'video';
    if (/\.(mp3|m4a|aac|flac|ogg|opus|wav)(\?|#|$)/.test(u)) return 'audio';
    return 'file';
  };

  const pageTitle = () => (document.title || location.hostname).trim().slice(0, 150);

  /* -------------------------------------------------- media discovery */

  /** Media visible in the DOM right now. */
  function scanDOM() {
    const items = [];
    const push = (rawUrl, extra = {}) => {
      const url = absolute(rawUrl);
      if (!url || !/^https?:/i.test(url)) return;
      items.push({
        url,
        kind: classify(url, extra.mime),
        pageUrl: location.href,
        title: pageTitle(),
        ...extra,
      });
    };

    document.querySelectorAll('video, audio').forEach((el) => {
      const mime = '';
      if (el.currentSrc) push(el.currentSrc, { mime, duration: el.duration || 0, width: el.videoWidth || 0, height: el.videoHeight || 0 });
      if (el.src) push(el.src, { mime, width: el.videoWidth || 0, height: el.videoHeight || 0 });
      el.querySelectorAll('source').forEach((s) => push(s.src, { mime: s.type || '' }));
      el.querySelectorAll('track').forEach((t) => push(t.src));
    });

    document.querySelectorAll('source[src]').forEach((s) => push(s.src, { mime: s.type || '' }));

    document.querySelectorAll('a[href]').forEach((a) => {
      const href = a.getAttribute('href') || '';
      if (MEDIA_EXT.test(href)) push(href);
    });

    // Frames that hold a player: ask them too (same-origin only).
    document.querySelectorAll('iframe[src]').forEach((f) => {
      if (/\.(m3u8|mpd)(\?|#|$)/i.test(f.src)) push(f.src);
    });

    return items;
  }

  /** Everything the browser fetched during this page view. */
  function scanPerformance() {
    const items = [];
    try {
      for (const entry of performance.getEntriesByType('resource')) {
        if (!entry.name || !/^https?:/i.test(entry.name)) continue;
        if (!MEDIA_EXT.test(entry.name)) continue;
        items.push({
          url: entry.name,
          kind: classify(entry.name),
          pageUrl: location.href,
          title: pageTitle(),
          size: Math.max(0, Math.round(entry.transferSize || entry.decodedBodySize || 0)),
        });
      }
    } catch { /* performance API unavailable */ }
    return items;
  }

  function allMedia() {
    const seen = new Set();
    const out = [];
    for (const item of [...scanDOM(), ...scanPerformance()]) {
      if (seen.has(item.url)) continue;
      seen.add(item.url);
      out.push(item);
    }
    return out;
  }

  function reportMedia(force = false) {
    const items = allMedia();
    if (!items.length) return 0;
    try {
      chrome.runtime.sendMessage({ type: 'superidm:pageMedia', items }, () => void chrome.runtime.lastError);
    } catch { /* extension context invalidated */ }
    return items.length;
  }

  /* --------------------------------------------------- link collection */

  function collectLinks(limit = 100) {
    const seen = new Set();
    const out = [];
    document.querySelectorAll('a[href]').forEach((a) => {
      const href = absolute(a.getAttribute('href') || '');
      if (!href || !/^https?:/i.test(href) || seen.has(href)) return;
      const looksDownloadable = FILE_EXT.test(href) || a.hasAttribute('download');
      if (!looksDownloadable) return;
      seen.add(href);
      let name = '';
      try {
        name = decodeURIComponent(new URL(href).pathname.split('/').pop() || '');
      } catch { /* keep empty */ }
      out.push({ url: href, fileName: name });
    });
    return out.slice(0, limit);
  }

  /* ------------------------------------------------------ floating pill */

  const PILL_ID = 'superidm-media-pill';
  let pill = null;
  let pillTarget = null;
  let hoverTimer = null;

  function removePill() {
    if (pill) {
      pill.remove();
      pill = null;
    }
    pillTarget = null;
  }

  function buildPill() {
    const el = document.createElement('div');
    el.id = PILL_ID;
    el.setAttribute('role', 'button');
    el.setAttribute('tabindex', '0');
    el.innerHTML = `
      <img src="${chrome.runtime.getURL('icons/icon32.png')}" alt="">
      <span>Download with SuperIDM</span>`;
    el.addEventListener('click', (ev) => {
      ev.preventDefault();
      ev.stopPropagation();
      handlePillClick();
    });
    el.addEventListener('mouseenter', () => clearTimeout(hoverTimer));
    el.addEventListener('mouseleave', removePill);
    return el;
  }

  function positionPill(el) {
    const r = el.getBoundingClientRect();
    if (r.width < 80 || r.height < 40) return false;
    pill.style.top = `${Math.max(8, r.top + 10)}px`;
    pill.style.left = `${Math.max(8, r.left + 10)}px`;
    return true;
  }

  async function handlePillClick() {
    if (!pillTarget) return;
    const target = pillTarget;
    let payload = { url: target.url, kind: target.kind, title: pageTitle(), pageUrl: location.href };

    // Prefer a captured stream: for HLS pages, the <video> src is often a blob,
    // while the real playlist is known to the background worker.
    try {
      const res = await chrome.runtime.sendMessage({
        type: 'superidm:pageMedia',
        items: allMedia(),
      });
      void res;
    } catch { /* ignore */ }

    try {
      chrome.runtime.sendMessage({ type: 'superidm:download', ...payload }, (resp) => {
        const err = chrome.runtime.lastError;
        const ok = !err && resp && resp.ok;
        showToast(ok ? 'Added to SuperIDM' : `SuperIDM: ${(err && err.message) || (resp && resp.error) || 'failed'}`);
      });
    } catch {
      showToast('SuperIDM is not reachable');
    }
    removePill();
  }

  let hoverInFlight = false;
  document.addEventListener('mouseover', (ev) => {
    const el = ev.target;
    if (!el || !(el instanceof Element)) return;
    if (el.id === PILL_ID) return;

    const media = el.closest('video, audio');
    if (!media) return;

    clearTimeout(hoverTimer);
    hoverTimer = setTimeout(async () => {
      if (hoverInFlight || pill) return;
      const cfg = await getConfigSafe();
      if (!cfg.showFloatingButton) return;

      const best = pickBestMedia(media);
      if (!best) return;

      hoverInFlight = true;
      pill = buildPill();
      pillTarget = best;
      document.documentElement.appendChild(pill);
      if (!positionPill(media)) removePill();
      hoverInFlight = false;
    }, 550);
  }, true);

  document.addEventListener('mouseout', (ev) => {
    const el = ev.target;
    if (!el || !(el instanceof Element)) return;
    if (!el.closest('video, audio')) {
      clearTimeout(hoverTimer);
    }
  }, true);

  document.addEventListener('scroll', () => {
    if (pill && pillTarget && pillTarget.el) positionPill(pillTarget.el);
  }, { passive: true });

  function pickBestMedia(el) {
    const list = allMedia();
    const streams = list.filter((m) => m.kind === 'hls' || m.kind === 'dash');
    if (streams.length) return { ...streams[0], el };
    const fromList = list.find((m) => m.kind === 'video' || m.kind === 'audio');
    if (fromList) return { ...fromList, el };
    if (el.currentSrc || el.src) {
      const url = absolute(el.currentSrc || el.src);
      if (url) {
        return {
          url,
          kind: classify(url, ''),
          title: pageTitle(),
          pageUrl: location.href,
          el,
        };
      }
    }
    return null;
  }

  let configCache = null;
  async function getConfigSafe() {
    if (configCache) return configCache;
    try {
      configCache = await chrome.runtime.sendMessage({ type: 'superidm:status' })
        .then((r) => (r && r.config) || { showFloatingButton: true });
    } catch {
      configCache = { showFloatingButton: true };
    }
    setTimeout(() => { configCache = null; }, 30000);
    return configCache;
  }

  /* ------------------------------------------------------------- toast */

  let toastEl = null;
  function showToast(text) {
    if (toastEl) toastEl.remove();
    toastEl = document.createElement('div');
    toastEl.id = 'superidm-toast';
    toastEl.textContent = text;
    document.documentElement.appendChild(toastEl);
    setTimeout(() => {
      if (toastEl) {
        toastEl.remove();
        toastEl = null;
      }
    }, 3200);
  }

  /* ------------------------------------------------------- messages in */

  chrome.runtime.onMessage.addListener((msg, sender, sendResponse) => {
    switch (msg && msg.type) {
      case 'superidm:scanMedia':
        sendResponse({ items: allMedia() });
        return true;
      case 'superidm:collectLinks':
        sendResponse({ links: collectLinks(msg.limit || 100) });
        return true;
      case 'superidm:toast':
        showToast(msg.text || '');
        sendResponse({ ok: true });
        return true;
      default:
        return false;
    }
  });

  /* ------------------------------------------------------ first report */

  const initialReport = () => reportMedia();
  if (document.readyState === 'complete') {
    setTimeout(initialReport, 800);
  } else {
    window.addEventListener('load', () => setTimeout(initialReport, 800));
  }

  // Players often attach their source late; keep watching for a while.
  let ticks = 0;
  const watcher = setInterval(() => {
    ticks++;
    reportMedia();
    if (ticks > 10) clearInterval(watcher);
  }, 2500);

  // SPA navigation.
  let lastHref = location.href;
  setInterval(() => {
    if (location.href !== lastHref) {
      lastHref = location.href;
      removePill();
      reportMedia();
    }
  }, 1500);
})();
