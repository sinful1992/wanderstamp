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
//   - cross-origin (map tiles) is never touched.

const CACHE = "shell-v1";

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
      .then((keys) => Promise.all(keys.filter((k) => k !== CACHE).map((k) => caches.delete(k))))
      .then(() => self.clients.claim())
  );
});

self.addEventListener("fetch", (e) => {
  if (e.request.method !== "GET") return;
  const url = new URL(e.request.url);
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
