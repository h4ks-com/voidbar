// Voidbar client loader.
//
// Downloads a frozen Discord web client build from a configured CDN mirror
// (e.g. an archive.org item), patches it in-memory so every API / gateway /
// asset request is redirected to this Voidbar instance, and boots it.
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
  // Endpoint formats MUST match the original client GLOBAL_ENV exactly:
  // the bundles prepend location.protocol to API_ENDPOINT /
  // MEDIA_PROXY_ENDPOINT / ASSET_ENDPOINT (expecting "//host" values) and
  // location.protocol + "//" + CDN_HOST (expecting a bare host). Absolute
  // URLs here produce mangled requests like GET /http:/host/api/....
  const rel = (u) => u.replace(/^https?:/, '');
  return {
    API_ENDPOINT: rel(`${location.origin}/api`),
    API_VERSION: 9,
    GATEWAY_ENDPOINT: cfg.gateway,
    WEBAPP_ENDPOINT: rel(location.origin),
    CDN_HOST: location.host,
    ASSET_ENDPOINT: rel(assetBase()),
    MEDIA_PROXY_ENDPOINT: rel(location.origin),
    WIDGET_ENDPOINT: rel(`${location.origin}/widget`),
    INVITE_HOST: location.host,
    GUILD_TEMPLATE_HOST: location.host,
    GIFT_CODE_HOST: `${location.host}/gifts`,
    RELEASE_CHANNEL: 'stable',
    MARKETING_ENDPOINT: rel(location.origin),
    BRAINTREE_KEY: '',
    STRIPE_KEY: '',
    // Empty strings are dangerous here: the client concatenates
    // "wss:"+REMOTE_AUTH_ENDPOINT+"/?v=1" and location.protocol+
    // RTC_LATENCY_ENDPOINT / NETWORKING_ENDPOINT, and an empty value
    // produces invalid URLs that crash React rendering (QR login screen).
    NETWORKING_ENDPOINT: rel(location.origin),
    RTC_LATENCY_ENDPOINT: rel(`${location.origin}/rtc`),
    ACTIVITY_APPLICATION_HOST: '',
    PROJECT_ENV: 'production',
    REMOTE_AUTH_ENDPOINT: rel(`${location.origin}/remote-auth`),
    SENTRY_TAGS: { buildId: 'voidbar', buildType: '' },
    MIGRATION_SOURCE_ORIGIN: '',
    MIGRATION_DESTINATION_ORIGIN: '',
    HTML_TIMESTAMP: Date.now(),
    ALGOLIA_KEY: '',
  };
}

// All asset fetches and patched asset references go through either the
// configured mirror directly, or - when the instance runs its opt-in CDN
// proxy - through same-origin paths (no CORS involved at all).
function assetBase() {
  return state.cfg.proxy_base
    ? `${location.origin}${state.cfg.proxy_base}`
    : state.cfg.cdn_base;
}

// ---------------------------------------------------------------------------
// Patching

function patchJS(text, cfg) {
  const host = location.host;

  // --- client diagnostics: null-safe store comparator -------------------
  // The client's shallow-equal comparator does Object.keys() on both args
  // with no null guard; a store selector returning undefined during a
  // dispatch crashes the whole gateway connection. Make it null-safe and
  // log the offending values instead of crashing.
  text = text.replaceAll(
    't.default=function(e,t,n){if(e===t)return!0;var r=Object.keys(e),i=Object.keys(t);',
    't.default=function(e,t,n){if(e===t)return!0;if(null==e||null==t){console.warn("[voidbar] shallowEqual null:",e,t);return!1}var r=Object.keys(e),i=Object.keys(t);',
  );
  // Log which component selector returns undefined (useStateFromStores).
  text = text.replaceAll(
    'var e=u.current();if(!i(f.current,e)){f.current=e;p({})}',
    'var e=u.current();if(null==e)console.warn("[voidbar] selector undefined:",String(u.current).slice(0,400));if(!i(f.current,e)){f.current=e;p({})}',
  );

  // Kill Sentry at the source: replace the DSN *contents* with an empty
  // string (the quotes stay from the original literal), which the SDK
  // treats as "disabled". Never inject quotes here - the DSN appears as
  // "https://key@sentry.io/id" in the source, and substituting '""' for
  // the URL would produce """" and a SyntaxError.
  text = text.replace(/https?:\/\/[0-9a-f]{16,}@sentry\.io\/\d+/g, '');

  // Kill telemetry transport before it can phone home.
  text = text.replaceAll('sentry.io', '0.0.0.0');
  text = text.replace(/track:function\([^)]*\){/, '$&return;');
  text = text.replace(
    /t\.analyticsTrackingStoreMaker=function\(e\){/,
    't.analyticsTrackingStoreMaker=function(e){return;',
  );

  // Redirect every hardcoded Discord origin to this instance. The client
  // should prefer GLOBAL_ENV, but plenty of code paths still use literals.
  text = text.replaceAll('status.discordapp.com', host);
  text = text.replaceAll('cdn.discordapp.com', host);
  text = text.replaceAll('discordapp.com', host);
  text = text.replaceAll('discord.gg', host);
  text = text.replaceAll(/([a-z]+\.)?discord\.com/g, host);
  text = text.replaceAll(/([a-z]+\.)?discord\.media/g, host);

  // Webpack asset manifest base: point media/asset lookups at the CDN (or
  // the instance's opt-in proxy).
  text = text.replaceAll(/e\.exports=n\.p/g, `e.exports="${assetBase()}/assets/"`);

  // When the instance itself is plain http (local dev), downgrade embedded
  // https URLs so browsers do not block mixed content.
  if (location.protocol !== 'https:') {
    text = text.replaceAll('https://', `${location.protocol}//`);
  }

  return text;
}

function patchCSS(text, cfg) {
  text = text.replaceAll(/url\(\/assets\//g, `url(${assetBase()}/assets/`);
  text = text.replaceAll('d3dsisomax34re.cloudfront.net', location.host);
  return text;
}

// ---------------------------------------------------------------------------
// OPFS asset cache
//
// Stores the RAW (unpatched) upstream bytes; patchJS/patchCSS are applied in
// memory at read time. This means loader patch changes never invalidate the
// cache - a new patcher version just re-patches instantly. The cache key
// covers the mirror, build and instance origin; switching any of them starts
// a fresh cache. v2 = raw-content format (v1 stored patched bytes).

const CACHE_VERSION = 'v2';

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
        `${CACHE_VERSION}|${state.cfg.proxy_base ? 'proxy' : 'direct'}|${state.cfg.cdn_base}|${state.cfg.build}|${location.host}`,
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
// Fetching

const FETCH_TIMEOUT_MS = 60_000;
const MAX_RETRIES = 5;
const POOL_SIZE = 4;

function sleep(ms) {
  return new Promise((resolve) => setTimeout(resolve, ms));
}

async function fetchRaw(url, opts = {}) {
  const controller = new AbortController();
  const timer = setTimeout(() => controller.abort(), FETCH_TIMEOUT_MS);
  try {
    return await fetch(url, { ...opts, signal: controller.signal });
  } finally {
    clearTimeout(timer);
  }
}

// Fetch with retry & exponential backoff. Mirrors rate-limit (429) and shed
// load (5xx, refused streams), so patience is required on the first boot.
// Returns the Response: 404 is returned as-is, everything else is retried.
async function fetchCDN(path, opts = {}) {
  const url = `${assetBase()}${path}`;
  let backoff = 1000;
  let lastError = null;
  for (let attempt = 0; attempt <= MAX_RETRIES; attempt++) {
    if (attempt > 0) {
      await sleep(backoff + Math.floor(Math.random() * 400));
      backoff = Math.min(backoff * 2, 30_000);
    }
    let res;
    try {
      res = await fetchRaw(url, opts);
    } catch (e) {
      lastError = e;
      continue; // network error / timeout / refused stream: retry
    }
    if (res.status === 404 && path.startsWith('/assets/')) {
      // Mirrors use slightly different layouts; retry without the prefix.
      return fetchCDN(path.slice('/assets/'.length), opts);
    }
    if (res.status === 429 || res.status >= 500) {
      const retryAfter = parseInt(res.headers.get('retry-after') || '', 10);
      await sleep(Number.isFinite(retryAfter) ? Math.min(retryAfter * 1000, 30_000) : backoff);
      lastError = new Error(`HTTP ${res.status}`);
      continue;
    }
    return res;
  }
  throw lastError || new Error(`fetch failed: ${path}`);
}

// ---------------------------------------------------------------------------
// Download scheduler
//
// A single work queue drained by a small worker pool. Script downloads
// enqueue webpack chunk groups they discover (chunks of chunks), so the
// queue stays bounded and the mirror sees at most POOL_SIZE concurrent
// requests.

const CHUNK_MAP_RE = /[{,]\s*(?:"|')?(\d+)(?:"|')?\s*:\s*(?:"|')([0-9a-zA-Z._-]{10,})(?:"|')/g;
const CSS_URL_RE = /url\((['"]?)(\/assets\/[^)'"]+)\1\)/g;

const cache = new Map(); // normalized path -> { url, blob }
const scheduler = {
  queue: [],
  inFlight: 0,
  seen: new Set(),
  missing: new Set(),
  doneCount: 0,

  get total() {
    return this.doneCount + this.queue.length + this.inFlight;
  },

  pump() {
    while (this.inFlight < POOL_SIZE && this.queue.length > 0) {
      const task = this.queue.shift();
      this.inFlight++;
      task()
        .catch((e) => log('task failed:', e.message))
        .finally(() => {
          this.inFlight--;
          this.doneCount++;
          setStatus(`DOWNLOADING CLIENT (${this.doneCount}/${this.total})`);
          setProgress(this.doneCount, this.total);
          this.pump();
        });
    }
  },

  enqueue(task) {
    this.queue.push(task);
    this.pump();
  },

  idle() {
    if (this.queue.length === 0 && this.inFlight === 0) return Promise.resolve();
    return new Promise((resolve) => {
      const check = () => {
        if (this.queue.length === 0 && this.inFlight === 0) resolve();
        else setTimeout(check, 100);
      };
      check();
    });
  },
};

function normalizePath(path) {
  if (path.startsWith('http')) return new URL(path).pathname;
  return path.startsWith('/') ? path : `/assets/${path}`;
}

function chunkVariants(content) {
  const byHash = new Map();
  let match;
  while ((match = CHUNK_MAP_RE.exec(content)) !== null) {
    const hash = match[2];
    if (!byHash.has(hash)) {
      byHash.set(hash, [
        `/assets/${match[1]}.${hash}.js`,
        `/assets/${hash}.js`,
        `/assets/${hash}.css`,
      ]);
    }
  }
  return byHash;
}

function assetURLs(content) {
  const out = new Set();
  let match;
  while ((match = CSS_URL_RE.exec(content)) !== null) {
    out.add(match[2]);
  }
  return out;
}

function makeBlob(normalized, body, type) {
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

// Download one file (with retry), persist the RAW bytes to OPFS, patch in
// memory, cache the blob, and enqueue discovered chunks/assets. Raw content
// is always available (from OPFS or the network), so chunk discovery also
// runs on resumed boots.
function enqueueFile(path, type) {
  const normalized = normalizePath(path);
  if (scheduler.seen.has(normalized)) return;
  scheduler.seen.add(normalized);
  scheduler.enqueue(async () => {
    let raw = await opfs.read(normalized);
    if (raw == null) {
      const res = await fetchCDN(normalized);
      if (!res.ok) {
        scheduler.missing.add(normalized);
        log('missing:', normalized);
        return;
      }
      raw = await res.text();
      opfs.write(normalized, raw); // fire-and-forget persistence of raw bytes
    }
    makeBlob(normalized, type === 'script' ? patchJS(raw, state.cfg) : patchCSS(raw, state.cfg), type);
    if (type === 'script') {
      for (const [hash, variants] of chunkVariants(raw)) enqueueChunk(hash, variants);
    }
    for (const asset of assetURLs(raw)) enqueueAsset(asset);
  });
}

function enqueueAsset(path) {
  const normalized = normalizePath(path);
  if (scheduler.seen.has(normalized)) return;
  scheduler.seen.add(normalized);
  scheduler.enqueue(async () => {
    const res = await fetchCDN(normalized);
    if (!res.ok) {
      scheduler.missing.add(normalized);
      return;
    }
    await res.arrayBuffer(); // warm browser HTTP cache; fetched by CSS at runtime
  });
}

// Webpack chunk: candidates are tried in order with GET (no HEAD round-trip;
// mirrors rate-limit hard). First non-404 wins.
function enqueueChunk(hash, variants) {
  const key = `chunk:${hash}`;
  if (scheduler.seen.has(key)) return;
  scheduler.seen.add(key);
  scheduler.enqueue(async () => {
    for (const candidate of variants) {
      const normalized = normalizePath(candidate);
      if (cache.has(normalized)) return;

      let raw = await opfs.read(normalized);
      if (raw == null) {
        const res = await fetchCDN(normalized);
        if (res.status === 404) continue; // try next variant
        if (!res.ok) {
          log('chunk fetch failed:', normalized, res.status);
          return;
        }
        raw = await res.text();
        opfs.write(normalized, raw);
      }
      const type = normalized.endsWith('.css') ? 'style' : 'script';
      makeBlob(normalized, type === 'script' ? patchJS(raw, state.cfg) : patchCSS(raw, state.cfg), type);
      if (type === 'script') {
        for (const [h, v] of chunkVariants(raw)) enqueueChunk(h, v);
        for (const asset of assetURLs(raw)) enqueueAsset(asset);
      }
      return;
    }
    log('chunk not found on mirror:', hash);
  });
}

// Load a resource outside of boot (dynamic chunk interception). Uses the
// same retry logic; OPFS (raw) and blob caches are consulted first.
async function loadResource(path, type) {
  const normalized = normalizePath(path);
  if (cache.has(normalized)) return cache.get(normalized);
  if (scheduler.missing.has(normalized)) throw new Error(`known missing: ${normalized}`);

  let raw = await opfs.read(normalized);
  if (raw == null) {
    const res = await fetchCDN(normalized);
    if (!res.ok) {
      if (res.status === 404) scheduler.missing.add(normalized);
      throw new Error(`HTTP ${res.status} for ${normalized}`);
    }
    raw = await res.text();
    opfs.write(normalized, raw);
  }
  return makeBlob(normalized, type === 'script' ? patchJS(raw, state.cfg) : patchCSS(raw, state.cfg), type);
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
  const styles = [];
  const scripts = [];
  const seen = new Set();
  const add = (list, v) => {
    if (v && v.startsWith('/') && !v.startsWith('/cdn-cgi/') && !seen.has(v)) {
      seen.add(v);
      list.push(v);
    }
  };

  for (const tag of combined.match(/<link[^>]+>/g) || []) {
    const href = tag.match(/href="([^"]+)"/)?.[1];
    if (/rel="[^"]*stylesheet[^"]*"/.test(tag)) add(styles, href);
    if (/as="script"/.test(tag)) add(scripts, href);
  }
  for (const m of combined.matchAll(/<script[^>]+src="([^"]+)"[^>]*>/g)) {
    add(scripts, m[1]);
  }
  return { styles, scripts };
}

async function loadAll(resources) {
  for (const path of resources.styles) enqueueFile(path, 'style');
  for (const path of resources.scripts) enqueueFile(path, 'script');
  await scheduler.idle();

  const styles = resources.styles
    .map((p) => cache.get(normalizePath(p)))
    .filter(Boolean);
  const scripts = resources.scripts
    .map((p) => cache.get(normalizePath(p)))
    .filter(Boolean);
  if (scripts.length === 0) throw new Error('all client scripts failed to load');
  return { styles, scripts };
}

function injectDOM(doc, styles, scripts) {
  const styleMap = new Map(styles.map((s) => [s.url, s.blob]));

  const rewrite = (elem) => {
    if (elem.hasAttribute('integrity')) elem.removeAttribute('integrity');
    if (elem.tagName === 'LINK') {
      const blob = styleMap.get(normalizePath(elem.getAttribute('href')));
      if (blob) elem.setAttribute('href', blob);
    }
    return elem;
  };

  // Inline scripts adopted from the parsed document never execute (per spec,
  // moving a script between documents does not start it), so they are
  // re-created here and run synchronously, in document order — defining the
  // globals (window.__OVERLAY__ etc.) the bundles rely on. External scripts
  // are NOT injected: executeScripts() creates them strictly in order later.
  const append = (target, node) => {
    if (node.tagName === 'SCRIPT') {
      if (node.hasAttribute('src')) return; // handled by executeScripts
      const fresh = document.createElement('script');
      for (const attr of node.attributes) {
        if (attr.name.toLowerCase() === 'integrity') continue;
        fresh.setAttribute(attr.name, attr.value);
      }
      fresh.textContent = node.textContent;
      target.appendChild(fresh);
      return;
    }
    target.appendChild(rewrite(node));
  };

  for (const node of [...doc.head.childNodes]) {
    if (node.nodeType === Node.ELEMENT_NODE) append(document.head, node);
  }
  for (const node of [...doc.body.childNodes]) {
    if (node.nodeType === Node.ELEMENT_NODE) append(document.body, node);
  }
}

async function executeScripts(entries) {
  // Create blob script elements sequentially so each bundle executes in the
  // order the build intended, after all inline globals are defined.
  for (const entry of entries) {
    await new Promise((resolve, reject) => {
      const fresh = document.createElement('script');
      fresh.src = entry.blob;
      fresh.onload = resolve;
      fresh.onerror = () => reject(new Error(`failed to execute ${entry.url}`));
      document.body.appendChild(fresh);
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
              .catch(() => elem.setAttribute('src', `${assetBase()}${normalized}`));
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
    throw new Error(`client HTML HTTP ${res.status} for ${state.cfg.cdn_base}${htmlPath}`);

  const doc = parseAppHtml(await res.text());
  const resources = collectResources(doc.head, doc.body);
  if (resources.scripts.length === 0) throw new Error('no scripts found in client HTML');

  const { styles, scripts } = await loadAll(resources);

  installInterceptor();
  injectDOM(doc, styles, scripts);

  setStatus('STARTING');
  await executeScripts(scripts);
  await waitForMount();

  setProgress(0, 1);
  setStatus('READY');
  document.getElementById('voidbar-loading')?.remove();
  log('client mounted');
}

boot().catch((e) => fatal(e.message || String(e)));
