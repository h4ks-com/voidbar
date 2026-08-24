// Voidbar client loader.
//
// Downloads a frozen Discord web client build from a configured CDN mirror
// (e.g. archive.org), patches it in-memory so every API / gateway / asset
// request is redirected to this Voidbar instance, and boots it.
//
// Voidbar itself never stores or serves Discord assets; only this original
// loader code is served by the instance.

const ui = {
  status: document.getElementById('voidbar-loading-status'),
  bar: document.getElementById('voidbar-loading-bar'),
  barInner: document.getElementById('voidbar-loading-bar-inner'),
  error: document.getElementById('voidbar-loading-error'),
};

function setStatus(text) {
  if (ui.status) ui.status.textContent = text;
}

function setProgress(current, total) {
  if (!ui.bar || !ui.barInner) return;
  if (total <= 0) {
    ui.bar.classList.remove('active');
    return;
  }
  ui.bar.classList.add('active');
  ui.barInner.style.width = `${Math.min(100, (current / total) * 100)}%`;
}

function fatal(message) {
  setStatus('ERROR');
  if (ui.error) {
    ui.error.textContent = message;
    ui.error.classList.add('visible');
  }
  console.error('[voidbar]', message);
}

const log = (...args) => console.log('[voidbar]', ...args);

// ---------------------------------------------------------------------------
// Configuration

async function fetchConfig() {
  const res = await fetch('/voidbar/config', { cache: 'no-store' });
  if (!res.ok) {
    const body = await res.text().catch(() => '');
    throw new Error(`instance config unavailable (HTTP ${res.status}) ${body}`);
  }
  return res.json();
}

function buildGlobalEnv(cfg) {
  const host = `${location.protocol}//${location.host}`;
  return {
    API_ENDPOINT: `${host}/api`,
    API_VERSION: 9,
    GATEWAY_ENDPOINT: cfg.gateway,
    WEBAPP_ENDPOINT: host,
    CDN_HOST: host,
    ASSET_ENDPOINT: cfg.cdn_base,
    MEDIA_PROXY_ENDPOINT: host,
    WIDGET_ENDPOINT: '',
    INVITE_HOST: location.host,
    GUILD_TEMPLATE_HOST: location.host,
    GIFT_CODE_HOST: `${location.host}/gifts`,
    RELEASE_CHANNEL: 'stable',
    MARKETING_ENDPOINT: '',
    BRAINTREE_KEY: '',
    STRIPE_KEY: '',
    NETWORKING_ENDPOINT: '',
    RTC_LATENCY_ENDPOINT: '',
    ACTIVITY_APPLICATION_HOST: '',
    PROJECT_ENV: 'production',
    REMOTE_AUTH_ENDPOINT: '',
    SENTRY_TAGS: { buildId: 'voidbar', buildType: '' },
    MIGRATION_SOURCE_ORIGIN: '',
    MIGRATION_DESTINATION_ORIGIN: '',
    HTML_TIMESTAMP: Date.now(),
    ALGOLIA_KEY: '',
  };
}

// ---------------------------------------------------------------------------
// Patching

function patchJS(text, cfg) {
  const host = location.host;

  // Kill telemetry transport before it can phone home.
  text = text.replaceAll('sentry.io', '0.0.0.0');
  text = text.replace(/track:function\([^)]*\){/, '$&return;');
  text = text.replace(/t\.analyticsTrackingStoreMaker=function\(e\){/, 't.analyticsTrackingStoreMaker=function(e){return;');

  // Redirect every hardcoded Discord origin to this instance. The client
  // should prefer GLOBAL_ENV, but plenty of code paths still use literals.
  text = text.replaceAll('status.discordapp.com', host);
  text = text.replaceAll('cdn.discordapp.com', host);
  text = text.replaceAll('discordapp.com', host);
  text = text.replaceAll('discord.gg', host);
  text = text.replaceAll(/([a-z]+\.)?discord\.com/g, host);
  text = text.replaceAll(/([a-z]+\.)?discord\.media/g, host);

  // Webpack asset manifest base: point media/asset lookups at the CDN.
  text = text.replaceAll(/e\.exports=n\.p/g, `e.exports="${cfg.cdn_base}/assets/"`);

  // When the instance itself is plain http (local dev), downgrade embedded
  // https URLs so browsers do not block mixed content.
  if (location.protocol !== 'https:') {
    text = text.replaceAll('https://', `${location.protocol}//`);
  }

  return text;
}

function patchCSS(text, cfg) {
  text = text.replaceAll(/url\(\/assets\//g, `url(${cfg.cdn_base}/assets/`);
  text = text.replaceAll('d3dsisomax34re.cloudfront.net', location.host);
  return text;
}

// ---------------------------------------------------------------------------
// OPFS asset cache
//
// After a resource is fetched and patched it is persisted in Origin Private
// File System; subsequent boots load from the local cache without touching
// the mirror. Cache key includes the mirror, build and instance origin, so
// switching any of them starts a fresh cache.

const CACHE_VERSION = 'v1';

function hashString(s) {
  let h = 2166136261;
  for (let i = 0; i < s.length; i++) {
    h ^= s.charCodeAt(i);
    h = Math.imul(h, 16777619);
  }
  return (h >>> 0).toString(36);
}

const opfs = {
  root: null,
  ready: false,

  async init() {
    if (!navigator.storage?.getDirectory) {
      log('OPFS not supported, every boot will hit the mirror');
      return;
    }
    try {
      const key = hashString(
        `${CACHE_VERSION}|${state.cfg.cdn_base}|${state.cfg.build}|${location.host}`,
      );
      let dir = await navigator.storage.getDirectory();
      for (const part of ['voidbar', CACHE_VERSION, key]) {
        dir = await dir.getDirectoryHandle(part, { create: true });
      }
      this.root = dir;
      this.ready = true;
      navigator.storage.persist?.().catch(() => {});
    } catch (e) {
      log('OPFS unavailable, caching disabled:', e.message);
    }
  },

  async walk(path, create = false) {
    const parts = path
      .replace(/^\//, '')
      .split('/')
      .filter((p) => p && p !== '..' && p !== '.');
    const name = parts.pop();
    let dir = this.root;
    for (const part of parts) {
      dir = await dir.getDirectoryHandle(part, { create });
    }
    return { dir, name };
  },

  async read(path) {
    if (!this.ready) return null;
    try {
      const { dir, name } = await this.walk(path);
      const handle = await dir.getFileHandle(name);
      return await (await handle.getFile()).text();
    } catch {
      return null;
    }
  },

  async write(path, text) {
    if (!this.ready) return;
    try {
      const { dir, name } = await this.walk(path, true);
      const handle = await dir.getFileHandle(name, { create: true });
      const writable = await handle.createWritable();
      await writable.write(text);
      await writable.close();
    } catch (e) {
      log('OPFS write skipped:', path, e.message);
    }
  },
};

// ---------------------------------------------------------------------------
// Resource loading

const cache = new Map(); // normalized path -> { url, blob }

function normalizePath(path) {
  if (path.startsWith('http')) return new URL(path).pathname;
  return path.startsWith('/') ? path : `/assets/${path}`;
}

async function fetchCDN(path, opts) {
  const url = `${state.cfg.cdn_base}${path}`;
  const res = await fetch(url, opts);
  if (res.status === 404 && path.startsWith('/assets/')) {
    // Mirrors use slightly different layouts; retry without the prefix.
    return fetch(`${state.cfg.cdn_base}${path.slice('/assets/'.length)}`, opts);
  }
  return res;
}

async function loadResource(path, type) {
  const normalized = normalizePath(path);
  if (cache.has(normalized)) return cache.get(normalized);

  let body = await opfs.read(normalized);

  if (body == null) {
    const res = await fetchCDN(normalized);
    if (!res.ok) throw new Error(`HTTP ${res.status} for ${normalized}`);

    const content = await res.text();
    body = type === 'script' ? patchJS(content, state.cfg) : patchCSS(content, state.cfg);

    if (type === 'script') {
      await preloadChunks(content).catch((e) => log('chunk preload failed:', e));
    }
    opfs.write(normalized, body); // fire-and-forget persistence
  }

  const sourceURL = `${location.protocol}//${location.host}${normalized}`;
  const withSourceURL =
    type === 'script'
      ? `${body}\n//# sourceURL=${sourceURL}`
      : `${body}\n/*# sourceURL=${sourceURL} */`;

  const blob = URL.createObjectURL(
    new Blob([withSourceURL], { type: type === 'script' ? 'application/javascript' : 'text/css' }),
  );

  const entry = { url: normalized, blob };
  cache.set(normalized, entry);
  return entry;
}

// ---------------------------------------------------------------------------
// Webpack chunk discovery and preloading

const CHUNK_MAP_RE = /[{,]\s*(?:"|')?(\d+)(?:"|')?\s*:\s*(?:"|')([0-9a-zA-Z._-]{10,})(?:"|')/g;

function extractChunkVariants(content) {
  const byHash = new Map();
  let match;
  while ((match = CHUNK_MAP_RE.exec(content)) !== null) {
    const hash = match[2];
    if (!byHash.has(hash)) {
      byHash.set(hash, [`/assets/${match[1]}.${hash}.js`, `/assets/${hash}.js`, `/assets/${hash}.css`]);
    }
  }
  return byHash;
}

async function resolveChunk(variants) {
  for (const path of variants) {
    try {
      const res = await fetchCDN(path, { method: 'HEAD' });
      if (res.ok) return path;
    } catch {
      // try next variant
    }
  }
  return null;
}

async function preloadChunks(content) {
  const variants = extractChunkVariants(content);
  if (variants.size === 0) return;

  log(`discovered ${variants.size} chunk candidates`);
  setStatus(`RESOLVING CHUNKS (0/${variants.size})`);

  const jobs = [...variants.entries()].map(async ([, candidate], index) => {
    const path = await resolveChunk(candidate);
    setStatus(`RESOLVING CHUNKS (${index + 1}/${variants.size})`);
    if (!path) return;
    if (cache.has(normalizePath(path))) return;
    await loadResource(path, path.endsWith('.css') ? 'style' : 'script').catch(() => {});
  });

  await Promise.all(jobs);
}

// ---------------------------------------------------------------------------
// DOM assembly and script execution

function parseAppHtml(html) {
  // The stock client inlines its own GLOBAL_ENV; ours must win, so drop it.
  html = html.replace(/<script(\s[^>]*)?>\s*window\.GLOBAL_ENV\s*=[\s\S]*?<\/script>/g, '');

  const doc = new DOMParser().parseFromString(html, 'text/html');
  return { head: doc.head, body: doc.body };
}

function collectResources(head, body) {
  const combined = head.innerHTML + body.innerHTML;
  const urls = (re) =>
    [...combined.matchAll(re)]
      .map((m) => m[1])
      .filter((u) => u && u.startsWith('/') && !u.startsWith('/cdn-cgi/'));

  const links = [...combined.matchAll(/<link[^>]+>/g)].map((m) => m[0]);
  const styles = [];
  const scripts = [];
  for (const tag of links) {
    const href = tag.match(/href="([^"]+)"/)?.[1];
    if (!href || !href.startsWith('/')) continue;
    if (/rel="[^"]*stylesheet[^"]*"/.test(tag)) styles.push(href);
    if (/as="script"/.test(tag)) scripts.push(href);
  }
  // De-duplicate while keeping order.
  const seen = new Set();
  const dedupe = (arr) => arr.filter((u) => !seen.has(u) && seen.add(u));

  styles.push(...urls(/<link[^>]+rel="[^"]*stylesheet[^"]*"[^>]*>/g).filter((u) => !u.endsWith('.ico')));
  scripts.push(...urls(/<script[^>]+src="([^"]+)"[^>]*>/g));

  return { styles: dedupe(styles), scripts: dedupe(scripts) };
}

async function loadAll(resources) {
  const total = resources.styles.length + resources.scripts.length;
  let done = 0;
  setProgress(0, total);
  setStatus(`DOWNLOADING CLIENT (0/${total})`);

  const tryLoad = async (path, type) => {
    try {
      return await loadResource(path, type);
    } catch (e) {
      // Missing optional resources (analytics, some chunks) must not kill boot.
      log('skipping resource:', path, e.message);
      return null;
    } finally {
      done++;
      setProgress(done, total);
      setStatus(`DOWNLOADING CLIENT (${done}/${total})`);
    }
  };

  const styles = (
    await Promise.all(resources.styles.map((path) => tryLoad(path, 'style')))
  ).filter(Boolean);

  const scripts = [];
  for (const path of resources.scripts) {
    const entry = await tryLoad(path, 'script');
    if (entry) scripts.push(entry);
  }
  if (scripts.length === 0) throw new Error('all client scripts failed to load');
  return { styles, scripts };
}

function injectDOM(doc, styles, scripts) {
  const styleMap = new Map(styles.map((s) => [s.url, s.blob]));
  const scriptMap = new Map(scripts.map((s) => [s.url, s.blob]));

  const rewrite = (elem) => {
    if (elem.hasAttribute('integrity')) elem.removeAttribute('integrity');
    if (elem.tagName === 'SCRIPT' && elem.hasAttribute('src')) {
      const blob = scriptMap.get(normalizePath(elem.getAttribute('src')));
      if (blob) elem.setAttribute('src', blob);
    }
    if (elem.tagName === 'LINK') {
      const blob = styleMap.get(normalizePath(elem.getAttribute('href')));
      if (blob) elem.setAttribute('href', blob);
    }
    return elem;
  };

  for (const node of [...doc.head.childNodes]) {
    if (node.nodeType === Node.ELEMENT_NODE) document.head.appendChild(rewrite(node));
  }
  for (const node of [...doc.body.childNodes]) {
    if (node.nodeType === Node.ELEMENT_NODE) document.body.appendChild(rewrite(node));
  }
}

async function executeScripts() {
  // Re-create every blob script element so they execute in order, exactly once.
  const elements = [...document.getElementsByTagName('script')].filter((s) => s.src.startsWith('blob:'));
  for (const elem of elements) {
    await new Promise((resolve, reject) => {
      const fresh = document.createElement('script');
      for (const attr of elem.attributes) fresh.setAttribute(attr.name, attr.value);
      fresh.onload = resolve;
      fresh.onerror = () => reject(new Error(`failed to execute ${elem.src}`));
      elem.replaceWith(fresh);
    });
  }
}

function waitForMount() {
  return new Promise((resolve) => {
    const timer = setInterval(() => {
      const mount = document.getElementById('app-mount');
      if (mount && mount.children.length > 0) {
        clearInterval(timer);
        resolve();
      }
    }, 100);
  });
}

// ---------------------------------------------------------------------------
// Dynamic chunk interception

function installInterceptor() {
  const skip = (url) =>
    typeof url !== 'string' || url.includes('/voidbar/') || url.startsWith('blob:');

  const originalCreate = document.createElement.bind(document);
  document.createElement = (tagName) => {
    const elem = originalCreate(tagName);
    const lower = tagName.toLowerCase();

    if (lower === 'script') {
      let blobURL = null;
      Object.defineProperty(elem, 'src', {
        get() {
          return elem.getAttribute('src') ?? '';
        },
        set(url) {
          if (blobURL) URL.revokeObjectURL(blobURL);
          if (skip(url)) {
            elem.setAttribute('src', url);
            return;
          }
          const normalized = normalizePath(url);
          const entry = cache.get(normalized);
          if (entry) {
            elem.setAttribute('src', entry.blob);
            blobURL = entry.blob;
          } else {
            log('uncached dynamic chunk, loading:', normalized);
            loadResource(normalized, 'script')
              .then((e) => elem.setAttribute('src', e.blob))
              .catch(() => elem.setAttribute('src', `${state.cfg.cdn_base}${normalized}`));
          }
        },
        configurable: true,
      });
    }

    if (lower === 'link') {
      Object.defineProperty(elem, 'href', {
        get() {
          return elem.getAttribute('href') ?? '';
        },
        set(url) {
          if (skip(url) || !url.endsWith('.css')) {
            elem.setAttribute('href', url);
            return;
          }
          const entry = cache.get(normalizePath(url));
          elem.setAttribute('href', entry ? entry.blob : url);
        },
        configurable: true,
      });
    }

    return elem;
  };
}

// ---------------------------------------------------------------------------
// Boot

const state = { cfg: null };

async function boot() {
  setStatus('FETCHING CONFIG');
  state.cfg = await fetchConfig();

  window.GLOBAL_ENV = buildGlobalEnv(state.cfg);
  document.title = state.cfg.instance_name || 'Voidbar';
  log('instance:', state.cfg.instance_name, 'build:', state.cfg.build);

  await opfs.init();

  setStatus('DOWNLOADING CLIENT');
  const htmlPath = `/${state.cfg.build ? state.cfg.build + '/' : ''}${state.cfg.html || 'app.html'}`;
  const res = await fetchCDN(htmlPath);
  if (!res.ok)
    throw new Error(
      `client HTML HTTP ${res.status} for ${state.cfg.cdn_base}${htmlPath}`,
    );

  const doc = parseAppHtml(await res.text());
  const resources = collectResources(doc.head, doc.body);
  if (resources.scripts.length === 0) throw new Error('no scripts found in client HTML');

  const { styles, scripts } = await loadAll(resources);

  installInterceptor();
  injectDOM(doc, styles, scripts);

  setStatus('STARTING');
  await executeScripts();
  await waitForMount();

  setProgress(0, 1);
  setStatus('READY');
  document.getElementById('voidbar-loading')?.remove();
  log('client mounted');
}

boot().catch((e) => fatal(e.message || String(e)));
