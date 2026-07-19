// App-shell service worker.
//
// Strategy, not a generic offline cache:
//   - mutable shell (/, app.js, style.css, manifest) — network-first with a
//     timeout, falling back to the cached copy. Online behaviour is unchanged
//     (always fresh, no version skew between index.html and app.js); offline
//     the app still opens.
//   - immutable assets (vendored leaflet/, the woff2 font, icons/) — cache-first;
//     they only ever change together with a bumped CACHE name.
//   - /api/* is never touched: live data stays live, and failures surface to
//     the app's own error handling instead of a stale cache lying about state.
//   - CARTO basemap tiles — cache-first with a FIFO cap. Only tiles actually
//     viewed get cached (no prefetch: bulk downloading is against tile-server
//     policy); browse an area online once and it renders offline later.
//   - other cross-origin is never touched.

const CACHE = "shell-v1";
const TILE_CACHE = "tiles-v1";
const TILE_HOST = /^[a-d]\.basemaps\.cartocdn\.com$/;
// ~16KB/tile average → cap ≈ 65MB. FIFO, trimmed probabilistically so we
// don't enumerate thousands of cache keys on every single tile fetch.
const TILE_MAX = 4000;

const SHELL = [
  "/",
  "/app.js",
  "/style.css",
  "/manifest.webmanifest",
  "/leaflet/leaflet.css",
  "/leaflet/leaflet.js",
  "/staatliches.woff2",
  "/leaflet/images/marker-icon.png",
  "/leaflet/images/marker-icon-2x.png",
  "/leaflet/images/marker-shadow.png",
];

const immutable = (path) =>
  path.startsWith("/leaflet/") || path.startsWith("/icons/") || path.endsWith(".woff2");

self.addEventListener("install", (e) => {
  e.waitUntil(caches.open(CACHE).then((c) => c.addAll(SHELL)).then(() => self.skipWaiting()));
});

self.addEventListener("activate", (e) => {
  e.waitUntil(
    caches.keys()
      .then((keys) => Promise.all(
        keys.filter((k) => k !== CACHE && k !== TILE_CACHE).map((k) => caches.delete(k))
      ))
      .then(() => self.clients.claim())
  );
});

self.addEventListener("fetch", (e) => {
  if (e.request.method !== "GET") return;
  const url = new URL(e.request.url);
  if (TILE_HOST.test(url.host)) {
    e.respondWith(serveTile(e, url));
    return;
  }
  if (url.origin !== location.origin) return;
  if (url.pathname.startsWith("/api/") || url.pathname === "/healthz") return;
  // navigations (including /share/{token}) get the shell, same as the server
  const path = e.request.mode === "navigate" ? "/" : url.pathname;
  e.respondWith(serve(path));
});

async function serve(path) {
  const cache = await caches.open(CACHE);
  if (immutable(path)) {
    const hit = await cache.match(path);
    if (hit) return hit;
  }
  try {
    // abort quickly on a flaky link so the cached shell steps in; a hard
    // offline failure rejects immediately anyway
    const fresh = await fetch(path, { signal: AbortSignal.timeout(5000) });
    if (fresh.ok) cache.put(path, fresh.clone());
    return fresh;
  } catch (ex) {
    const hit = await cache.match(path);
    if (hit) return hit;
    throw ex;
  }
}

// The a-d subdomains all serve identical tiles, so the cache key drops the
// shard — a tile fetched from a. still hits when Leaflet later asks b. for it.
const tileKey = (url) => "https://basemaps.cartocdn.com" + url.pathname;

async function serveTile(e, url) {
  const key = tileKey(url);
  const cache = await caches.open(TILE_CACHE);
  const hit = await cache.match(key);
  if (hit) return hit;
  const fresh = await fetch(e.request);
  if (fresh.ok && fresh.type !== "opaque") {
    await cache.put(key, fresh.clone());
    if (Math.random() < 0.02) e.waitUntil(trimTiles(cache));
  }
  return fresh;
}

async function trimTiles(cache) {
  const keys = await cache.keys();
  // keys() returns insertion order; dropping from the front is FIFO eviction.
  // Trim below the cap with slack so this runs rarely, not on every overflow.
  if (keys.length <= TILE_MAX) return;
  await Promise.all(keys.slice(0, keys.length - (TILE_MAX - 400)).map((k) => cache.delete(k)));
}
