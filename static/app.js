/* Holiday Map frontend — vanilla JS + Leaflet. All user text goes through
   textContent (never innerHTML) so titles/notes can't inject markup. */

"use strict";

const SWATCHES = ["#1d6fb8", "#c2452d", "#2e8b57", "#b8860b", "#7b4b94", "#d81b60", "#00838f", "#5d4037"];
// what a screen reader says for each swatch, in the same order
const SWATCH_NAMES = ["Blue", "Red", "Green", "Gold", "Purple", "Pink", "Teal", "Brown"];

const state = {
  me: null,
  holidays: [],
  pins: [],
  hidden: new Set(),      // holiday ids toggled off (remembered, see saveHidden)
  query: "",              // the trips list's search box
  year: null,             // the trips list's year chip, null = all years
  markers: new Map(),  // pin key -> Leaflet marker, reconciled in renderMarkers
  routes: [],             // one dotted itinerary line per visible trip
  dests: new Map(),       // holiday id -> destination stamp marker
  focused: null,          // holiday id spotlit by tapping its trip row
  placing: false,
  didFit: false,
  fitKey: "",             // the points the opening view was fitted to
};

const $ = (id) => document.getElementById(id);

// Share mode: /share/<token> serves this same app read-only for one trip.
const SHARE = location.pathname.startsWith("/share/") ? location.pathname.split("/")[2] : null;

function photoURL(asset, kind) {
  return SHARE ? `/api/share/${SHARE}/photo/${asset}/${kind}` : `/api/photo/${asset}/${kind}`;
}

function el(tag, cls, text) {
  const n = document.createElement(tag);
  if (cls) n.className = cls;
  if (text !== undefined) n.textContent = text;
  return n;
}

// F7: "1:05" for a video's length
function fmtDuration(sec) {
  const s = Math.max(0, Math.round(sec || 0));
  const h = Math.floor(s / 3600), m = Math.floor((s % 3600) / 60), ss = String(s % 60).padStart(2, "0");
  return h ? `${h}:${String(m).padStart(2, "0")}:${ss}` : `${m}:${ss}`;
}

// One photo tile. A video is its poster frame with a play mark and its
// length on it; onClick gets the img either way (selection marks the img).
function thumbCell(ph, onClick) {
  const img = el("img");
  img.loading = "lazy";
  img.src = photoURL(ph.asset_id, "thumb");
  img.alt = "";
  img.dataset.asset = ph.asset_id;
  img.onclick = (e) => onClick(e, img);
  if (ph.type !== "video") return img;
  const cell = el("span", "vthumb");
  const badge = el("span", "vbadge", `\u25B6 ${fmtDuration(ph.duration)}`);
  badge.setAttribute("aria-hidden", "true");
  img.alt = `Video, ${fmtDuration(ph.duration)}`;
  cell.append(img, badge);
  return cell;
}

// The trips sheet is a disclosure; one setter keeps the pill's announced
// state and the class in step.
function setSheet(open) {
  $("sheet").classList.toggle("collapsed", !open);
  $("sheet-pill").setAttribute("aria-expanded", String(open));
}

// "1 pin", "2 pins", "0 photos". English only, like the rest of the UI.
function plural(n, one, many = one + "s") {
  return `${n} ${n === 1 ? one : many}`;
}

// action, when given, is { label, fn }: a button in the toast (Undo). That
// toast stays up longer, long enough to read it and reach the button.
function toast(msg, action) {
  const t = $("toast");
  t.textContent = msg;
  if (action) {
    const b = el("button", "toast-act", action.label);
    b.type = "button";
    b.onclick = () => { t.hidden = true; clearTimeout(toast.timer); action.fn(); };
    t.append(" ", b);
  }
  t.hidden = false;
  clearTimeout(toast.timer);
  toast.timer = setTimeout(() => { t.hidden = true; }, action ? 10000 : 3500);
}

// F2: a delete answers with an undo token the server keeps for 30 s.
function deletedToast(what, r) {
  if (!r || !r.undo) { toast(`${what} deleted`); return; }
  toast(`${what} deleted`, {
    label: "Undo",
    fn: () => busy(null, async () => {
      try {
        await api("POST", `/api/undo/${encodeURIComponent(r.undo)}`);
      } catch (ex) {
        if (ex.status === 410) { toast("Too late to undo"); return; }
        // the server keeps whatever was made or changed since, rather than
        // overwrite it (a pin the photo sync remade and someone named, say)
        if (ex.status === 409) {
          toast("Couldn't undo: something there has changed since, or another trip is live");
          await loadData();
          return;
        }
        throw ex;
      }
      toast(`${what} is back`);
      await loadData();
    }),
  });
}

// meta, when given, receives the raw response (meta.resp) for its headers.
async function api(method, path, body, meta) {
  const opts = { method, credentials: "same-origin" };
  if (body !== undefined) {
    opts.headers = { "Content-Type": "application/json" };
    opts.body = JSON.stringify(body);
  }
  const resp = await fetch(path, opts);
  if (meta) meta.resp = resp;
  if (resp.status === 401 && path !== "/api/login") {
    showLogin();
    const err = new Error("not logged in");
    err.status = 401;
    throw err;
  }
  if (resp.status === 428) {
    // signed in on a temporary password: the server opens nothing else
    showChoosePassword(null);
    const err = new Error("choose your own password first");
    err.status = 428;
    throw err;
  }
  const data = await resp.json().catch(() => ({}));
  if (!resp.ok) {
    const err = new Error(data.error || resp.statusText);
    err.status = resp.status;
    throw err;
  }
  return data;
}

/* ---------- offline: data snapshot + pin queue ---------- */

// The service worker keeps the shell; these keep the data. A snapshot of the
// last successful load lets the map open with its trips and pins when there's
// no signal, and pins dropped offline wait in a queue until the network comes
// back. localStorage rather than IndexedDB: the payload is a few KB of JSON
// and synchronous access keeps every call site trivial.
const SNAP_KEY = "hm-snapshot";
const QUEUE_KEY = "hm-pin-queue";

function saveSnapshot() {
  try {
    localStorage.setItem(SNAP_KEY, JSON.stringify({ holidays: state.holidays, pins: state.pins, at: Date.now(),
      user: state.me ? state.me.username : null }));
  } catch { /* storage full or blocked — the offline view just won't have data */ }
}

function restoreSnapshot() {
  try {
    const s = JSON.parse(localStorage.getItem(SNAP_KEY));
    if (!s || !Array.isArray(s.holidays)) return false;
    state.holidays = s.holidays;
    state.pins = s.pins || [];
    state.snapUser = s.user || null;
    loadHidden(s.user);
    renderAll(true);
    return true;
  } catch {
    return false;
  }
}

function pinQueue() {
  try { return JSON.parse(localStorage.getItem(QUEUE_KEY)) || []; } catch { return []; }
}

function setPinQueue(q) {
  try { localStorage.setItem(QUEUE_KEY, JSON.stringify(q)); } catch {}
}

// One id per pin, made before its first post. The server remembers it
// (client_id), so the same pin sent again after a lost answer is stored once.
const CLIENT_ID_OK = /^[A-Za-z0-9_:.-]{1,64}$/;
function newClientId() {
  // randomUUID needs a secure context; the LAN address over plain HTTP has none
  if (window.crypto && typeof crypto.randomUUID === "function") return crypto.randomUUID();
  return Date.now() + ":" + Math.random().toString(36).slice(2, 8);
}

// WA-3: a waiting pin belongs to whoever placed it. On an offline boot
// /api/map never answered, so the snapshot's user is the one on this device.
function queueOwner() {
  return state.me ? state.me.username : (state.snapUser ?? null);
}

// A pin queued before owners were recorded has none: it goes as today.
function ownQueued(item) {
  return item.by == null || item.by === queueOwner();
}

function queuePin(pin) {
  const { client_id, ...rest } = pin;
  rest.qid = client_id || newClientId();
  rest.by = queueOwner();
  setPinQueue([...pinQueue(), rest]);
  renderMarkers();
}

// Network failures (offline, server unreachable) surface as TypeError from
// fetch; HTTP-level failures come back as plain Errors from api(). Only the
// former mean "worth retrying later".
function isNetworkError(ex) {
  return ex instanceof TypeError;
}

// Worth sending again later: no network, a server that's restarting or behind
// a proxy that answers 502/503, a rate limit, or a sign-in that has to happen
// first. Only a real "no" from the server (a 4xx: the trip was deleted) drops it.
function isRetryable(ex) {
  if (isNetworkError(ex)) return true;
  const st = ex && ex.status;
  return st === undefined || st === 401 || st === 408 || st === 428 || st === 429 || st >= 500;
}

let syncingQueue = false;
async function syncQueue() {
  if (syncingQueue || SHARE) return;
  // someone else's pins wait for them to sign in on this device again
  const q = state.me ? pinQueue().filter(ownQueued) : [];
  if (!q.length) return;
  syncingQueue = true;
  let sent = 0, dropped = 0, gone = 0;
  // Taken off the stored queue one by one, so a pin queued while this runs
  // stays queued.
  const same = (a, b) => (a.qid || b.qid ? a.qid === b.qid : JSON.stringify(a) === JSON.stringify(b));
  const remove = (item) => {
    const cur = pinQueue();
    const i = cur.findIndex((x) => same(x, item));
    if (i >= 0) { cur.splice(i, 1); setPinQueue(cur); }
  };
  try {
    for (const item of q) {
      // discarded while an earlier pin was sending: don't send it
      if (!pinQueue().some((x) => same(x, item))) continue;
      const { qid, by, ...pin } = item;
      // a queue id the server would refuse goes without: never lose a pin over its key
      if (CLIENT_ID_OK.test(String(qid ?? ""))) pin.client_id = String(qid);
      try {
        await api("POST", "/api/pins", pin);
        sent++;
      } catch (ex) {
        if (isRetryable(ex)) {
          // A 5xx is about this pin's request: keep it for the next pass and
          // try the rest. No network, a sign-in or a rate limit hits every
          // pin alike: stop and keep everything.
          if (ex && ex.status >= 500) continue;
          break;
        }
        // 410: this pin did reach the server earlier and was deleted since:
        // nothing was lost, so nothing to report
        if (ex && ex.status === 410) gone++;
        else dropped++; // the server said no (trip deleted?) — retrying forever won't fix it
      }
      remove(item);
    }
  } finally {
    syncingQueue = false;
  }
  if (!sent && !dropped) {
    if (gone) renderMarkers();
    return;
  }
  const bits = [];
  if (sent) bits.push(`${sent} offline pin${sent > 1 ? "s" : ""} synced`);
  if (dropped) bits.push(`${dropped} rejected by the server`);
  toast(bits.join(" · "));
  if (sent) loadData().catch(() => {});
  else renderMarkers();
}

window.addEventListener("online", () => {
  // an offline boot never loaded /api/me — reboot properly now that we can
  if (!state.me && !SHARE) { location.reload(); return; }
  syncQueue();
});

/* ---------- map ---------- */

const map = L.map("map", { zoomControl: false, worldCopyJump: true });
L.control.zoom({ position: "bottomright" }).addTo(map);
// Esri World Topo: a printed-atlas plate — muted greens, relief shading and
// italic serif water labels — and still keyless, which CARTO's basemaps
// stopped being in Aug 2026 (every tile came back stamped "API KEY REQUIRED").
// Dark scheme swaps to Esri's night plate, matching the CSS token flip in
// style.css. Note Esri's tile path is {z}/{y}/{x}, not the usual {z}/{x}/{y}.
const darkScheme = matchMedia("(prefers-color-scheme: dark)");
const ESRI = "https://server.arcgisonline.com/ArcGIS/rest/services";
const esriPlate = (service, opts) =>
  L.tileLayer(`${ESRI}/${service}/MapServer/tile/{z}/{y}/{x}`, {
    maxZoom: 20,
    // deepest level Esri has cached; Leaflet upscales beyond it rather than
    // dropping to blank tiles when you zoom in on a single caravan pitch
    maxNativeZoom: 19,
    // CORS mode so the service worker's tile cache stores real responses, not
    // opaque ones (opaque entries get ~7MB of quota padding each in Chromium)
    crossOrigin: "anonymous",
    attribution: 'Tiles &copy; <a href="https://www.esri.com/">Esri</a>',
    ...opts,
  });
// Dark Gray ships its labels as a SEPARATE reference layer — the base alone
// carries no place names at all, so the night plate is two layers stacked.
const plates = {
  light: [esriPlate("World_Topo_Map")],
  dark: [
    esriPlate("Canvas/World_Dark_Gray_Base", { zIndex: 1 }),
    esriPlate("Canvas/World_Dark_Gray_Reference", { zIndex: 2, attribution: "" }),
  ],
};
let plate = [];
function setPlate() {
  const next = darkScheme.matches ? plates.dark : plates.light;
  if (next === plate) return;
  plate.forEach((l) => map.removeLayer(l));
  plate = next;
  plate.forEach((l) => l.addTo(map));
}
setPlate();
darkScheme.addEventListener("change", setPlate);
map.setView([35, 10], 3);

// The plate's age: a flat warm wash in a pane of its own between the tiles
// (200) and the routes and pins (400+), so it tints the atlas and never a
// pin. It used to be a sepia filter on the tile pane, which re-filtered
// every tile on every frame of a pan; a flat layer costs nothing to move.
// It is far larger than the screen and re-centred as the map moves (a
// transform, not a repaint), so even a long fling never shows its edge.
const washPane = map.createPane("wash");
washPane.style.zIndex = 250;
washPane.style.pointerEvents = "none";
const wash = L.DomUtil.create("div", "plate-wash", washPane);
function placeWash() {
  L.DomUtil.setPosition(wash, map.containerPointToLayerPoint([0, 0]));
}
map.on("move moveend zoomend resize viewreset", placeWash);
placeWash();

// Pins wear three outfits by zoom: enamel dots at country/world zoom (so a
// hundred photo stops never blanket a continent), small prints at region
// zoom, full photo prints with counts up close. CSS reads data-zoom.
function applyZoomTier() {
  const z = map.getZoom();
  $("map").dataset.zoom = z >= 11 ? "near" : z >= 7 ? "mid" : "far";
}
// P2: clusters are worked out per zoom level, and a print only fetches its
// thumbnail once it's big enough to show it
const CLUSTER_BELOW = 7;
map.on("zoomend", () => { if (state.pins) renderMarkers(); });
map.on("zoomend", applyZoomTier);
applyZoomTier();

map.on("click", (e) => {
  if (state.placing) { placePin(e.latlng); return; }
  setSheet(false); // tap the atlas to tuck the trips sheet away
  if (story.hid !== null) return; // the story keeps its spotlight while open
  if (state.focused !== null) setFocus(null); // tap the atlas to release the spotlight
});

/* ---------- data ---------- */

function holidayById(id) {
  return state.holidays.find((h) => h.id === id);
}

// One round trip: who's signed in, the trips and the pins together.
async function loadData(fit) {
  const meta = {};
  const d = await api("GET", "/api/map", undefined, meta);
  state.me = d.me;
  state.holidays = d.holidays;
  state.pins = d.pins;
  loadHidden(d.me && d.me.username);
  renderAccount();
  renderAll(fit);
  for (const fn of dataListeners) {
    try { fn(state); } catch (ex) { console.error(ex); } // one broken module can't stop the map
  }
  saveSnapshot();
  syncQueue(); // reaching the server just now proves any queued pins can go
  if (meta.resp.headers.get("X-Photo-Sync") === "running") awaitPhotoSync();
}

// Opening the map starts a background photo sync on the server, which answers
// with the pins it already had. Wait for that sync and load again, or photos
// that reached Immich since the last visit only show up on the next one.
// Reloads once, and only after a successful sync: a failing Immich leaves the
// map as it is rather than looping.
let awaitingSync = false;
async function awaitPhotoSync() {
  if (awaitingSync) return;
  awaitingSync = true;
  try {
    const r = await api("GET", "/api/sync/wait");
    if (r.ok) await loadData();
  } catch {
    // offline or signed out: the map keeps what it has
  } finally {
    awaitingSync = false;
  }
}

// A form someone has typed into. A background reload (photo sync, coming
// back online) must not rebuild the panel under it and throw the text away;
// that panel catches up on the next load after the form is saved or closed.
document.addEventListener("input", (e) => {
  const f = e.target.closest && e.target.closest("form");
  if (f) f.dataset.dirty = "1";
}, true);
// A plain form is done with at submit; one saved through busy() stays dirty
// until its save worked.
document.addEventListener("submit", (e) => { delete e.target.dataset.dirty; }, true);

function editing(id) {
  return [...$(id).querySelectorAll("form[data-dirty]")].some((f) => !f.closest("[hidden]") && f.isConnected);
}

function renderAll(fit) {
  if (story.hid !== null && !holidayById(story.hid)) closeStory();
  if (state.focused !== null && !holidayById(state.focused)) state.focused = null;
  renderMarkers();
  if (!editing("story")) refreshStory();
  if (!editing("trip-list")) renderSheet();
  renderBanner();
  // The first fit may have been drawn from the saved snapshot; fresh data
  // that moves the opening view (a trip added on another phone) fits again,
  // once, before anything else has claimed the map.
  if (fit && (!state.didFit || (state.fitFromSnapshot && story.hid === null))) {
    const key = fitAll(state.didFit ? state.fitKey : null);
    state.fitFromSnapshot = !state.me;
    state.didFit = true;
    state.fitKey = key;
  }
}

function visiblePins() {
  return mapPins(state.pins.filter((p) => !state.hidden.has(p.holiday_id)));
}

// Photos taken on the way there get no pin on the map — otherwise every trip
// would open with a cluster of photo prints around home. They keep their
// chapters in the story's way leg, and a pin placed by hand on the way stays.
// Worked out once per load: every render and every row asks, and state.pins
// and state.holidays are replaced (never edited in place) when data changes.
const wayMemo = { pins: null, holidays: null, ids: null };
function wayPhotoIds() {
  if (wayMemo.pins === state.pins && wayMemo.holidays === state.holidays) return wayMemo.ids;
  const byTrip = new Map();
  for (const p of state.pins) {
    if (!byTrip.has(p.holiday_id)) byTrip.set(p.holiday_id, []);
    byTrip.get(p.holiday_id).push(p);
  }
  const ids = new Set();
  for (const h of state.holidays) {
    if (!hasDest(h)) continue;
    const j = journey(h, tripStops(byTrip.get(h.id) || []));
    // a photo stop you went back to after arriving is on the map
    const stayed = new Set(j.stay.map((s) => s.pin.id));
    for (const s of j.way) if (s.pin.kind === "photo" && !stayed.has(s.pin.id)) ids.add(s.pin.id);
  }
  Object.assign(wayMemo, { pins: state.pins, holidays: state.holidays, ids });
  return ids;
}

function mapPins(pins) {
  const way = wayPhotoIds();
  return pins.filter((p) => !way.has(p.id));
}

// Which day of the trip a moment falls on. The day turns over at 4 am, as
// the server's visits do, so a photo after midnight stays with its evening.
// Counted from the trip's first date as a calendar date, not an instant:
// bare dates are stored as UTC midnight.
function tripDay(h, at) {
  const t = new Date(new Date(at).getTime() - 4 * 3600000);
  const key = `${t.getFullYear()}-${t.getMonth()}-${t.getDate()}`;
  // the first day in the viewer's calendar: a picked date as that date, a
  // real start moment in local time (start_at's UTC date is a day off in
  // New York or Bangkok)
  const s0 = calDay(h.start_at);
  const first = new Date(s0.getFullYear(), s0.getMonth(), s0.getDate());
  const today = new Date(t.getFullYear(), t.getMonth(), t.getDate());
  const n = Math.max(1, Math.round((today - first) / 86400000) + 1);
  const label = t.toLocaleDateString(undefined, { weekday: "long", day: "numeric", month: "long" });
  return { key, n, label };
}

// A trip's stops in the order they happened. A pin is a place; a stop is one
// stay there (the server's visits, see visits.go), so the hotel you slept at
// twice, or the castle you went back to, is a stop each time: its own story
// chapter with only that day's photos, and the route goes back to it. A pin
// from an old snapshot has no visits and is one stop, as it used to be.
function tripStops(pins) {
  const stops = [];
  for (const pin of pins) {
    const vs = pin.visits && pin.visits.length ? pin.visits
      : [{ at: pin.visited_at, until: pin.visited_at, n: pin.photo_count, whole: true }];
    for (const v of vs) stops.push({ pin, at: v.at, until: v.until, n: v.n, whole: !!v.whole, day: v.day, note: v.note, key: `${pin.id}@${v.at}` });
  }
  stops.sort((a, b) => (a.at < b.at ? -1 : a.at > b.at ? 1 : a.pin.id - b.pin.id));
  // "again": back after being somewhere else, not the next morning at the hotel
  const been = new Set();
  stops.forEach((s, i) => {
    s.again = been.has(s.pin.id) && stops[i - 1].pin !== s.pin;
    been.add(s.pin.id);
  });
  return stops;
}

// Fits the map to every visible pin and returns what it fitted to; given
// the key of an earlier fit, it leaves the map alone when nothing moved.
function fitAll(sameAs) {
  const pts = visiblePins().map((p) => [p.lat, p.lng]);
  // where a live or planned trip is headed belongs in the opening view too
  for (const h of state.holidays) {
    if (hasDest(h) && !h.end_at && !state.hidden.has(h.id)) pts.push([h.dest_lat, h.dest_lng]);
  }
  const key = JSON.stringify(pts);
  if (pts.length && key !== sameAs) map.fitBounds(pts, { padding: [50, 50], maxZoom: 12 });
  return key;
}

/* ---------- the journey: the way there, then the trip itself ---------- */

function hasDest(h) {
  return !!(h && h.dest_name);
}

// The destination's own name: "Warwick Castle, Castle Hill, …" -> "Warwick Castle".
// "lat, lng" as typed into a destination field
const COORD_RE = /^\s*(-?\d+(?:\.\d+)?)\s*[, ]\s*(-?\d+(?:\.\d+)?)\s*$/;

// A destination still named by its coordinates (typed while the place
// lookup was unreachable, or saved before it existed) has no short name —
// cutting "50.70000, -1.70000" at the comma would leave half a number.
function destShort(h) {
  return COORD_RE.test(h.dest_name) ? "your destination" : h.dest_name.split(",")[0].trim();
}

function kmBetween(a, b) {
  const rad = Math.PI / 180;
  const dLat = (b.lat - a.lat) * rad, dLng = (b.lng - a.lng) * rad;
  const x = Math.sin(dLat / 2) ** 2 + Math.cos(a.lat * rad) * Math.cos(b.lat * rad) * Math.sin(dLng / 2) ** 2;
  return 12742 * Math.asin(Math.sqrt(x));
}

// F5: a leg this long was flown, not driven: it's drawn as the great circle
// a plane takes, with a small plane at its middle pointing the way it went.
const ARC_KM = 300;

// Points along the great circle from a to b ([lat, lng] pairs). Longitudes
// are unwrapped so the line never jumps across the map at the 180th meridian.
function greatCircle(a, b, n = 64) {
  const rad = Math.PI / 180;
  const toV = ([lat, lng]) => [Math.cos(lat * rad) * Math.cos(lng * rad), Math.cos(lat * rad) * Math.sin(lng * rad), Math.sin(lat * rad)];
  const va = toV(a), vb = toV(b);
  const dot = Math.min(1, Math.max(-1, va[0] * vb[0] + va[1] * vb[1] + va[2] * vb[2]));
  const w = Math.acos(dot);
  if (w < 1e-9 || Math.PI - w < 1e-6) return [a, b]; // same point, or exactly opposite (no single way)
  const out = [];
  let prev = a[1];
  for (let i = 0; i <= n; i++) {
    const t = i / n;
    const k1 = Math.sin((1 - t) * w) / Math.sin(w), k2 = Math.sin(t * w) / Math.sin(w);
    const x = k1 * va[0] + k2 * vb[0], y = k1 * va[1] + k2 * vb[1], z = k1 * va[2] + k2 * vb[2];
    let lng = Math.atan2(y, x) / rad;
    while (lng - prev > 180) lng -= 360;
    while (lng - prev < -180) lng += 360;
    prev = lng;
    out.push([Math.atan2(z, Math.hypot(x, y)) / rad, lng]);
  }
  return out;
}

const wrapLng = (lng) => ((((lng + 180) % 360) + 360) % 360) - 180;

// Cuts a continuous (unwrapped) line where it crosses the 180th meridian, so
// every piece lies in -180..180, the world copy the pins are drawn in: the
// line leaves at one edge and comes back in at the other, meeting each pin.
function splitAtSeam(points) {
  const pieces = [];
  let off = 0;
  let cur = [];
  for (const p of points) {
    const q = [p[0], p[1] + off];
    if (cur.length && (q[1] > 180 || q[1] < -180)) {
      const prev = cur[cur.length - 1];
      const edge = q[1] > 180 ? 180 : -180;
      const lat = prev[0] + (edge - prev[1]) / (q[1] - prev[1]) * (q[0] - prev[0]);
      cur.push([lat, edge]);
      pieces.push(cur);
      off += edge === 180 ? -360 : 360;
      cur = [[lat, -edge], [p[0], p[1] + off]];
      continue;
    }
    cur.push(q);
  }
  if (cur.length > 1) pieces.push(cur);
  return pieces;
}

// A route as line pieces, with every long leg bent into its arc, plus where
// each arc's plane sits and which way it points (degrees clockwise from north
// on the map; Mercator keeps angles, so it holds at every zoom).
function arcRoute(latlngs) {
  // first one continuous line: each leg takes the short way round, so a hop
  // over the 180th meridian is a few km, not a sweep across the whole map
  const pts = [latlngs[0]];
  const planes = [];
  for (let i = 1; i < latlngs.length; i++) {
    const a = latlngs[i - 1], b = latlngs[i];
    const from = pts[pts.length - 1];
    let to = b[1] + (from[1] - a[1]);
    while (to - from[1] > 180) to -= 360;
    while (to - from[1] < -180) to += 360;
    if (kmBetween({ lat: a[0], lng: a[1] }, { lat: b[0], lng: b[1] }) < ARC_KM) { pts.push([b[0], to]); continue; }
    const arc = greatCircle(from, [b[0], to]);
    pts.push(...arc.slice(1));
    const m = Math.floor(arc.length / 2);
    const p = L.CRS.EPSG3857.latLngToPoint(L.latLng(arc[m - 1]), 4), q = L.CRS.EPSG3857.latLngToPoint(L.latLng(arc[m + 1] || arc[m]), 4);
    planes.push({ at: [arc[m][0], wrapLng(arc[m][1])], deg: Math.atan2(q.x - p.x, p.y - q.y) * 180 / Math.PI });
  }
  return { pieces: splitAtSeam(pts), planes };
}

function planeIcon(color, deg) {
  const div = el("div", "plane-mark");
  div.style.setProperty("--c", color);
  div.style.transform = `rotate(${Math.round(deg)}deg)`;
  div.innerHTML = '<svg viewBox="0 0 24 24" aria-hidden="true"><path d="M12 2c.8 0 1.4.9 1.4 2v5.2l7.6 4.4v2l-7.6-2.3v4.6l2.2 1.7V21L12 20l-3.6 1v-1.4l2.2-1.7v-4.6L3 15.6v-2l7.6-4.4V4c0-1.1.6-2 1.4-2z"/></svg>';
  return L.divIcon({ html: div.outerHTML, className: "plane-icon", iconSize: [18, 18], iconAnchor: [9, 9] });
}

// Splits a trip's pins (in visit order) at the first one that reached the
// destination — decided by the server (arrival_pin), which holds the place's
// real outline: anywhere inside Paris is Paris, and a pin within 5 km of the
// point counts too. Everything before that pin was on the way there. A live trip that
// hasn't arrived is still on the way; a finished trip that never pinned near
// its destination isn't split at all — its destination was a region, or the
// plans changed, and calling the whole trip "on the way" would be wrong.
function journey(h, stops) {
  const none = { way: [], stay: stops, arrived: false, enRoute: false };
  if (!hasDest(h)) return none;
  const i = h.arrival_pin ? stops.findIndex((s) => s.pin.id === h.arrival_pin) : -1;
  if (i >= 0) return { way: stops.slice(0, i), stay: stops.slice(i), arrived: true, enRoute: false };
  if (h.active) return { way: stops, stay: [], arrived: false, enRoute: true };
  return none;
}

/* ---------- markers ---------- */

// D1: a stop number rides on the open story's pins (n = its first stop)
function pinIcon(pin, color, n) {
  const far = map.getZoom() < CLUSTER_BELOW;
  // A hand pin that photos were filed onto wears the print too, with a
  // pushpin head in its corner so it still reads as a place you chose.
  if (pin.kind === "photo" || pin.photo_count > 0) {
    // A small landscape photo print laid on the atlas: cream print border,
    // enamel edge in the trip's colour, count on the corner. The print is
    // centred in a fixed icon box so zoom-tier resizing stays anchored.
    const div = el("div", pin.kind === "photo" ? "photo-pin" : "photo-pin hand");
    div.style.setProperty("--c", color);
    const print = el("span", "print");
    // offline the thumb request can't succeed — the empty cream print reads
    // better than a broken-image glyph
    if (pin.cover_asset && navigator.onLine && !far) {
      const img = el("img");
      img.src = photoURL(pin.cover_asset, "thumb");
      img.alt = "";
      print.appendChild(img);
    }
    div.appendChild(print);
    div.appendChild(el("span", "badge-count", String(pin.photo_count)));
    if (pin.kind !== "photo") div.appendChild(el("span", "hand-mark"));
    if (n) div.appendChild(el("span", "stop-n", String(n)));
    return L.divIcon({ html: div.outerHTML, iconSize: [48, 40], iconAnchor: [24, 20], popupAnchor: [0, -20] });
  }
  // a place picked by hand: a map-maker's ringed dot in the trip's ink,
  // centred on the place (the old teardrop pointed a few px off it)
  const div = el("div", "manual-pin" + (n ? " numbered" : ""));
  div.style.setProperty("--c", color);
  if (n) div.appendChild(el("span", "stop-n", String(n)));
  return L.divIcon({ html: div.outerHTML, iconSize: [22, 22], iconAnchor: [11, 11], popupAnchor: [0, -11] });
}

// P2: a trip's pins that would sit on top of each other at world zoom fold
// into one dot with a count; tapping it zooms in until they come apart
function clusterIcon(n, color) {
  const div = el("div", "pin-cluster");
  div.style.setProperty("--c", color);
  div.appendChild(el("span", "cl-n", String(n)));
  const size = n < 10 ? 26 : n < 100 ? 30 : 36;
  return L.divIcon({ html: div.outerHTML, iconSize: [size, size], iconAnchor: [size / 2, size / 2] });
}

// Folds a trip's pins that sit within 44 px of each other at the current
// zoom: each group grows around its first pin (lowest id, so the groups and
// their markers stay put across reloads), found through a grid of seeds so
// 1,800 pins stay cheap. The open story's trip is never folded: its chapters
// need their own pins.
const CLUSTER_PX = 44;
function clusterPins(pins) {
  if (map.getZoom() >= CLUSTER_BELOW) return { singles: pins, groups: [] };
  const z = map.getZoom();
  const seeds = new Map(); // "trip|cx|cy" -> [{ x, y, pins, key }]
  const all = [];
  const singles = [];
  for (const pin of [...pins].sort((a, b) => a.id - b.id)) {
    if (pin.holiday_id === story.hid) { singles.push(pin); continue; }
    const p = map.project([pin.lat, pin.lng], z);
    const cx = Math.floor(p.x / CLUSTER_PX), cy = Math.floor(p.y / CLUSTER_PX);
    let best = null, bestD = CLUSTER_PX;
    for (let dx = -1; dx <= 1; dx++) {
      for (let dy = -1; dy <= 1; dy++) {
        for (const g of seeds.get(`${pin.holiday_id}|${cx + dx}|${cy + dy}`) || []) {
          const d = Math.hypot(g.x - p.x, g.y - p.y);
          if (d < bestD) { best = g; bestD = d; }
        }
      }
    }
    if (best) { best.pins.push(pin); continue; }
    const g = { x: p.x, y: p.y, pins: [pin], key: `${pin.holiday_id}|${pin.id}` };
    const k = `${pin.holiday_id}|${cx}|${cy}`;
    if (!seeds.has(k)) seeds.set(k, []);
    seeds.get(k).push(g);
    all.push(g);
  }
  const groups = [];
  for (const g of all) {
    if (g.pins.length === 1) singles.push(g.pins[0]);
    else groups.push({ key: g.key, pins: g.pins });
  }
  return { singles, groups };
}

// D1: each pin's number in the open story (its first stop), the same
// numbers the chapters carry
function storyNumbers() {
  const nums = new Map();
  if (story.hid === null) return nums;
  const h = holidayById(story.hid);
  if (!h) return nums;
  tripStops(state.pins.filter((p) => p.holiday_id === h.id)).forEach((s, i) => {
    if (!nums.has(s.pin.id)) nums.set(s.pin.id, i + 1);
  });
  return nums;
}

// Markers are reconciled, not rebuilt. Tearing the whole layer down on every
// loadData() meant every pin replayed its pop-in animation after any edit or
// reconnect, and it threw away Leaflet's DOM for pins that had not changed.
// Now a marker is only rebuilt when something it draws from actually differs.
function pinSig(pin, color, n) {
  return [pin.kind, color, pin.cover_asset, pin.photo_count, navigator.onLine ? 1 : 0,
    map.getZoom() < CLUSTER_BELOW ? "far" : "", n || ""].join("|");
}

function renderMarkers() {
  const want = new Map(); // key -> descriptor
  const nums = storyNumbers();
  const shown = visiblePins();
  const { singles, groups } = clusterPins(shown);
  for (const pin of singles) {
    const h = holidayById(pin.holiday_id);
    want.set("p" + pin.id, { pin, color: h ? h.color : "#666", n: nums.get(pin.id) });
  }
  for (const g of groups) {
    const h = holidayById(g.pins[0].holiday_id);
    want.set("c" + g.key, { group: g, color: h ? h.color : "#666" });
  }
  // Pins waiting to sync ride along as ghosts: same pushpin, dashed and
  // faded. They're local-only until the network returns; tap to discard.
  for (const qp of pinQueue()) {
    if (!ownQueued(qp)) continue;
    if (state.hidden.has(qp.holiday_id)) continue;
    const h = holidayById(qp.holiday_id);
    want.set("q" + qp.qid, { qp, color: h ? h.color : "#666" });
  }

  for (const [key, marker] of state.markers) {
    if (!want.has(key)) { marker.remove(); state.markers.delete(key); }
  }

  const byTrip = new Map();
  for (const pin of shown) {
    if (!byTrip.has(pin.holiday_id)) byTrip.set(pin.holiday_id, []);
    byTrip.get(pin.holiday_id).push(pin);
  }
  for (const [key, w] of want) {
    if (w.group) {
      const g = w.group;
      const sig = `c|${w.color}|${g.pins.length}`;
      let m = state.markers.get(key);
      const ll = L.latLngBounds(g.pins.map((p) => [p.lat, p.lng])).getCenter();
      if (!m) {
        m = L.marker(ll, { icon: clusterIcon(g.pins.length, w.color), riseOnHover: true }).addTo(map);
        m.sig = sig;
        state.markers.set(key, m);
        m.on("click", () => {
          const b = L.latLngBounds(m.cluster.map((p) => [p.lat, p.lng]));
          // the pins share a spot even at this zoom: go straight to where they part
          if (b.getNorthEast().equals(b.getSouthWest(), 1e-4)) map.setView(b.getCenter(), CLUSTER_BELOW + 1);
          else map.fitBounds(b, { padding: [40, 40], maxZoom: CLUSTER_BELOW + 4 });
        });
      } else {
        m.setLatLng(ll);
        if (m.sig !== sig) { m.setIcon(clusterIcon(g.pins.length, w.color)); m.sig = sig; }
      }
      m.cluster = g.pins;
      m.tripId = g.pins[0].holiday_id;
      m.pinId = null;
      const trip = holidayById(m.tripId);
      const elm = m.getElement();
      if (elm) elm.setAttribute("aria-label", `${g.pins.length} pins${trip ? ` · ${trip.name}` : ""}, zoom in`);
      continue;
    }
    const pending = !w.pin;
    const src = w.pin || w.qp;
    const sig = pending ? "q|" + w.color : pinSig(w.pin, w.color, w.n);
    let marker = state.markers.get(key);
    if (!marker) {
      marker = L.marker([src.lat, src.lng], {
        icon: pending ? pendingIcon(w.color) : pinIcon(w.pin, w.color, w.n),
        riseOnHover: true,
      }).addTo(map);
      marker.sig = sig;
      state.markers.set(key, marker);
    } else {
      const ll = marker.getLatLng();
      if (ll.lat !== src.lat || ll.lng !== src.lng) marker.setLatLng([src.lat, src.lng]);
      if (marker.sig !== sig) {
        marker.setIcon(pending ? pendingIcon(w.color) : pinIcon(w.pin, w.color, w.n));
        marker.sig = sig;
      }
    }
    marker.tripId = src.holiday_id;
    marker.pinId = pending ? null : w.pin.id;
    marker.qp = pending ? w.qp : null;
    // a pin is a button to a screen reader too, so it says which place it is
    const elm = marker.getElement();
    if (elm) {
      const trip = holidayById(src.holiday_id);
      const what = pending ? `${src.title || "Pin"}, waiting to sync` : src.title || "Pin";
      elm.setAttribute("aria-label", trip ? `${what} · ${trip.name}` : what);
    }
    // Bound once, on creation: the handler reads the marker's current fields
    // rather than closing over the descriptor from the render that made it.
    // Tapping a pin opens its trip's story at that chapter — the chapter
    // carries everything the old popup did (photos, note, edit, delete).
    if (!marker.bound) {
      marker.bound = true;
      marker.on("click", () => {
        if (marker.qp) { openPendingPopup(marker.qp); return; }
        const owner = holidayById(marker.tripId);
        if (owner) openStory(owner, marker.pinId);
      });
    }
  }

  // The itinerary line: a dotted ink route joining each trip's stops in the
  // order they happened (tripStops: a place visited twice is passed through
  // twice, so pins added after the fact still land in the right leg). Lines
  // render in Leaflet's overlay pane, under the marker pane — photo prints
  // always sit on top of the ink. Cheap to rebuild, so they still are.
  //
  // A trip with a destination draws the way there differently: long road
  // dashes up to the pin that arrived, the usual dots from there on, and
  // while a live trip is still travelling, a faint dash from the last stop
  // to the destination stamp — the part of the drive still to go.
  for (const r of state.routes) r.remove();
  state.routes = [];
  const draw = (hid, latlngs, cls, dash) => {
    if (latlngs.length < 2) return;
    const h = holidayById(hid);
    const { pieces, planes } = arcRoute(latlngs);
    for (const pl of planes) {
      const mark = L.marker(pl.at, { icon: planeIcon(h ? h.color : "#666", pl.deg), interactive: false, keyboard: false }).addTo(map);
      mark.tripId = hid;
      state.routes.push(mark);
    }
    const line = L.polyline(pieces, {
      color: h ? h.color : "#666",
      weight: 2.5, dashArray: dash, lineCap: "round", opacity: 0.8,
      interactive: false, // never steal taps from pins or the map
      className: "route-line " + cls,
    }).addTo(map);
    line.tripId = hid;
    state.routes.push(line);
  };
  const at = (s) => [s.pin.lat, s.pin.lng];
  for (const [hid, pins] of byTrip) {
    const h = holidayById(hid);
    const stops = tripStops(pins);
    const j = journey(h, stops);
    // the way-there line runs into the arrival pin, so the two legs join
    draw(hid, [...j.way, ...j.stay.slice(0, 1)].map(at), "route-way", "7 8");
    draw(hid, j.stay.map(at), "route-stay", "1 9");
    if (j.enRoute && stops.length) draw(hid, [at(stops[stops.length - 1]), [h.dest_lat, h.dest_lng]], "route-ahead", "7 8");
  }
  renderDests();
  applyFocus();
}

/* ---------- destination stamps ---------- */

// Where a trip is headed, from the place picked when it was started. It's
// drawn from the trip itself rather than stored as a pin: a pin would become
// chapter one of the story and the first stop of the route. The stamp is
// dashed until a pin lands near it, then it's inked: arrived.
function destIcon(h, arrived) {
  const div = el("div", "dest-stamp" + (arrived ? " arrived" : ""));
  div.style.setProperty("--c", h.color);
  div.appendChild(el("span", "ring"));
  div.appendChild(el("span", "dest-name", destShort(h)));
  return L.divIcon({ html: div.outerHTML, iconSize: [30, 30], iconAnchor: [15, 15] });
}

function renderDests() {
  const want = new Map();
  for (const h of state.holidays) {
    if (!hasDest(h) || state.hidden.has(h.id)) continue;
    const pins = state.pins.filter((p) => p.holiday_id === h.id);
    want.set(h.id, { h, arrived: journey(h, tripStops(pins)).arrived });
  }
  for (const [hid, m] of state.dests) {
    if (!want.has(hid)) { m.remove(); state.dests.delete(hid); }
  }
  for (const [hid, { h, arrived }] of want) {
    const sig = [h.color, h.dest_name, h.dest_lat, h.dest_lng, arrived].join("|");
    let m = state.dests.get(hid);
    if (!m) {
      m = L.marker([h.dest_lat, h.dest_lng], {
        icon: destIcon(h, arrived),
        zIndexOffset: -500, // the trip's own pins sit on top of its stamp
        keyboard: false,
        title: h.dest_name,
      }).addTo(map);
      m.tripId = hid;
      m.isDest = true;
      m.on("click", (e) => {
        // pin mode: the stamp mustn't swallow the tap that pins "we're here"
        if (state.placing) { placePin(e.latlng); return; }
        const owner = holidayById(m.tripId);
        if (owner) openStory(owner, "dest");
      });
      state.dests.set(hid, m);
    } else if (m.sig !== sig) {
      m.setLatLng([h.dest_lat, h.dest_lng]);
      m.setIcon(destIcon(h, arrived));
    }
    m.sig = sig;
  }
}

function pendingIcon(color) {
  const div = el("div", "manual-pin pending");
  div.style.setProperty("--c", color);
  return L.divIcon({ html: div.outerHTML, iconSize: [22, 22], iconAnchor: [11, 11], popupAnchor: [0, -11] });
}

function openPendingPopup(qp) {
  const box = el("div", "pop-form");
  if (qp.title) box.appendChild(el("p", "pop-title", qp.title));
  box.appendChild(el("p", "pop-coords", "Saved offline — syncs when you're back online"));
  const drop = el("button", null, "Discard pin");
  drop.onclick = () => {
    setPinQueue(pinQueue().filter((p) => p.qid !== qp.qid));
    map.closePopup();
    renderMarkers();
    toast("Offline pin discarded");
  };
  box.appendChild(drop);
  L.popup({ maxWidth: 260, minWidth: 180 }).setLatLng([qp.lat, qp.lng]).setContent(box).openOn(map);
}

/* ---------- focus: spotlight one trip, fade the rest ---------- */

// Classes are toggled on the live elements rather than re-rendering, so
// pins keep their position and don't replay the pop-in animation.
function applyFocus() {
  const activePin = story.activeEl ? story.activeEl.dataset.pinId : null;
  const activeDest = story.activeEl ? story.activeEl.dataset.dest : null;
  for (const layer of [...state.markers.values(), ...state.routes, ...state.dests.values()]) {
    const elm = layer.getElement && layer.getElement();
    if (!elm) continue;
    elm.classList.toggle("dimmed", state.focused !== null && layer.tripId !== state.focused);
    const lit = layer.isDest
      ? activeDest != null && String(layer.tripId) === activeDest
      : activePin != null && String(layer.pinId) === activePin;
    elm.classList.toggle("story-active", lit);
  }
}

function setFocus(id) {
  state.focused = id;
  applyFocus();
  renderSheet();
}

/* ---------- trip story: scroll the chapters, the map follows ---------- */

// How many thumbnails a chapter shows before deferring to the lightbox.
// Two rows of three: enough to recognise the stop, short enough that the
// next chapter is always within a screen.
const STORY_THUMBS = 6;

const story = { hid: null, loadObserver: null, activeEl: null, holdUntil: 0, photos: new Map() };

// A pin's photos, fetched once per story however many stops it has.
function pinPhotos(pinId) {
  if (!story.photos.has(pinId)) {
    const path = SHARE ? `/api/share/${SHARE}/pins/${pinId}/photos` : `/api/pins/${pinId}/photos`;
    const p = api("GET", path);
    p.catch(() => story.photos.delete(pinId)); // a failed fetch is tried again
    story.photos.set(pinId, p);
  }
  return story.photos.get(pinId);
}

// Centre the pin in the half of the screen the panel leaves visible: the
// map's true centre sits behind the panel, so the target is offset by half
// the panel's size (down past a bottom sheet, left past a side panel).
function storyFly(latlng) {
  const z = Math.max(map.getZoom(), 12);
  const p = map.project(latlng, z);
  const side = matchMedia("(min-width: 720px)").matches;
  const dx = side ? -Math.round($("story").offsetWidth / 2) : 0;
  const dy = side ? 0 : Math.round($("story").offsetHeight / 2);
  const center = map.unproject([p.x + dx, p.y + dy], z);
  if (matchMedia("(prefers-reduced-motion: reduce)").matches) map.setView(center, z);
  else map.flyTo(center, z, { duration: 0.9 });
}

function activateSection(sec) {
  if (story.activeEl === sec) return;
  if (story.activeEl) story.activeEl.classList.remove("active");
  story.activeEl = sec;
  sec.classList.add("active");
  applyFocus(); // lifts this chapter's pin above its siblings
  if (sec.dataset.lat) storyFly(L.latLng(+sec.dataset.lat, +sec.dataset.lng));
}

function loadStoryPhotos(sec) {
  const grid = sec.querySelector(".story-grid");
  if (!grid || grid.dataset.loaded) return;
  grid.dataset.loaded = "1";
  const { at, until } = grid.dataset;
  pinPhotos(grid.dataset.pin).then((all) => {
    // only this stop's photos: the same pin on another day is another chapter
    const photos = at ? all.filter((ph) => ph.taken_at >= at && ph.taken_at <= until) : all;
    grid.textContent = "";
    photos.slice(0, STORY_THUMBS).forEach((ph, i) => {
      grid.appendChild(thumbCell(ph, (e) => { e.stopPropagation(); openLightbox(photos, i, { aside: !SHARE }); }));
    });
    const more = sec.querySelector(".story-more");
    if (more) {
      more.disabled = false;
      more.textContent = photos.length === 1 ? "The photo" : `All ${photos.length} photos`;
      more.onclick = (e) => { e.stopPropagation(); openLightbox(photos, 0, { aside: !SHARE }); };
    }
  }).catch(() => { grid.textContent = ""; grid.appendChild(el("span", "pop-sub", "Couldn't load photos")); });
}

function openStory(h, pinId) {
  closeStory(true);
  story.hid = h.id;
  state.focused = h.id;
  document.body.classList.add("storying");
  setSheet(false);
  $("story-title").textContent = h.name;
  $("story").style.setProperty("--c", h.color); // the cover bar's edge and the leg marks
  const scroll = $("story-scroll");
  scroll.textContent = "";
  scroll.scrollTop = 0;
  // F6: who started the trip ('' on old rows and on a share: say nothing)
  if (h.created_by) scroll.appendChild(el("p", "story-by story-by-trip", `Trip started by ${h.created_by}`));

  story.photos = new Map();
  const pins = state.pins.filter((p) => p.holiday_id === h.id);
  const stops = tripStops(pins);
  const j = journey(h, stops);
  const way = wayPhotoIds();
  const secs = [];
  // Leg headings mark where the drive ends and the trip begins. They carry no
  // coordinates, so the scroll sync steps over them.
  const leg = (text, cls) => scroll.appendChild(el("h4", "story-leg " + cls, text));
  let day = null;
  for (const stop of stops) {
    const pin = stop.pin;
    // The story reads day by day: one entry stamp where each day begins, and
    // that day's stops beneath it — the date isn't repeated on every chapter.
    const d = tripDay(h, stop.at);
    if (d.key !== day) {
      day = d.key;
      const stamp = el("h4", "story-date");
      stamp.append(el("span", "sd-n", `Day ${d.n}`), el("span", "sd-date", d.label));
      scroll.appendChild(stamp);
    }
    if (stop === j.way[0]) leg(j.arrived ? `The way to ${destShort(h)}` : `On the way to ${destShort(h)}`, "leg-way");
    if (j.arrived && stop === j.stay[0]) leg(`Arrived at ${destShort(h)}`, "leg-arrived");
    const sec = el("section", "story-sec");
    if (j.way.includes(stop)) sec.classList.add("on-the-way");
    // a way-there photo has no pin to fly to, so the scroll sync steps over it
    if (!way.has(pin.id)) {
      sec.dataset.lat = pin.lat;
      sec.dataset.lng = pin.lng;
    }
    sec.dataset.pinId = pin.id;
    sec.dataset.stop = stop.key;
    if (stop.again) sec.appendChild(el("p", "story-day", "Back again"));
    const place = el("h3", "story-place");
    // D1: the same number the pin wears on the map
    place.append(el("span", "stop-num", String(stops.indexOf(stop) + 1)), pin.title || (pin.kind === "photo" ? "Photo stop" : "Pin"));
    sec.appendChild(place);
    const first = stops.find((s) => s.pin === pin) === stop;
    // a note belongs to its visit; a pin with no visits (or an old snapshot)
    // has only the pin's own note, said once
    const stopNote = stop.note !== undefined ? stop.note : first ? pin.note : "";
    if (stopNote) sec.appendChild(el("p", "story-note", stopNote));
    // F6: who dropped the pin, once, at its first stop
    if (pin.created_by && first) sec.appendChild(el("p", "story-by", `Added by ${pin.created_by}`));
    if (stop.n > 0) {
      // A chapter shows a contact strip, not the whole roll. Day 1 of a trip
      // can carry 20+ photos, and an uncapped grid made every chapter several
      // screens tall — you scrolled photographs instead of chapters, so the
      // map never got to fly between the stops. The rest are one tap away.
      const shown = Math.min(stop.n, STORY_THUMBS);
      const grid = el("div", "story-grid");
      grid.dataset.pin = pin.id;
      if (!stop.whole) { grid.dataset.at = stop.at; grid.dataset.until = stop.until; }
      // Placeholder cells reserve the grid's final height before the photos
      // arrive — chapters must not grow later, or the open-at-pin scroll (and
      // the reader's place) slides as content above them expands.
      for (let i = 0; i < shown; i++) grid.appendChild(el("span", "ph"));
      sec.appendChild(grid);
      if (stop.n > shown) {
        const more = el("button", "story-more", stop.n === 1 ? "The photo" : `All ${stop.n} photos`);
        more.type = "button";
        more.disabled = true; // enabled once the photos are in hand
        sec.appendChild(more);
      }
    }
    if (SHARE) { sec.onclick = () => activateSection(sec); scroll.appendChild(sec); secs.push(sec); continue; }
    const actions = el("div", "story-actions");
    const ed = el("button", null, "Edit");
    ed.onclick = (e) => {
      e.stopPropagation();
      sec.textContent = "";
      sec.appendChild(editForm(pin, stop));
      const cancel = el("button", "linkish", "Cancel");
      cancel.onclick = (ev) => { ev.stopPropagation(); refreshStory(); };
      sec.appendChild(cancel);
    };
    const del = el("button", null, "Delete");
    del.onclick = async (e) => {
      e.stopPropagation();
      const visits = stops.filter((s) => s.pin === pin).length;
      const what = visits > 1 ? `Delete this pin, and all ${visits} visits to it?` : "Delete this pin?";
      if (!confirm(pin.photo_count > 0 ? `${what} Its photos stay in Immich.` : what)) return;
      await busy(del, async () => {
        deletedToast("Pin", await api("DELETE", `/api/pins/${pin.id}`));
        loadData();
      });
    };
    actions.append(ed, del);
    sec.appendChild(actions);
    sec.onclick = () => activateSection(sec); // tapping a chapter flies there too
    scroll.appendChild(sec);
    secs.push(sec);
  }
  // Not there yet: the destination closes the story as the chapter still to
  // come, so a live or planned trip always shows where it's going.
  if (hasDest(h) && !j.arrived && !h.end_at) {
    const sec = el("section", "story-sec story-ahead");
    sec.dataset.lat = h.dest_lat;
    sec.dataset.lng = h.dest_lng;
    sec.dataset.dest = h.id;
    const last = stops.length ? stops[stops.length - 1].pin : null;
    const label = h.planned ? countdown(h)
      : last ? `Still to go · ${Math.round(kmBetween(last, { lat: h.dest_lat, lng: h.dest_lng }))} km as the crow flies`
      : "Heading here";
    sec.appendChild(el("p", "story-day", label));
    const short = destShort(h);
    sec.appendChild(el("h3", "story-place", short.charAt(0).toUpperCase() + short.slice(1)));
    if (h.dest_name !== destShort(h)) sec.appendChild(el("p", "story-dest-full", h.dest_name));
    sec.onclick = () => activateSection(sec);
    scroll.appendChild(sec);
    secs.push(sec);
  }
  // F3: every photo of the trip, day by day, one tap from the story
  const total = pins.reduce((n, p) => n + (p.photo_count || 0), 0) + (h.unplaced_count || 0);
  if (!SHARE && total > 0) {
    const sec = el("section", "story-sec story-gallery");
    sec.appendChild(el("h3", "story-place", "All photos"));
    const open = el("button", "sub-link", `See ${total === 1 ? "the photo" : `all ${total} photos`} day by day`);
    open.type = "button";
    open.onclick = () => openGallery(h);
    sec.appendChild(open);
    scroll.appendChild(sec);
  }
  if (h.journal) {
    const sec = el("section", "story-sec");
    sec.appendChild(el("h3", "story-place", "Journal"));
    sec.appendChild(el("p", "story-note", h.journal));
    if (h.journal_by) sec.appendChild(el("p", "story-by", `Written by ${h.journal_by}`));
    scroll.appendChild(sec);
  }
  if (h.unplaced_count > 0) {
    const sec = el("section", "story-sec");
    sec.appendChild(el("h3", "story-place", "Photos without a location"));
    const file = el("button", "sub-link", `file ${plural(h.unplaced_count, "photo")} onto pins`);
    file.onclick = () => openUnplaced(h);
    sec.appendChild(file);
    scroll.appendChild(sec);
  }
  if (h.ask_count > 0 && !SHARE) {
    const sec = el("section", "story-sec");
    sec.appendChild(el("h3", "story-place", "Photos near your pins"));
    const check = el("button", "sub-link", `file ${h.ask_count} ${h.ask_count === 1 ? "photo" : "photos"}`);
    check.onclick = () => openAsk(h);
    sec.appendChild(check);
    scroll.appendChild(sec);
  }
  if (h.set_aside_count > 0 && !SHARE) {
    const sec = el("section", "story-sec");
    sec.appendChild(el("h3", "story-place", "Set aside"));
    const n = h.set_aside_count;
    const open = el("button", "sub-link", `${n} ${n === 1 ? "photo" : "photos"} not on the map`);
    open.onclick = () => openSetAside(h);
    sec.appendChild(open);
    scroll.appendChild(sec);
  }
  if (!secs.length && !h.unplaced_count) {
    scroll.appendChild(el("p", "empty-note", "No pins on this trip yet — the story writes itself as you pin places."));
  }
  $("story").hidden = false;
  renderMarkers(); // D1: this trip's pins take their numbers, and come out of any cluster
  // F10: GPX tracks, logged-in only (a share never carries tracks)
  if (!SHARE && window.WSTracks) window.WSTracks.draw(h.id);
  applyFocus(); // dims the other trips even before a chapter activates
  renderSheet();

  // Photos fetch a screenful before they're read.
  story.loadObserver = new IntersectionObserver((entries) => {
    for (const e of entries) if (e.isIntersecting) { loadStoryPhotos(e.target); story.loadObserver.unobserve(e.target); }
  }, { root: scroll, rootMargin: "600px 0px" });
  for (const s of secs) story.loadObserver.observe(s);
  // "dest" = opened from the destination stamp: its chapter if the trip is
  // still heading there, otherwise the arrival
  const target = pinId === "dest"
    ? secs.find((s) => s.dataset.dest) || secs.find((s) => j.stay[0] && s.dataset.stop === j.stay[0].key)
    // a stop's own key (a refresh), or a pin id (a marker tap: its first visit)
    : pinId != null && (secs.find((s) => s.dataset.stop === String(pinId))
      || secs.find((s) => s.dataset.pinId === String(pinId)));
  if (target) {
    // Jump straight to the tapped pin's chapter. The scroll this causes must
    // not re-derive the active chapter (a bottom-clamped scroll would pick a
    // later one), so the sync handler holds off briefly.
    story.holdUntil = performance.now() + 600;
    // a leg heading just above the chapter comes into view with it
    const top = target.previousElementSibling?.classList.contains("story-leg") ? target.previousElementSibling : target;
    scroll.scrollTop = Math.max(0, top.offsetTop - scroll.offsetTop - 8);
    activateSection(target);
  } else if (secs.length) {
    onStoryScroll();
  }
}

// Rebuild the open story in place (after a pin edit/delete or data reload),
// keeping the reader's scroll position and active chapter — the map must not
// fly back to chapter one just because the data reloaded.
function refreshStory() {
  if (story.hid === null) return;
  const h = holidayById(story.hid);
  if (!h) { closeStory(); return; }
  const sc = $("story-scroll");
  const keep = sc.scrollTop;
  const act = story.activeEl;
  const activePin = act ? (act.dataset.dest ? "dest" : act.dataset.stop) : null;
  story.holdUntil = performance.now() + 600; // gates openStory's own sync call
  openStory(h, activePin);
  story.holdUntil = performance.now() + 600;
  sc.scrollTop = keep;
}

// Active chapter = the last one whose top has crossed a line 35% down the
// panel. Chapters are often shorter than that band, so the scroll ends are
// anchored explicitly: top of the scroll always reads as chapter one, the
// bottom as the final chapter.
function onStoryScroll() {
  if (performance.now() < story.holdUntil) return;
  const sc = $("story-scroll");
  const secs = [...sc.querySelectorAll(".story-sec[data-lat]")];
  if (!secs.length) return;
  let cur;
  if (sc.scrollTop <= 4) {
    cur = secs[0];
  } else if (sc.scrollTop + sc.clientHeight >= sc.scrollHeight - 4) {
    cur = secs[secs.length - 1];
  } else {
    const line = sc.getBoundingClientRect().top + sc.clientHeight * 0.35;
    for (const sec of secs) {
      if (sec.getBoundingClientRect().top <= line) cur = sec;
      else break;
    }
    cur = cur || secs[0];
  }
  activateSection(cur);
}
$("story-scroll").onscroll = () => {
  if (story.ticking) return;
  story.ticking = true;
  requestAnimationFrame(() => { story.ticking = false; if (story.hid !== null) onStoryScroll(); });
};

// reopening: openStory is about to show a story again (often the same one,
// on a data reload), so the routes and markers are left for it to redo
function closeStory(reopening) {
  if (story.hid === null) return;
  const again = reopening === true; // the close button passes an event
  story.loadObserver.disconnect();
  story.loadObserver = null;
  story.activeEl = null;
  story.hid = null;
  $("story").hidden = true;
  if (!again && !SHARE && window.WSTracks) window.WSTracks.draw(null); // takes the track off the map
  document.body.classList.remove("storying");
  if (!again) renderMarkers();
  setFocus(null); // also clears story-active via applyFocus
}

$("story-close").onclick = closeStory;

/* ---------- pin editing (lives in the story chapters) ---------- */

// repositionPopup re-measures and re-pans the open popup after its content
// changes size (Leaflet only does this on open). popup.update() would re-run
// the bound content function and wipe a swapped-in edit form, so call the
// layout/pan steps directly.
function repositionPopup() {
  const p = map._popup;
  if (p && p._updateLayout && p._adjustPan) { p._updateLayout(); p._adjustPan(); }
}

// coverPick renders a tappable thumb grid for choosing a cover photo.
// Quietly removes itself if the photos can't be fetched.
function coverPick(photosPromise, current, onPick) {
  const label = el("p", "form-label", "Cover photo");
  const grid = el("div", "cover-pick");
  photosPromise.then((photos) => {
    if (!photos.length) { label.remove(); grid.remove(); return; }
    photos.forEach((ph) => {
      const img = el("img");
      img.loading = "lazy";
      img.src = photoURL(ph.asset_id, "thumb");
      img.alt = "";
      if (ph.asset_id === current) img.classList.add("sel");
      img.onclick = () => {
        grid.querySelectorAll("img").forEach((x) => x.classList.remove("sel"));
        img.classList.add("sel");
        onPick(ph.asset_id);
      };
      grid.appendChild(img);
    });
    repositionPopup(); // the thumbs just grew the form
  }).catch(() => { label.remove(); grid.remove(); });
  const frag = document.createDocumentFragment();
  frag.append(label, grid);
  return frag;
}

// The days a pin's note can belong to, oldest first: one entry per local day
// (two visits on one day share a note), then notes left on a day with no visit.
function noteDays(pin) {
  const days = new Map();
  for (const v of pin.visits || []) {
    if (v.day && !days.has(v.day)) days.set(v.day, { day: v.day, note: v.note || "", gone: false });
  }
  for (const o of pin.other_notes || []) {
    if (!days.has(o.day)) days.set(o.day, { day: o.day, note: o.note, gone: true });
  }
  return [...days.values()];
}

function noteDayLabel(d) {
  const s = new Date(`${d.day}T12:00:00Z`).toLocaleDateString("en-GB", { weekday: "short", day: "numeric", month: "short", timeZone: "UTC" });
  return d.gone ? `${s} (no photos there now)` : s;
}

function editForm(pin, stop) {
  const form = el("form", "pop-form");
  const title = el("input");
  title.type = "text"; title.placeholder = "Title"; title.value = pin.title; title.maxLength = 80;
  const note = el("textarea");
  note.placeholder = "Notes"; note.value = pin.note;
  const save = el("button", "primary", "Save");
  save.type = "submit";
  let cover = pin.cover_asset;
  const days = noteDays(pin);
  const start = days.find((d) => stop && d.day === stop.day) || days[0];
  // the day the text in the box came from; saving under another day moves it
  let fromDay = start ? start.day : "";
  let loaded = start ? start.note : pin.note;
  note.value = loaded;
  const which = el("select", "note-day");
  which.setAttribute("aria-label", "Which visit is this note about?");
  for (const d of days) {
    const o = el("option", null, noteDayLabel(d));
    o.value = d.day;
    which.appendChild(o);
  }
  if (start) which.value = start.day;
  let picked = which.value;
  which.onchange = () => {
    const d = days.find((x) => x.day === which.value);
    if (d.note) {
      if (note.value !== loaded && !confirm("That day already has a note. Show it instead of what you typed?")) {
        which.value = picked; // back to the day the typed words were meant for
        return;
      }
      note.value = d.note;
      loaded = d.note;
      fromDay = d.day;
    }
    picked = which.value;
    // a day with no note: the text stays and moves there on Save
    moveHint();
  };
  const hint = el("p", "form-hint note-move");
  const moveHint = () => {
    const d = days.find((x) => x.day === fromDay);
    const moving = d && which.value !== fromDay && loaded && note.value === loaded;
    hint.textContent = moving ? `Moves the note from ${noteDayLabel(d)}` : "";
    hint.hidden = !moving;
  };
  note.addEventListener("input", moveHint);
  hint.hidden = true;
  form.append(title);
  if (days.length > 1) {
    const lab = el("label", "note-day-label", "Which visit?");
    lab.appendChild(which);
    form.append(lab, hint);
  }
  form.append(note);
  if (pin.photo_count > 0) {
    form.appendChild(coverPick(api("GET", `/api/pins/${pin.id}/photos`), pin.cover_asset, (a) => { cover = a; }));
  }
  form.appendChild(save);
  form.onsubmit = async (e) => {
    e.preventDefault();
    const body = { title: title.value };
    const day = days.length ? which.value : "";
    if (!day) {
      if (note.value !== pin.note) body.note = note.value;
    } else if (note.value !== loaded || day !== fromDay) {
      body.note = note.value;
      body.note_day = day;
      // moved only if it is still the same words; new words are a new note
      if (day !== fromDay && loaded && note.value === loaded) body.from_day = fromDay;
    }
    if (cover && cover !== pin.cover_asset) body.cover_asset = cover;
    await busy(submitter(e), async () => {
      await api("PATCH", `/api/pins/${pin.id}`, body);
      loadData(); // refreshStory rebuilds the chapter with the new values
      toast("Pin saved");
    });
  };
  return form;
}

/* ---------- place a pin ---------- */

function setPlacing(on) {
  state.placing = on;
  document.body.classList.toggle("placing", on);
  $("place-hint").hidden = !on;
}

function placePin(latlng) {
  setPlacing(false);
  if (!state.holidays.length) {
    toast("Start a holiday first — pins belong to trips");
    return;
  }
  const active = state.holidays.find((h) => h.active);
  const form = el("form", "pop-form");
  const title = el("input");
  title.type = "text"; title.placeholder = "What's here?"; title.maxLength = 80;
  const note = el("textarea");
  note.placeholder = "Notes (optional)";
  // Pins usually go on the live trip, but a forgotten place can be added
  // to any past holiday after the fact.
  const trip = el("select");
  for (const h of state.holidays) {
    const opt = el("option", null, h.active ? h.name + " (now)" : h.planned ? h.name + " (planned)" : h.name);
    opt.value = h.id;
    trip.appendChild(opt);
  }
  // With nothing live, the latest trip taken, not the furthest-off plan
  // (they sort first, by start date).
  trip.value = String((active || state.holidays.find((h) => !h.planned) || state.holidays[0]).id);
  const save = el("button", "primary", "Add pin");
  save.type = "submit";
  // Exactly where this pin lands; tap to copy (clipboard needs HTTPS, so it
  // may quietly do nothing on plain-HTTP LAN — the numbers stay readable).
  // WS-7: at low zoom the world repeats; a click on another copy is past ±180.
  // The popup stays where the tap was; the pin is saved on the one world.
  const lng = wrapLng(latlng.lng);
  const coords = el("p", "pop-coords", `${latlng.lat.toFixed(5)}, ${lng.toFixed(5)}`);
  coords.title = "Tap to copy the coordinates";
  coords.onclick = () => {
    navigator.clipboard?.writeText(coords.textContent)
      .then(() => toast("Coordinates copied"))
      .catch(() => {});
  };
  form.append(coords, title, note, trip, save);
  const clientId = newClientId(); // one per form: a second tap on Add pin is the same pin
  const popup = L.popup({ maxWidth: 300, minWidth: 220 }).setLatLng(latlng).setContent(form).openOn(map);
  title.focus();
  form.onsubmit = async (e) => {
    e.preventDefault();
    const pin = {
      holiday_id: Number(trip.value),
      lat: latlng.lat, lng,
      title: title.value, note: note.value,
      client_id: clientId,
    };
    // created_at rides along so a pin synced hours later still lands on the
    // right day of the trip's story
    const offline = () => {
      queuePin({ ...pin, created_at: new Date().toISOString() });
      map.closePopup(popup);
      toast("No signal — pin saved, will sync when you're back online");
    };
    if (!navigator.onLine) { offline(); return; } // known-dead link: skip the doomed request
    await busy(submitter(e), async () => {
      try {
        await api("POST", "/api/pins", pin);
      } catch (err) {
        if (!isNetworkError(err)) throw err;
        offline();
        return;
      }
      map.closePopup(popup);
      loadData();
      toast("Pin added");
    });
  };
}

$("fab-pin").onclick = () => setPlacing(!state.placing);

// Pin exactly where the phone says you are — no map-hunting mid-holiday.
$("fab-gps").onclick = () => {
  if (!navigator.geolocation) {
    toast("This device can't share its location");
    return;
  }
  setPlacing(false);
  toast("Finding your location…");
  navigator.geolocation.getCurrentPosition(
    (pos) => {
      const latlng = L.latLng(pos.coords.latitude, pos.coords.longitude);
      map.setView(latlng, 15);
      placePin(latlng);
    },
    (err) => {
      toast(err.code === err.PERMISSION_DENIED
        ? "Location permission denied — enable it for this site"
        : "Couldn't get your location");
    },
    { enableHighAccuracy: true, timeout: 10000, maximumAge: 30000 }
  );
};

/* ---------- busy buttons ---------- */

// One tap, one request: the button is off while its request runs, so a
// double tap can't make two pins or two trips, and a failure is said out
// loud instead of being swallowed. label, if given, shows while it runs.
async function busy(btn, fn, label) {
  if (btn) {
    if (btn.disabled) return;
    btn.disabled = true;
    btn.setAttribute("aria-busy", "true");
  }
  const old = btn && label ? btn.textContent : null;
  if (old !== null) btn.textContent = label;
  // A form being saved still holds what was typed: it stays "dirty" (see
  // editing()) until the save worked, so a reload while it runs, or after it
  // failed, doesn't wipe it.
  // "saving" turns back to "1" if more is typed meanwhile (the input
  // listener), and then the form stays dirty after the save too.
  const form = btn && btn.type === "submit" ? btn.form : null;
  if (form) form.dataset.dirty = "saving";
  try {
    const r = await fn();
    if (form && form.dataset.dirty === "saving") delete form.dataset.dirty;
    return r;
  } catch (err) {
    if (form) form.dataset.dirty = "1";
    toast(err && err.message ? err.message : "Something went wrong");
  } finally {
    if (btn) {
      btn.disabled = false;
      btn.removeAttribute("aria-busy");
      if (old !== null) btn.textContent = old;
    }
  }
}

const submitter = (e) => e.submitter || e.target.querySelector("[type=submit]");

/* ---------- trips sheet ---------- */

const DATE_OPTS = { day: "numeric", month: "short", year: "numeric" };

function fmtDate(s) {
  return new Date(s).toLocaleDateString(undefined, DATE_OPTS);
}

// A trip's start and end are calendar days when they were picked as dates:
// the server stores those as 00:00:00Z / 23:59:59Z of that date. Read in the
// viewer's timezone they'd move a day (BST turns 24 May 23:59Z into 25 May),
// so those are taken from their date part. A real moment (a trip started or
// ended with the button) stays a moment, shown in local time.
function calDay(s) {
  if (/^\d{4}-\d{2}-\d{2}(T(00:00:00|23:59:59)(\.\d+)?(Z|[+-]00:00))?$/.test(s)) {
    const [y, m, d] = s.slice(0, 10).split("-").map(Number);
    return new Date(y, m - 1, d);
  }
  return new Date(s);
}

function fmtTripDay(s) {
  return calDay(s).toLocaleDateString(undefined, DATE_OPTS);
}

// Days away, counted on the calendar: a trip from the 23rd to the 24th is 2.
function tripDays(h) {
  const a = calDay(h.start_at);
  const b = h.end_at ? calDay(h.end_at) : new Date();
  const day = (d) => Date.UTC(d.getFullYear(), d.getMonth(), d.getDate());
  return Math.max(1, Math.round((day(b) - day(a)) / 86400000) + 1);
}

// A date range says the shared parts once. Intl does this per-locale — an
// elision written by hand ("23 – May 25, 2026") only reads correctly where
// the day precedes the month, which is not something to assume.
const rangeFmt = (() => {
  try {
    const f = new Intl.DateTimeFormat(undefined, { day: "numeric", month: "short", year: "numeric" });
    return typeof f.formatRange === "function" ? f : null;
  } catch { return null; }
})();

function fmtRange(a, b) {
  const s = calDay(a), e = calDay(b);
  if (isNaN(s) || isNaN(e)) return fmtTripDay(a);
  if (rangeFmt) return rangeFmt.formatRange(s, e);
  return `${fmtTripDay(a)} – ${fmtTripDay(b)}`;
}

// Whole days until a planned trip's first day, counted on the calendar in
// the viewer's own timezone. start_at is midnight UTC of the chosen date, so
// only its date part is the departure; round, because a day with a DST change
// is 23 or 25 hours long.
function departureDay(h) {
  const [y, m, d] = h.start_at.slice(0, 10).split("-").map(Number);
  return new Date(y, m - 1, d);
}

function daysUntil(h) {
  const today = new Date();
  today.setHours(0, 0, 0, 0);
  return Math.round((departureDay(h) - today) / 86400000);
}

function countdown(h) {
  const n = daysUntil(h);
  return n > 0 ? `${n} ${n === 1 ? "day" : "days"} to go` : "Departs today";
}

// A planned trip's line under its name. Once its day has come it can still be
// planned: another trip is live (it goes the moment that one ends), or it's
// the hour between local and UTC midnight.
function plannedMeta(h) {
  // the departure is a calendar day, so it's formatted as one: fmtDate would
  // shift UTC midnight into the day before anywhere west of Greenwich
  const date = departureDay(h).toLocaleDateString(undefined, { day: "numeric", month: "short", year: "numeric" });
  if (daysUntil(h) > 0) return `Departs ${date} · ${countdown(h)}`;
  const live = state.holidays.find((t) => t.active);
  return live ? `Due ${date} · goes live when ${live.name} ends` : "Departs today";
}

function tripRange(h) {
  if (h.planned) return plannedMeta(h);
  if (h.active) return `since ${fmtTripDay(h.start_at)}`;
  return fmtRange(h.start_at, h.end_at);
}

/* ---------- finding a trip (F1) ---------- */

// Hidden trips stay hidden across reloads, on this device. Per user, so a
// shared tablet keeps each person's own map. Shares never use it.
let hiddenFor = null; // whose hidden set state.hidden holds
const hiddenKey = (who) => `hm-hidden:${who}`;
function loadHidden(who) {
  if (SHARE || !who || who === hiddenFor) return;
  hiddenFor = who;
  try {
    const ids = JSON.parse(localStorage.getItem(hiddenKey(who)) || "[]");
    state.hidden = new Set(Array.isArray(ids) ? ids.filter(Number.isInteger) : []);
  } catch { state.hidden = new Set(); }
}
function saveHidden() {
  if (SHARE || !hiddenFor) return;
  try { localStorage.setItem(hiddenKey(hiddenFor), JSON.stringify([...state.hidden])); } catch {}
}

// The search box and year chips only show once a list is long enough to need them.
const FIND_FROM = 5;

// Lower case without accents, so "cote" finds "Côte d'Azur".
const fold = (s) => String(s || "").normalize("NFD").replace(/[\u0300-\u036f]/g, "").toLowerCase();

// The calendar years a trip touches; a live trip runs to this year.
function tripYears(h) {
  const a = calDay(h.start_at).getFullYear();
  const b = h.end_at ? calDay(h.end_at).getFullYear() : Math.max(a, new Date().getFullYear());
  const ys = [];
  for (let y = a; y <= b && ys.length < 50; y++) ys.push(y);
  return ys;
}

function tripMatches(h) {
  if (state.year !== null && !tripYears(h).includes(state.year)) return false;
  const q = fold(state.query).trim();
  if (!q) return true;
  const hay = fold([h.name, h.dest_name, h.journal].join(" "));
  return q.split(/\s+/).every((w) => hay.includes(w));
}

function renderFind() {
  const box = $("trip-find");
  const on = state.holidays.length >= FIND_FROM;
  box.hidden = !on;
  if (!on) { state.query = ""; state.year = null; $("trip-search").value = ""; return; }
  const years = [...new Set(state.holidays.flatMap(tripYears))].sort((a, b) => b - a);
  if (state.year !== null && !years.includes(state.year)) state.year = null;
  const chips = $("year-chips");
  chips.textContent = "";
  for (const y of [null, ...years]) {
    const b = el("button", "chip", y === null ? "All" : String(y));
    b.type = "button";
    b.setAttribute("aria-pressed", String(state.year === y));
    b.onclick = () => { state.year = y; renderSheet(); };
    chips.appendChild(b);
  }
}

function renderSheet() {
  const dots = $("pill-dots");
  dots.textContent = "";
  state.holidays.slice(0, 6).forEach((h) => {
    const i = el("i");
    i.style.background = h.color;
    dots.appendChild(i);
  });
  $("pill-label").textContent = state.holidays.length ? plural(state.holidays.length, "trip") : "Trips";

  const list = $("trip-list");
  // An edit form that's open but untouched is rebuilt with the fresh data,
  // not closed: a background photo sync shouldn't shut it under your finger.
  const openForm = list.querySelector("form.trip-edit");
  const reopenId = openForm ? Number(openForm.dataset.hid) : null;
  list.textContent = "";
  if (!state.holidays.length) {
    // the sheet's first page: the same blank stamp the passport shows
    const blank = el("div", "sheet-blank");
    blank.appendChild(el("div", "stamp-blank", "No entries yet"));
    blank.appendChild(el("p", "form-hint",
      "Start a holiday as you set off, or add a past trip by giving it both dates."));
    list.appendChild(blank);
  }
  renderFind();
  const shown = state.holidays.filter(tripMatches);
  if (state.holidays.length && !shown.length) {
    const none = el("div", "sheet-none");
    none.appendChild(el("p", "form-hint", "No trips match."));
    const clear = el("button", "", "Show all trips");
    clear.type = "button";
    clear.onclick = () => { state.query = ""; state.year = null; $("trip-search").value = ""; renderSheet(); };
    none.appendChild(clear);
    list.appendChild(none);
  }
  for (const h of shown) {
    const row = el("div", "trip-row"
      + (state.hidden.has(h.id) ? " hidden-trip" : "")
      + (state.focused === h.id ? " focused-trip" : ""));
    row.style.setProperty("--c", h.color);
    // Cover = the trip's earliest photo, ringed in its colour; falls back to
    // a plain colour dot for trips with no photos yet.
    const cover = el("span", "trip-cover");
    cover.style.setProperty("--c", h.color);
    if (h.planned) {
      // No photos yet, so the slot they'll fill counts down to them instead.
      const n = daysUntil(h);
      cover.classList.add("countdown");
      cover.appendChild(el("span", "countdown-n", n > 0 ? String(n) : "\u2708"));
      cover.setAttribute("aria-hidden", "true"); // the meta line says it in words
    } else if (h.cover_asset) {
      const img = el("img");
      img.loading = "lazy";
      img.src = photoURL(h.cover_asset, "thumb");
      img.alt = "";
      cover.appendChild(img);
    } else {
      cover.classList.add("no-cover");
    }
    const info = el("div", "trip-info");
    const name = el("div", "trip-name", h.name + " ");
    if (h.active) {
      const live = el("span", "live", "NOW");
      live.style.background = h.color;
      name.appendChild(live);
    }
    info.append(name, el("div", "trip-meta", h.planned
      ? tripRange(h)
      : [tripRange(h), h.pin_count && plural(h.pin_count, "pin"), h.photo_count && plural(h.photo_count, "photo")]
        .filter(Boolean).join(" · ")));

    // The cover + name is the trip's own control. It used to be a click
    // handler on the row div, which no keyboard could ever reach.
    const main = el("button", "trip-main");
    main.type = "button";
    main.append(cover, info);

    // The pill links get their own line, so the name is no longer competing
    // with them (and with ✎/👁) for one row's width.
    const sub = el("div", "trip-sub");
    if (h.pin_count > 0 || h.unplaced_count > 0) {
      const st = el("button", "sub-link", "story");
      st.onclick = (e) => { e.stopPropagation(); openStory(h); };
      sub.append(st);
    }
    if (h.ask_count > 0 && !SHARE) {
      const ask = el("button", "sub-link", `${h.ask_count} to file`);
      ask.onclick = (e) => { e.stopPropagation(); openAsk(h); };
      sub.append(ask);
    }
    if (h.unplaced_count > 0) {
      const un = el("button", "sub-link", `${h.unplaced_count} without location`);
      un.onclick = (e) => { e.stopPropagation(); openUnplaced(h); };
      sub.append(un);
    }
    if (h.journal) {
      const jr = el("button", "sub-link", "journal");
      jr.onclick = (e) => { e.stopPropagation(); openJournal(h); };
      sub.append(jr);
    }
    const mfLeft = (h.pack_total || 0) - (h.pack_done || 0);
    const mf = el("button", "sub-link",
      !h.pack_total ? "manifest" : mfLeft ? `manifest · ${mfLeft} to pack` : "manifest ✓");
    mf.onclick = (e) => { e.stopPropagation(); openManifest(h); };
    sub.append(mf);

    const edit = el("button", "trip-eye", "✎");
    edit.type = "button";
    edit.title = "Edit this trip";
    edit.setAttribute("aria-label", `Edit ${h.name}`);
    edit.onclick = (e) => {
      e.stopPropagation();
      const existing = row.nextElementSibling;
      if (existing && existing.classList.contains("trip-edit")) {
        existing.remove();
      } else {
        list.querySelectorAll(".trip-edit").forEach((x) => x.remove());
        row.after(tripEditForm(h));
      }
    };
    const eye = el("button", "trip-eye", state.hidden.has(h.id) ? "🚫" : "👁");
    eye.type = "button";
    eye.title = "Show or hide this trip's pins";
    eye.setAttribute("aria-label", `Show or hide ${h.name} on the map`);
    eye.setAttribute("aria-pressed", String(!state.hidden.has(h.id)));
    eye.onclick = (e) => {
      e.stopPropagation();
      state.hidden.has(h.id) ? state.hidden.delete(h.id) : state.hidden.add(h.id);
      saveHidden();
      if (state.hidden.has(h.id) && state.focused === h.id) state.focused = null;
      renderMarkers();
      renderSheet();
    };
    row.append(main, edit, eye, sub);
    main.onclick = () => {
      // First tap spotlights the trip (others fade) and flies to it;
      // tapping the spotlit row again releases the focus.
      if (state.focused === h.id) {
        setFocus(null);
        return;
      }
      const pts = mapPins(state.pins.filter((p) => p.holiday_id === h.id)).map((p) => [p.lat, p.lng]);
      if (pts.length && h.dest_name) pts.push([h.dest_lat, h.dest_lng]); // the whole journey, there and about
      if (pts.length) {
        setFocus(h.id);
        map.fitBounds(pts, { padding: [50, 50], maxZoom: 13 });
        setSheet(false);
      } else if (h.dest_name) {
        // no pins yet, but the trip knows where it's going
        map.flyTo([h.dest_lat, h.dest_lng], 10);
        setSheet(false);
      } else {
        toast("No pins on this trip yet");
      }
    };
    list.appendChild(row);
    if (h.id === reopenId) row.after(tripEditForm(h));
  }
}

function tripEditForm(h) {
  const wrap = el("form", "trip-edit");
  wrap.dataset.hid = h.id;
  const name = el("input");
  name.type = "text"; name.value = h.name; name.maxLength = 60; name.required = true;
  const sw = el("div");
  sw.className = "swatch-row";
  let color = h.color;
  makeSwatches(sw, h.color, (c) => { color = c; });
  const start = el("input");
  start.type = "date"; start.value = h.start_at.slice(0, 10);
  const startRow = el("label", "date-row", "First day ");
  startRow.appendChild(start);
  const end = el("input");
  end.type = "date"; end.value = h.end_at ? h.end_at.slice(0, 10) : "";
  const endRow = el("label", "date-row", "Last day ");
  endRow.appendChild(end);
  // Destination: where the stamp goes and what "arrived" means. Set here for
  // trips started without one, changed when plans change, or removed.
  // undefined = leave as is; null = remove; an object = the new place.
  let newDest;
  const destBox = el("div", "dest-edit");
  const destNow = el("p", "form-hint");
  const destRule = el("p", "form-hint");
  const destRemove = el("button", "linkish", "Remove destination");
  destRemove.type = "button";
  const showDest = () => {
    const d = newDest === undefined ? (h.dest_name ? { name: h.dest_name } : null) : newDest;
    destNow.textContent = d ? `Destination: ${d.name}` : "No destination yet";
    // what "arrived" will mean, so a city trip isn't a surprise
    const whole = newDest === undefined ? h.dest_area : !!(newDest && newDest.osm);
    destRule.textContent = !d ? "" : whole
      ? "Arrival: your first pin inside its boundary, or within 5 km"
      : "Arrival: your first pin within 5 km of it";
    destRule.hidden = !d;
    destRemove.hidden = !d;
  };
  destRemove.onclick = () => { newDest = null; destRes.textContent = ""; showDest(); };
  const destRow = el("div", "form-row");
  const destQ = el("input");
  destQ.type = "text"; destQ.maxLength = 120; destQ.autocomplete = "off";
  destQ.placeholder = h.dest_name ? "Change to… place or lat, lng" : "Where to? Place or lat, lng";
  destQ.setAttribute("aria-label", "Destination");
  const destFind = el("button", null, "Find");
  destFind.type = "button";
  const destRes = el("div", "dest-results");
  const findDest = () => placeSearch(destQ.value, destRes, (d) => { if (d) { newDest = d; showDest(); } });
  destFind.onclick = findDest;
  destQ.addEventListener("keydown", (e) => { if (e.key === "Enter") { e.preventDefault(); findDest(); } });
  destRow.append(destQ, destFind);
  destBox.append(destNow, destRule, destRow, destRes, destRemove);
  showDest();
  const journal = el("textarea");
  journal.placeholder = "Trip journal — the stories the photos don't tell";
  journal.value = h.journal;
  const save = el("button", "primary", "Save");
  save.type = "submit";
  const del = el("button", "danger", "Delete trip");
  del.type = "button";
  del.onclick = async () => {
    if (!confirm(`Delete "${h.name}" and all its pins? Photos stay in Immich.`)) return;
    await busy(del, async () => {
      deletedToast("Trip", await api("DELETE", `/api/holidays/${h.id}`));
      loadData();
    });
  };
  const btnRow = el("div", "form-row");
  btnRow.append(save, del);
  // F2: an ended trip can be live again (ended by mistake, or one more day).
  if (h.end_at && !h.active && !h.planned) {
    const reopen = el("button", null, "Reopen trip");
    reopen.type = "button";
    reopen.onclick = async () => {
      const live = state.holidays.find((x) => x.active);
      if (live) { toast(`End "${live.name}" first: one trip is live at a time`); return; }
      if (!confirm(`Reopen "${h.name}"? New photos will join it until you end it again.`)) return;
      await busy(reopen, async () => {
        await api("POST", `/api/holidays/${h.id}/reopen`);
        toast(`"${h.name}" is live again`);
        await loadData();
      });
    };
    btnRow.appendChild(reopen);
  }
  let cover = h.cover_asset;
  // A live or planned trip gets its last day by being ended.
  wrap.append(name, sw, startRow, h.active || h.planned ? el("span") : endRow, destBox);
  if (h.photo_count > 0 || h.unplaced_count > 0) {
    wrap.appendChild(coverPick(api("GET", `/api/holidays/${h.id}/timeline`), h.cover_asset, (a) => { cover = a; }));
  }
  const shareRow = el("div", "share-row");
  if (h.shared) {
    shareRow.appendChild(el("p", "share-state", h.share_expires_at
      ? `Shared · the link works until ${fmtDate(h.share_expires_at)}`
      : "Shared · the link never expires"));
  }
  const mkShare = el("button", "linkish", h.shared ? "New share link" : "Share this trip — view-only link");
  mkShare.type = "button";
  // how long a new link works; the server takes 7, 30, 90, 365 or 0 (never)
  const life = el("select", "share-life");
  life.setAttribute("aria-label", "How long the link works");
  for (const [days, label] of [[30, "for 30 days"], [7, "for a week"], [90, "for 3 months"], [365, "for a year"], [0, "with no end date"]]) {
    const o = el("option", null, label);
    o.value = String(days);
    life.appendChild(o);
  }
  mkShare.onclick = () => {
    if (h.shared && !confirm("Make a new link? The old link stops working for everyone who has it.")) return;
    return busy(mkShare, async () => {
      const res = await api("POST", `/api/holidays/${h.id}/share`, { days: Number(life.value) });
      const url = location.origin + res.url;
      const until = res.expires_at ? ` It works until ${fmtDate(res.expires_at)}.` : "";
      try {
        await navigator.clipboard.writeText(url);
        toast(`View-only link copied — anyone with it can see this trip.${until}`);
      } catch {
        window.prompt("Copy the view-only link:", url);
      }
      loadData();
    });
  };
  const mkWrap = el("span", "share-make");
  mkWrap.append(mkShare, life);
  shareRow.appendChild(mkWrap);
  if (h.shared) {
    const revoke = el("button", "linkish", "Revoke share link");
    revoke.type = "button";
    revoke.onclick = () => busy(revoke, async () => {
      await api("DELETE", `/api/holidays/${h.id}/share`);
      toast("Share link revoked");
      loadData();
    });
    shareRow.appendChild(revoke);
  }
  // nothing to show anyone until the trip has begun
  wrap.append(journal, h.planned ? el("span") : shareRow, btnRow);
  wrap.onsubmit = async (e) => {
    e.preventDefault();
    // Dates go out only when they changed: a bare date means midnight (or
    // 23:59:59 for the end), so re-sending it on every rename would drag the
    // exact start/end moments to the day's edges and widen the photo window.
    const body = { name: name.value, color, journal: journal.value };
    const startChanged = start.value !== h.start_at.slice(0, 10);
    const endChanged = !h.active && !h.planned && end.value && end.value !== (h.end_at || "").slice(0, 10);
    if (startChanged) body.start_at = start.value;
    if (endChanged) body.end_at = end.value;
    if (cover && cover !== h.cover_asset) body.cover_asset = cover;
    if (newDest !== undefined) Object.assign(body, destBody(newDest));
    await busy(submitter(e), async () => {
      await api("PATCH", `/api/holidays/${h.id}`, body);
      toast("Trip saved");
      const dates = startChanged || endChanged;
      // Saved: the form closes, unless more was typed while it saved.
      if (wrap.dataset.dirty === "saving") wrap.remove();
      loadData();
      if (dates) api("POST", `/api/holidays/${h.id}/sync`).then(() => loadData()).catch(() => {});
    });
  };
  return wrap;
}

/* ---------- overlays: passport, unplaced, journal ---------- */

// Modal plumbing shared by the overlay and the lightbox. Both cover the whole
// screen, so the page behind them is marked inert (keyboard and screen reader
// skip it), focus moves in, and on close it returns to whatever opened it —
// otherwise closing a dialog dumps you back at the top of the document.
const BEHIND = ["map", "banner", "fabs", "sheet", "place-hint", "story"];
let modalDepth = 0;
const returnFocus = [];

function modalOpen(id, focusEl) {
  returnFocus.push(document.activeElement);
  if (modalDepth === 0) for (const b of BEHIND) $(b).inert = true;
  modalDepth++;
  $(id).hidden = false;
  if (focusEl) focusEl.focus();
}

function modalClose(id) {
  $(id).hidden = true;
  modalDepth = Math.max(0, modalDepth - 1);
  if (modalDepth === 0) for (const b of BEHIND) $(b).inert = false;
  const prev = returnFocus.pop();
  if (prev && prev.isConnected) prev.focus();
}

function openOverlay(title, contentNode) {
  $("overlay-title").textContent = title;
  const c = $("overlay-content");
  c.textContent = "";
  c.appendChild(contentNode);
  // a page that redraws itself (packing lists, family) re-opens in place;
  // opening it again would stack a second modal level that one close can't undo
  if ($("overlay").hidden) modalOpen("overlay", $("overlay-close"));
}

function closeOverlay() {
  if (!$("overlay").hidden) modalClose("overlay");
}
$("overlay-close").onclick = closeOverlay;

$("btn-passport").onclick = async () => {
  try {
    const stamps = await api("GET", "/api/stamps");
    // An open booklet: the data page on the left, the visa page facing it.
    // On a phone the two pages stack, and each keeps its own stitched edge.
    const spread = el("div", "pp-spread");
    const page = el("div", "passport pp-data");
    const visas = el("div", "passport pp-visas");
    spread.append(page, visas);

    // a planned trip hasn't been anywhere yet
    const taken = state.holidays.filter((h) => !h.planned);
    const days = taken.reduce((sum, h) => sum + tripDays(h), 0);
    const countries = new Set(stamps.map((s) => s.country));
    const places = state.holidays.reduce((n, h) => n + h.pin_count, 0);

    // The data page. Passports set their fields as label/value pairs, so these
    // read as fields rather than as a row of dashboard stat tiles.
    const head = el("div", "pp-head");
    head.appendChild(el("div", "pp-crest", "\u2708"));
    const fields = el("dl", "pp-fields");
    for (const [k, v] of [
      ["Holder", state.me ? state.me.username : "\u2014"],
      ["Trips", String(taken.length)],
      ["Countries", String(countries.size)],
      ["Days away", String(days)],
      ["Places pinned", String(places)],
    ]) {
      fields.appendChild(el("dt", "pp-key", k));
      fields.appendChild(el("dd", "pp-val", v));
    }
    head.appendChild(fields);
    page.appendChild(head);

    // The machine-readable zone sits at the foot of the data page, as it does
    // in a real passport, built from the record it actually describes.
    const mrz = (t) => t.toUpperCase().replace(/[^A-Z0-9]+/g, "<").slice(0, 44).padEnd(44, "<");
    const holder = state.me ? state.me.username : "traveller";
    page.appendChild(el("p", "pp-mrz",
      mrz("P<GBR<" + holder) + "\n" +
      mrz(countries.size + " countries " + taken.length + " trips " + days + " days")));

    visas.appendChild(el("h3", "pp-runhead", "Visas"));
    const wrap = el("div", "stamp-grid");
    stamps.forEach((s, i) => {
      const card = el("button", "stamp-card");
      card.style.setProperty("--c", s.color);
      card.style.setProperty("--r", ((i % 5) - 2) * 1.6 + "deg");
      card.style.setProperty("--i", i % 12);
      card.appendChild(el("div", "stamp-country", s.country));
      card.appendChild(el("div", "stamp-trip", s.name));
      card.appendChild(el("div", "stamp-admit", "Admitted \u00b7 " + fmtTripDay(s.start_at)));
      card.title = "Open this trip's story";
      card.onclick = () => {
        const h = holidayById(s.holiday_id);
        if (!h) return;
        closeOverlay();
        openStory(h);
      };
      wrap.appendChild(card);
    });
    if (!stamps.length) {
      wrap.appendChild(el("div", "stamp-blank", "Awaiting first entry"));
    }
    visas.appendChild(wrap);
    if (!stamps.length) {
      visas.appendChild(el("p", "pp-empty",
        "A country is stamped here once a trip has photos with a location on them."));
    }
    // F4: linus's stats module, when it's loaded
    if (window.WSStats) {
      const more = el("button", "sub-link pp-stats", "Travel stats");
      more.type = "button";
      more.onclick = () => window.WSStats.open();
      visas.appendChild(more);
    }
    openOverlay("Passport", spread);
  } catch (err) {
    toast(err.message);
  }
};

// dayGroupedGallery lays photos out in taken order under day headings.
// onClick(photos, index) handles a tap; if absent, taps open the lightbox.
// With a trip (h), the headings are its days ("Day 2 · Sunday 24 May"), cut
// at the same 4 am line as the story.
function dayGroupedGallery(photos, onClick, h) {
  const wrap = el("div");
  let lastDay = "";
  let grid = null;
  photos.forEach((ph, i) => {
    const d = h ? tripDay(h, ph.taken_at) : null;
    const day = d ? d.key : fmtDate(ph.taken_at);
    if (day !== lastDay) {
      const head = el("h3", "day-head", d ? `Day ${d.n} \u00B7 ${d.label}` : day);
      wrap.appendChild(head);
      grid = el("div", "gallery-grid");
      wrap.appendChild(grid);
      lastDay = day;
    }
    grid.appendChild(thumbCell(ph, (_e, img) => (onClick ? onClick(photos, i, img) : openLightbox(photos, i))));
  });
  return wrap;
}

// F3: the trip's whole roll (pinned and unplaced), in taken order
async function openGallery(h) {
  try {
    const photos = await api("GET", `/api/holidays/${h.id}/timeline`);
    const wrap = el("div", "trip-gallery");
    if (!photos.length) wrap.appendChild(el("p", "empty-note", "No photos on this trip yet."));
    else wrap.appendChild(dayGroupedGallery(photos, (all, i) => openLightbox(all, i, { aside: true }), h));
    openOverlay(`Photos \u00B7 ${h.name}`, wrap);
  } catch (err) {
    toast(err.message);
  }
}

async function openUnplaced(h) {
  try {
    const photos = await api("GET", `/api/holidays/${h.id}/unplaced`);
    const selected = new Set();
    const wrap = el("div");

    const bar = el("div", "attach-bar");
    const hint = el("span", "attach-hint", "Tap photos, then pick the pin they belong to.");
    const pinPick = el("select");
    const noOpt = el("option", null, "Choose a pin…");
    noOpt.value = "";
    pinPick.appendChild(noOpt);
    state.pins.filter((p) => p.holiday_id === h.id).forEach((p) => {
      const opt = el("option", null, p.title || (p.kind === "photo" ? "Photo stop" : "Pin"));
      opt.value = p.id;
      pinPick.appendChild(opt);
    });
    const attachBtn = el("button", "primary", "Attach");
    attachBtn.disabled = true;
    bar.append(hint, pinPick, attachBtn);

    const gallery = dayGroupedGallery(photos, (_photos, _i, img) => {
      const id = img.dataset.asset;
      if (selected.has(id)) { selected.delete(id); img.classList.remove("sel"); }
      else { selected.add(id); img.classList.add("sel"); }
      hint.textContent = selected.size ? `${selected.size} selected` : "Tap photos, then pick the pin they belong to.";
      attachBtn.disabled = selected.size === 0 || !pinPick.value;
    });
    pinPick.onchange = () => { attachBtn.disabled = selected.size === 0 || !pinPick.value; };
    attachBtn.onclick = async () => {
      try {
        const r = await api("POST", `/api/pins/${pinPick.value}/attach`, { asset_ids: [...selected] });
        toast(`Moved ${plural(r.moved, "photo")} onto the pin`);
        closeOverlay();
        loadData();
      } catch (err) {
        toast(err.message);
      }
    };

    wrap.append(bar, gallery);
    if (!state.pins.some((p) => p.holiday_id === h.id)) {
      hint.textContent = "Drop a pin on this trip first, then you can file these photos onto it.";
      bar.querySelector("select").hidden = true;
      attachBtn.hidden = true;
    }
    openOverlay(`${h.name} — photos without a location`, wrap);
  } catch (err) {
    toast(err.message);
  }
}

// The photos taken off a trip with "Not for the map". Tap the ones to bring
// back; a fresh sync files them as if they'd never left.
async function openSetAside(h) {
  try {
    const photos = await api("GET", `/api/holidays/${h.id}/aside`);
    const selected = new Set();
    const wrap = el("div");
    const bar = el("div", "attach-bar");
    const idle = "Kept off the map; still in Immich. Tap any to put back.";
    const hint = el("span", "attach-hint", idle);
    const back = el("button", "primary", "Put back");
    back.disabled = true;
    bar.append(hint, back);
    const gallery = dayGroupedGallery(photos, (_photos, _i, img) => {
      const id = img.dataset.asset;
      if (selected.has(id)) { selected.delete(id); img.classList.remove("sel"); }
      else { selected.add(id); img.classList.add("sel"); }
      hint.textContent = selected.size ? `${selected.size} selected` : idle;
      back.disabled = selected.size === 0;
    });
    back.onclick = async () => {
      back.disabled = true;
      try {
        for (const id of selected) await api("DELETE", `/api/photos/${id}/aside`);
        toast(`Put back ${selected.size} ${selected.size === 1 ? "photo" : "photos"}`);
        closeOverlay();
        loadData();
      } catch (err) {
        toast(err.message);
        back.disabled = false;
      }
    };
    wrap.append(bar, gallery);
    openOverlay(`${h.name} — set aside`, wrap);
  } catch (err) {
    toast(err.message);
  }
}

// Photos sync filed on a hand-placed pin without being sure: close enough to
// be the same place with a poor GPS fix, far enough to be the pub next door.
// One question per photo, on the manifest's paper; the answer is stamped on
// the print, then kept for good, so it's never asked again.
async function openAsk(h) {
  try {
    const groups = await api("GET", `/api/holidays/${h.id}/ask`);
    const box = el("div", "manifest ask");
    const tally = el("p", "mf-tally");
    const list = el("div", "ask-list");
    let left = groups.reduce((n, g) => n + g.photos.length, 0);
    const count = () => {
      tally.textContent = left ? `${left} ${left === 1 ? "photo" : "photos"} to file` : "All filed";
      tally.classList.toggle("done", !left);
    };
    count();
    box.append(tally,
      el("p", "ask-hint", `${left === 1 ? "This was" : "These were"} taken a short way from a pin you placed. Was it there, or somewhere of its own?`),
      list);
    const still = !window.matchMedia("(prefers-reduced-motion: no-preference)").matches;
    for (const g of groups) {
      const name = g.title || "your pin";
      g.photos.forEach((ph, i) => {
        const row = el("div", "ask-row");
        const print = el("button", "ask-print");
        print.setAttribute("aria-label", "View photo");
        const img = el("img");
        img.loading = "lazy";
        img.src = photoURL(ph.asset_id, "thumb");
        img.alt = "";
        print.appendChild(img);
        print.onclick = () => openLightbox(g.photos, i);
        const body = el("div", "ask-body");
        const line = el("p", "ask-line");
        line.append(el("span", "ask-pin", name), el("span", "mf-lead"), el("span", "ask-km", `${ph.km} km`));
        body.append(line, el("p", "ask-when",
          new Date(ph.taken_at).toLocaleString([], { weekday: "short", day: "numeric", month: "short", hour: "2-digit", minute: "2-digit" })));
        const keep = el("button", "primary", `Keep on ${name}`);
        const own = el("button", "ask-own", "Make its own stop");
        const actions = el("div", "ask-actions");
        actions.append(keep, own);
        body.appendChild(actions);
        row.append(print, body);
        list.appendChild(row);
        const answer = async (same) => {
          keep.disabled = own.disabled = true;
          try {
            await api("POST", `/api/holidays/${h.id}/ask`, { asset_ids: [ph.asset_id], same });
          } catch (err) {
            keep.disabled = own.disabled = false;
            toast(err.message);
            return;
          }
          print.appendChild(el("span", "mf-stamp thunk", same ? "Kept" : "New stop"));
          row.classList.add("answered");
          left--;
          count();
          loadData();
          setTimeout(() => {
            row.remove();
            if (!list.childElementCount) closeOverlay();
          }, still ? 400 : 900);
        };
        keep.onclick = () => answer(true);
        own.onclick = () => answer(false);
      });
    }
    openOverlay(`${h.name} — photos to file`, box);
  } catch (err) {
    toast(err.message);
  }
}

function openJournal(h) {
  const box = el("div", "journal-read");
  box.appendChild(el("p", "pop-sub", tripRange(h)));
  box.appendChild(el("p", "journal-text", h.journal));
  openOverlay(h.name, box);
}

/* ---------- manifest: the packing slip ---------- */

// Starter master lists, offered once when there are none yet.
const STARTER_LISTS = {
  "Caravan": ["Gas bottle", "Levelling ramps", "Electric hook-up cable", "Fresh water hose",
    "Waste water container", "Toilet chemicals", "Awning, pegs & mallet", "Folding chairs",
    "Torch", "Kettle & pans", "Tea towels", "Bottle opener", "Bin bags", "First aid kit", "Phone chargers"],
  "Beach": ["Swimwear", "Beach towels", "Sun cream", "After-sun", "Sunglasses", "Hats",
    "Flip flops", "Beach bag", "Books", "Power bank", "Travel adapters", "Medications", "Passports"],
  "City break": ["Passports", "Boarding passes", "Comfortable shoes", "Day bag", "Umbrella",
    "Travel adapters", "Power bank", "Medications", "Camera"],
};

// A trip's manifest: tick items off; the stamp box takes a PACKED stamp.
async function openManifest(h) {
  try {
    const [items, templates] = await Promise.all([
      api("GET", `/api/holidays/${h.id}/packing`),
      api("GET", "/api/packing/templates"),
    ]);
    const box = el("div", "manifest");
    const tally = el("p", "mf-tally");
    const list = el("div", "mf-list");

    const syncCounts = () => {
      h.pack_total = items.length;
      h.pack_done = items.filter((i) => i.checked).length;
      renderSheet(); // keep the trip row's "N to pack" honest behind the overlay
      if (!items.length) tally.textContent = "Nothing on the manifest";
      else if (h.pack_done === items.length) { tally.textContent = "All packed"; tally.classList.add("done"); }
      else { tally.textContent = `${h.pack_done} of ${h.pack_total} packed`; tally.classList.remove("done"); }
    };

    const promote = async (it, tpl) => {
      try {
        const r = await api("POST", `/api/packing/${it.id}/promote`, { template_id: tpl.id });
        toast(r.added ? `Added to the ${tpl.name} list` : `Already on the ${tpl.name} list`);
      } catch (err) {
        toast(err.message);
      }
    };

    const mfRow = (it) => {
      const row = el("div", "mf-row" + (it.checked ? " packed" : ""));
      const boxBtn = el("button", "mf-box");
      boxBtn.type = "button";
      boxBtn.setAttribute("aria-pressed", String(!!it.checked));
      boxBtn.setAttribute("aria-label", (it.checked ? "Unpack " : "Pack ") + it.label);
      // .thunk animates only a freshly stamped item, not every stamp on re-render
      boxBtn.appendChild(it.checked ? el("span", "mf-stamp" + (it.thunk ? " thunk" : ""), "Packed") : el("span", "mf-void"));
      delete it.thunk;
      const toggle = async () => {
        try {
          await api("PATCH", `/api/packing/${it.id}`, { checked: !it.checked });
          it.checked = !it.checked;
          if (it.checked) it.thunk = true;
          renderRows();
        } catch (err) {
          toast(err.message);
        }
      };
      boxBtn.onclick = (e) => { e.stopPropagation(); toggle(); };
      row.onclick = toggle;
      const star = el("button", "mf-icon", "☆");
      star.type = "button";
      star.title = "Always pack this — add it to a packing list";
      star.onclick = (e) => {
        e.stopPropagation();
        if (!templates.length) { toast("No packing lists yet — Packing lists in the trips menu creates them"); return; }
        if (templates.length === 1) { promote(it, templates[0]); return; }
        if (row.querySelector("select")) return;
        const pick = el("select");
        const ph = el("option", null, "Add to…");
        ph.value = "";
        pick.appendChild(ph);
        for (const t of templates) {
          const o = el("option", null, t.name);
          o.value = t.id;
          pick.appendChild(o);
        }
        pick.onclick = (ev) => ev.stopPropagation();
        pick.onchange = () => {
          const tpl = templates.find((t) => t.id === +pick.value);
          if (tpl) promote(it, tpl);
          pick.remove();
        };
        star.before(pick);
      };
      const del = el("button", "mf-icon", "✕");
      del.type = "button";
      del.title = "Remove from the manifest";
      del.onclick = async (e) => {
        e.stopPropagation();
        try {
          await api("DELETE", `/api/packing/${it.id}`);
          items.splice(items.indexOf(it), 1);
          renderRows();
        } catch (err) {
          toast(err.message);
        }
      };
      row.append(el("span", "mf-label", it.label), el("span", "mf-lead"), boxBtn, star, del);
      return row;
    };

    const renderRows = () => {
      list.textContent = "";
      if (!items.length) {
        list.appendChild(el("p", "empty-note", "An empty manifest — add items below, or bring in a packing list."));
      }
      for (const it of items) list.appendChild(mfRow(it));
      syncCounts();
    };

    const addForm = el("form", "form-row");
    const inp = el("input");
    inp.type = "text"; inp.placeholder = "Add something to pack…"; inp.maxLength = 80; inp.required = true;
    const addBtn = el("button", "primary", "Add");
    addBtn.type = "submit";
    addForm.append(inp, addBtn);
    addForm.onsubmit = async (e) => {
      e.preventDefault();
      try {
        const it = await api("POST", `/api/holidays/${h.id}/packing`, { label: inp.value });
        items.push(it);
        inp.value = "";
        renderRows();
        inp.focus();
      } catch (err) {
        toast(err.message);
      }
    };

    box.append(tally, list, addForm);

    if (templates.length) {
      const applyRow = el("div", "form-row");
      const pick = el("select");
      const ph = el("option", null, "Bring in a packing list…");
      ph.value = "";
      pick.appendChild(ph);
      for (const t of templates) {
        const o = el("option", null, `${t.name} · ${t.items.length}`);
        o.value = t.id;
        pick.appendChild(o);
      }
      const applyBtn = el("button", null, "Add list");
      applyBtn.type = "button";
      applyBtn.onclick = async () => {
        if (!pick.value) return;
        try {
          const r = await api("POST", `/api/holidays/${h.id}/packing/apply`, { template_id: +pick.value });
          toast(r.added ? `${r.added} items added` : "Everything on that list is already here");
          const fresh = await api("GET", `/api/holidays/${h.id}/packing`);
          items.length = 0;
          items.push(...fresh);
          pick.value = "";
          renderRows();
        } catch (err) {
          toast(err.message);
        }
      };
      applyRow.append(pick, applyBtn);
      box.appendChild(applyRow);
    }

    const editLists = el("button", "linkish", "Edit packing lists");
    editLists.onclick = () => openMasterLists();
    box.appendChild(editLists);

    renderRows();
    openOverlay(`Manifest — ${h.name}`, box);
  } catch (err) {
    toast(err.message);
  }
}

// The master lists: the "always pack this" reference, kept between trips.
async function openMasterLists() {
  try {
    const templates = await api("GET", "/api/packing/templates");
    const box = el("div", "manifest");
    box.appendChild(el("p", "form-hint",
      "Packing lists hold the things you always pack. Bring one onto a trip from its manifest, and tick items off there."));

    if (!templates.length) {
      // A blank slip: ruled leader lines show the shape a list takes, so the
      // empty state is an invitation rather than a void.
      const blank = el("div", "mf-blank");
      blank.appendChild(el("p", "mf-blank-head", "Nothing on your lists yet"));
      const ruled = el("div", "mf-list");
      for (let i = 0; i < 4; i++) {
        const row = el("div", "mf-row");
        const cell = el("span", "mf-void-cell");
        cell.appendChild(el("span", "mf-void"));
        row.append(el("span", "mf-label", "\u00a0"), el("span", "mf-lead"), cell);
        ruled.appendChild(row);
      }
      blank.appendChild(ruled);
      box.appendChild(blank);
      const starter = el("button", "primary", "Start with caravan, beach and city break lists");
      starter.onclick = async () => {
        try {
          for (const [name, items] of Object.entries(STARTER_LISTS)) {
            await api("POST", "/api/packing/templates", { name, items });
          }
          toast("Starter lists created — make them yours");
          openMasterLists();
        } catch (err) {
          toast(err.message);
        }
      };
      box.appendChild(starter);
    }

    for (const t of templates) {
      const head = el("div", "mf-head");
      head.appendChild(el("h3", "day-head", `${t.name} · ${t.items.length}`));
      const delList = el("button", "linkish", "delete list");
      delList.onclick = async () => {
        if (!confirm(`Delete the "${t.name}" packing list? Trips keep their own copies.`)) return;
        await busy(delList, async () => {
          await api("DELETE", `/api/packing/templates/${t.id}`);
          openMasterLists();
        });
      };
      head.appendChild(delList);
      box.appendChild(head);

      const list = el("div", "mf-list");
      for (const it of t.items) {
        const row = el("div", "mf-row");
        const del = el("button", "mf-icon", "✕");
        del.type = "button";
        del.title = "Remove from this list";
        del.onclick = async () => {
          try {
            await api("DELETE", `/api/packing/templates/${t.id}/items/${it.id}`);
            row.remove();
          } catch (err) {
            toast(err.message);
          }
        };
        row.append(el("span", "mf-label", it.label), el("span", "mf-lead"), del);
        list.appendChild(row);
      }
      box.appendChild(list);

      const addForm = el("form", "form-row");
      const inp = el("input");
      inp.type = "text"; inp.placeholder = `Add to ${t.name}…`; inp.maxLength = 80; inp.required = true;
      const addBtn = el("button", "primary", "Add");
      addBtn.type = "submit";
      addForm.append(inp, addBtn);
      addForm.onsubmit = async (e) => {
        e.preventDefault();
        try {
          await api("POST", `/api/packing/templates/${t.id}/items`, { label: inp.value });
          openMasterLists();
        } catch (err) {
          toast(err.message);
        }
      };
      box.appendChild(addForm);
    }

    if (templates.length) {
      const newForm = el("form", "form-row");
      const inp = el("input");
      inp.type = "text"; inp.placeholder = "New packing list — e.g. Ski"; inp.maxLength = 80; inp.required = true;
      const btn = el("button", "primary", "Create");
      btn.type = "submit";
      newForm.append(inp, btn);
      newForm.onsubmit = async (e) => {
        e.preventDefault();
        try {
          await api("POST", "/api/packing/templates", { name: inp.value });
          openMasterLists();
        } catch (err) {
          toast(err.message);
        }
      };
      box.appendChild(newForm);
    }

    openOverlay("Packing lists", box);
  } catch (err) {
    toast(err.message);
  }
}

/* ---------- banner ---------- */

function renderBanner() {
  const banner = $("banner");
  const active = state.holidays.find((h) => h.active);
  // Nothing live: the tag counts down to the next planned trip instead, and
  // its button opens that trip's packing list.
  const next = active ? null : state.holidays.filter((h) => h.planned)
    .sort((a, b) => a.start_at.localeCompare(b.start_at))[0];
  banner.classList.toggle("planned", !!next);
  $("btn-sync").hidden = $("btn-end").hidden = !active;
  $("btn-pack").hidden = !next;
  if (next) {
    banner.hidden = false;
    $("banner-stub").hidden = false;
    banner.style.setProperty("--c", next.color);
    $("banner-name").textContent = next.name;
    const n = daysUntil(next);
    // Staatliches draws 1 as a bare bar, so the one count it could be misread
    // at is said in words.
    $("stub-n").textContent = n > 1 ? String(n) : n === 1 ? "Tomorrow" : "Today";
    $("stub-unit").textContent = n > 1 ? "days to go" : "";
    $("stub-n").classList.toggle("word", n <= 1);
    $("banner-stub").setAttribute("aria-label", countdown(next));
    const left = (next.pack_total || 0) - (next.pack_done || 0);
    $("btn-pack").textContent = !next.pack_total ? "Manifest" : left ? `Manifest · ${left}` : "Manifest ✓";
    $("btn-pack").title = left ? `${left} still to pack` : "Open this trip's manifest";
    $("btn-pack").onclick = () => openManifest(next);
    return;
  }
  if (!active) { banner.hidden = true; return; }
  banner.hidden = false;
  banner.style.setProperty("--c", active.color);
  $("banner-name").textContent = active.name;
  // Live, the same stub counts up instead: which day of the trip this is.
  // Never below 1 — a start later today is still the first day.
  // Same day count as the story's (a day turns over at 4 am, see tripDay).
  const day = tripDay(active, Date.now()).n;
  $("banner-stub").hidden = false;
  $("stub-n").textContent = day > 1 ? String(day) : "day";
  $("stub-unit").textContent = day > 1 ? "day" : "first";
  $("stub-n").classList.remove("word");
  $("banner-stub").setAttribute("aria-label", `Day ${day} of ${active.name}`);
  $("btn-end").onclick = async () => {
    if (!confirm(`End "${active.name}"? Photos taken from now on won't join it.`)) return;
    // ending pulls the trip's last photos from Immich first, which can take a while
    toast("Ending the holiday and pulling its last photos…");
    await busy($("btn-end"), async () => {
      const r = await api("POST", `/api/holidays/${active.id}/end`);
      toast(r.sync_error ? "Holiday ended (photo sync failed — use Sync later)" : "Holiday ended — happy memories!");
      loadData();
    }, "Ending…");
  };
  $("btn-sync").onclick = async () => {
    toast("Syncing photos…");
    await busy($("btn-sync"), async () => {
      const r = await api("POST", `/api/holidays/${active.id}/sync`);
      const extra = r.unplaced ? ` (+${r.unplaced} without location)` : "";
      toast(`Synced: ${plural(r.photos, "photo")} across ${plural(r.photo_pins, "place")}${extra}`);
      loadData();
    });
  };
}

$("btn-cancel-place").onclick = () => setPlacing(false);

/* ---------- new trip form ---------- */

let chosenColor = SWATCHES[0];

function makeSwatches(wrap, initial, onPick) {
  wrap.textContent = "";
  for (const c of SWATCHES) {
    const b = el("button");
    b.type = "button";
    b.style.background = c;
    const name = SWATCH_NAMES[SWATCHES.indexOf(c)] || c;
    b.title = name;
    b.setAttribute("aria-label", `${name} trip colour`);
    b.setAttribute("aria-pressed", String(c === initial));
    if (c === initial) b.classList.add("sel");
    b.onclick = () => {
      onPick(c);
      wrap.querySelectorAll("button").forEach((x) => { x.classList.remove("sel"); x.setAttribute("aria-pressed", "false"); });
      b.classList.add("sel");
      b.setAttribute("aria-pressed", "true");
    };
    wrap.appendChild(b);
  }
}

function buildSwatches() {
  const used = new Set(state.holidays.map((h) => h.color));
  // Prefer a colour no trip has used yet.
  chosenColor = SWATCHES.find((c) => !used.has(c)) || SWATCHES[0];
  makeSwatches($("swatches"), chosenColor, (c) => { chosenColor = c; });
}

// Destination picker: search places (Nominatim, proxied by the server so the
// browser never talks to a third party) or type "lat, lng" straight in.
let chosenDest = null;
// The name the last place pick filled in: a later pick may replace it, but
// never a name the person typed themselves.
let autoName = "";

function resetDest() {
  chosenDest = null;
  autoName = "";
  $("trip-dest").value = "";
  $("dest-results").textContent = "";
}

// placeSearch runs one lookup into a results box and calls onPick with the
// chosen place: { name, lat, lng, osm? }. Typed "lat, lng" is taken as-is.
// Shared by the new-trip form and trip editing.
async function placeSearch(q, res, onPick) {
  q = q.trim();
  if (!q) return;
  const m = q.match(COORD_RE);
  if (m) {
    const lat = +m[1], lng = +m[2];
    if (lat < -90 || lat > 90 || lng < -180 || lng > 180) { toast("Coordinates out of range"); return; }
    const coords = `${lat.toFixed(5)}, ${lng.toFixed(5)}`;
    const d = { name: coords, lat, lng };
    res.textContent = "";
    const hint = res.appendChild(el("p", "form-hint", "Looking up the place…"));
    // Name the spot after the town it's in; the numbers stay if nothing
    // answers (offline, open sea). No outline: the spot is the destination.
    try {
      const r = await api("GET", `/api/geocode/reverse?lat=${lat}&lng=${lng}`);
      if (r.name) d.name = r.name;
    } catch { /* keep the coordinates */ }
    hint.textContent = d.name === coords ? `Destination set: ${coords}` : `Destination set: ${d.name} (${coords})`;
    onPick(d, d.name === coords ? undefined : d.name.split(",")[0].trim());
    return;
  }
  res.textContent = "";
  res.appendChild(el("p", "form-hint", "Searching…"));
  try {
    const hits = await api("GET", `/api/geocode?q=${encodeURIComponent(q)}`);
    res.textContent = "";
    onPick(null);
    if (!hits.length) {
      res.appendChild(el("p", "form-hint", "No places found — try a broader name, or type lat, lng"));
      return;
    }
    for (const hit of hits) {
      const b = el("button", "dest-opt", hit.name);
      b.type = "button";
      b.onclick = () => {
        res.querySelectorAll(".dest-opt").forEach((x) => x.classList.toggle("sel", x === b));
        onPick({ name: hit.name.split(",").slice(0, 2).join(","), lat: hit.lat, lng: hit.lng, osm: hit.osm },
          hit.name.split(",")[0].trim());
      };
      res.appendChild(b);
    }
  } catch (err) {
    res.textContent = "";
    toast(err.message);
  }
}

// the request fields for a picked destination (or for removing it: null)
function destBody(d) {
  if (!d) return { dest_name: "" };
  const b = { dest_name: d.name, dest_lat: d.lat, dest_lng: d.lng };
  if (d.osm) b.dest_osm = d.osm; // the server fetches the place's outline
  return b;
}

function searchDest() {
  return placeSearch($("trip-dest").value, $("dest-results"), (d, short) => {
    chosenDest = d;
    if (!d || !short) return;
    const name = $("trip-name");
    if (!name.value.trim() || name.value === autoName) {
      autoName = short;
      name.value = autoName;
    }
  });
}
$("btn-dest-search").onclick = searchDest;
$("trip-dest").addEventListener("keydown", (e) => {
  if (e.key === "Enter") { e.preventDefault(); searchDest(); }
});

// A first day after today plans the trip; a planned trip has no last day
// yet, so the field goes away and the button says what will happen.
function syncTripForm() {
  const d = new Date();
  const today = `${d.getFullYear()}-${String(d.getMonth() + 1).padStart(2, "0")}-${String(d.getDate()).padStart(2, "0")}`;
  const planned = $("trip-start").value > today;
  $("trip-end-row").hidden = planned;
  if (planned) $("trip-end").value = "";
  $("trip-submit").textContent = planned ? "Plan" : $("trip-end").value ? "Add" : "Start";
}
$("trip-start").oninput = syncTripForm;
$("trip-end").oninput = syncTripForm;

$("btn-new-trip").onclick = () => {
  buildSwatches();
  $("trip-start").value = new Date().toISOString().slice(0, 10);
  syncTripForm();
  resetDest();
  $("new-trip-form").hidden = false;
  $("btn-new-trip").hidden = true;
  $("trip-dest").focus();
};
$("btn-cancel-trip").onclick = () => {
  $("new-trip-form").hidden = true;
  $("btn-new-trip").hidden = false;
};
$("trip-search").oninput = (e) => { state.query = e.target.value; renderSheet(); };

$("new-trip-form").onsubmit = async (e) => {
  e.preventDefault();
  await busy(submitter(e), async () => {
    const body = {
      name: $("trip-name").value.trim(),
      color: chosenColor,
      start_at: $("trip-start").value,
      end_at: $("trip-end").value,
    };
    if (chosenDest) Object.assign(body, destBody(chosenDest));
    const created = await api("POST", "/api/holidays", body);
    $("new-trip-form").hidden = true;
    $("btn-new-trip").hidden = false;
    $("trip-name").value = "";
    $("trip-end").value = "";
    const dest = chosenDest;
    resetDest();
    toast(created.planned ? `Planned ${created.name}: ${countdown(created)}`
      : created.active ? "Holiday started — pins and photos now attach to it" : "Past trip added — pulling its photos…");
    await loadData();
    if (dest) map.flyTo([dest.lat, dest.lng], 10);
    if (created.planned) return; // nothing to pull until it starts
    api("POST", `/api/holidays/${created.id}/sync`)
      .then((r) => { toast(r.photos ? `Found ${plural(r.photos, "photo")} across ${plural(r.photo_pins, "place")}` : "No photos found for those dates yet"); loadData(); })
      .catch(() => {});
  });
};

/* ---------- sheet + admin + logout ---------- */

$("sheet-pill").onclick = () => setSheet($("sheet").classList.contains("collapsed"));

$("btn-manifest").onclick = () => openMasterLists();

/* ---------- your account ---------- */

// The account button carries the holder's initial; it appears once the
// server has said who is signed in (never on a snapshot or a share link).
function renderAccount() {
  const b = $("btn-account");
  b.hidden = !state.me;
  if (state.me) b.textContent = [...state.me.username][0].toUpperCase();
}

// The holder's page: everything about the person rather than the trips,
// kept out of the trips sheet so it doesn't sink below a growing list.
function openAccount() {
  const me = state.me;
  if (!me) return;
  const box = el("div", "manifest register account");

  const holder = el("div", "fm-row");
  const head = el("div", "fm-head");
  head.appendChild(el("span", "fm-name", me.username));
  if (me.is_admin) head.appendChild(el("span", "fm-role", "Admin"));
  holder.appendChild(head);
  box.appendChild(holder);

  box.appendChild(el("h3", "day-head", "Password"));
  const form = el("form", "fm-add");
  const cur = el("input");
  cur.type = "password";
  cur.placeholder = "Current password";
  cur.autocomplete = "current-password";
  cur.required = true;
  const next = el("input");
  next.type = "password";
  next.placeholder = "New password (8+ characters)";
  next.autocomplete = "new-password";
  next.minLength = 8;
  next.required = true;
  const save = el("button", "primary", "Change password");
  save.type = "submit";
  form.append(cur, next, save, el("p", "form-hint", "Changing it signs you out everywhere else."));
  form.onsubmit = async (e) => {
    e.preventDefault();
    try {
      await api("POST", "/api/password", { current_password: cur.value, new_password: next.value });
      cur.value = ""; next.value = "";
      toast("Password changed — other devices were signed out");
    } catch (err) {
      toast(err.message);
    }
  };
  box.appendChild(form);

  if (me.is_admin) {
    box.appendChild(el("h3", "day-head", "Family"));
    box.appendChild(el("p", "form-hint", "Add people, reset a forgotten password, see who has signed in."));
    const fam = el("button", "linkish", "Family accounts");
    fam.type = "button";
    fam.onclick = () => openFamily();
    box.appendChild(fam);
  }

  box.appendChild(showOnTV());

  box.appendChild(el("h3", "day-head", "Your data"));
  box.appendChild(el("p", "form-hint", "Every trip, pin, journal and packing list, as one JSON file."));
  const exp = el("a", "linkish", "Export my data");
  exp.href = "/api/export";
  exp.download = "";
  box.appendChild(exp);

  const foot = el("div", "acct-foot");
  const out = el("button", "linkish", "Sign out");
  out.type = "button";
  out.onclick = signOut;
  foot.append(out, el("p", "form-hint", me.version === "dev" ? "dev build" : me.version));
  box.appendChild(foot);

  openOverlay("Your account", box);
}

// Task 133: pair a TV running the Wanderstamp TV app. The TV shows a 4-letter
// code; typed here, it lets that TV show every trip (previews only) until
// it's unpaired below.
function showOnTV() {
  const sec = el("section", "tv-pair");
  sec.id = "tv-pair";
  sec.appendChild(el("h3", "day-head", "Show on TV"));
  sec.appendChild(el("p", "form-hint",
    "Open the Wanderstamp app on the TV and type the code it shows. That TV can then show every trip, previews only, until you unpair it."));
  const form = el("form", "fm-add");
  const code = el("input");
  code.id = "tv-code";
  code.placeholder = "Code on the TV";
  code.autocomplete = "off";
  code.autocapitalize = "characters";
  code.spellcheck = false;
  code.maxLength = 8;
  code.required = true;
  code.setAttribute("aria-label", "Code on the TV");
  const name = el("input");
  name.id = "tv-name";
  name.placeholder = "Name, e.g. Living room";
  name.value = "Living room";
  name.maxLength = 40;
  name.required = true;
  name.setAttribute("aria-label", "Name for this TV");
  const go = el("button", "primary", "Pair TV");
  go.type = "submit";
  form.append(code, name, go);
  const list = el("ul", "tv-list");
  list.id = "tv-list";
  form.onsubmit = async (e) => {
    e.preventDefault();
    const c = code.value.replace(/\s+/g, "").toUpperCase();
    const n = name.value.trim();
    if (!c) return;
    if (!n) { toast("Give the TV a name, e.g. Living room"); name.focus(); return; }
    if (go.disabled) return;
    go.disabled = true; // (not busy(): it toasts its own message and hides the 404)
    try {
      await api("POST", "/api/tv/pair", { code: c, name: n });
      code.value = "";
      toast(`Paired: ${n}`);
      loadTVs(list);
    } catch (err) {
      toast(err.status === 404 ? "That code isn't on any TV right now — check the TV and try again"
        : err.status === 429 ? "Too many wrong codes — try again in 15 minutes"
        : err.message);
    } finally {
      go.disabled = false;
    }
  };
  sec.append(form, list);
  loadTVs(list);
  return sec;
}

async function loadTVs(list) {
  let tvs;
  try {
    tvs = await api("GET", "/api/tv/devices");
  } catch {
    return; // an older server without TV pairing: the form says enough
  }
  list.textContent = "";
  for (const d of Array.isArray(tvs) ? tvs : []) {
    const li = el("li", "tv-item");
    li.appendChild(el("span", "tv-name", d.name));
    const seen = d.last_seen ? `last on ${fmtDate(d.last_seen)}` : "not used yet";
    li.appendChild(el("span", "form-hint", ` · paired by ${d.created_by || "someone"}, ${seen}`));
    const un = el("button", "linkish", "Unpair");
    un.type = "button";
    un.onclick = async () => {
      if (!confirm(`Unpair ${d.name}? It stops showing your trips straight away.`)) return;
      un.disabled = true;
      try {
        await api("DELETE", `/api/tv/devices/${d.id}`);
        toast(`${d.name} unpaired`);
      } catch (err) {
        toast(err.status === 404 ? `${d.name} was already unpaired` : err.message);
      }
      loadTVs(list);
    };
    li.appendChild(un);
    list.appendChild(li);
  }
}

async function signOut() {
  // WA-3: send this person's waiting pins while still signed in as them; any
  // that can't go now stay queued under their name, never sent as the next user
  await syncQueue().catch(() => {});
  await api("POST", "/api/logout", {});
  // the map this device saw goes with the session
  try { localStorage.removeItem(SNAP_KEY); } catch {}
  location.reload();
}

$("btn-account").onclick = openAccount;

/* ---------- lightbox ---------- */

const lb = { photos: [], idx: 0, dir: 1, seq: 0, aside: false };

// Previews are ~400 KB each, and a phone away from home pulls them through
// the house's upload — a few seconds apiece. So the viewer never waits blank:
// the small thumb (usually already on screen in the strip) shows at once,
// softened, and the full image swaps in when it lands. The next one is then
// fetched behind the reader's back, so a steady "next" is usually instant.
// Photo responses are cached immutable, so a preview that has landed once
// comes straight from the browser cache — only its URL is remembered here.
const previews = new Set(); // urls fully loaded
const previewLoads = new Map(); // url → { p, im }, while in flight

function loadPreview(url) {
  if (previews.has(url)) return Promise.resolve();
  if (!previewLoads.has(url)) {
    const im = new Image(); // kept in the map so nothing collects it mid-load
    const p = new Promise((res, rej) => { im.onload = res; im.onerror = rej; })
      .then(() => { previews.add(url); })
      .finally(() => previewLoads.delete(url));
    im.src = url;
    previewLoads.set(url, { p, im });
  }
  return previewLoads.get(url).p;
}

// aside: offer "Not for the map" — only on a trip's own photos, signed in
function openLightbox(photos, idx, { aside = false } = {}) {
  lb.photos = photos.slice(); // setting one aside takes it out of this list
  lb.idx = idx;
  lb.dir = 1;
  lb.aside = aside;
  $("lb-aside-wrap").hidden = !aside;
  modalOpen("lightbox", $("lb-close"));
  showLightbox();
}

function stopVideo() {
  const v = $("lb-video");
  if (!v.hidden || v.getAttribute("src")) { v.pause(); v.removeAttribute("src"); v.load(); }
  v.hidden = true;
}

function showLightbox() {
  const ph = lb.photos[lb.idx];
  const img = $("lb-img");
  stopVideo();
  img.hidden = false;
  $("lb-count").textContent = `${lb.idx + 1} / ${lb.photos.length}`;
  $("lb-date").textContent = fmtDate(ph.taken_at);
  $("lb-prev").style.visibility = lb.idx > 0 ? "visible" : "hidden";
  $("lb-next").style.visibility = lb.idx < lb.photos.length - 1 ? "visible" : "hidden";
  // a share never streams a video: Immich's playback can be the original
  // file, GPS and all (S2), so a share shows its poster and says so
  $("lb-note").hidden = !(SHARE && ph.type === "video");
  if (ph.type === "video" && !SHARE) {
    // F7: the server streams Immich's playback (Range), the poster is its preview
    ++lb.seq;
    img.hidden = true;
    img.removeAttribute("src");
    const v = $("lb-video");
    v.poster = photoURL(ph.asset_id, "preview");
    v.src = photoURL(ph.asset_id, "video");
    v.hidden = false;
    $("lb-orig").href = photoURL(ph.asset_id, "original");
    $("lb-orig").textContent = "Open original";
    return;
  }
  const url = photoURL(ph.asset_id, "preview");
  const seq = ++lb.seq; // a late preview for a photo already left is dropped
  // the blur lifts only once the full image is decoded, so the thumb never
  // flashes sharp and pixelated on its way out
  const sharpen = () => {
    img.src = url;
    img.decode().catch(() => {}).then(() => { if (seq === lb.seq) img.classList.remove("soft"); });
    preloadAhead(seq);
  };
  if (previews.has(url)) {
    sharpen();
  } else {
    img.src = photoURL(ph.asset_id, "thumb");
    img.classList.add("soft");
    loadPreview(url).then(() => { if (seq === lb.seq) sharpen(); },
      () => { if (seq === lb.seq) img.classList.remove("soft"); });
  }
  // a share link gets the preview, never the original file with its GPS data
  $("lb-orig").href = photoURL(ph.asset_id, SHARE ? "preview" : "original");
  $("lb-orig").textContent = SHARE ? "Open photo" : "Open original";
}

// Only once the photo on screen has landed, and one at a time: on a thin
// uplink a parallel fetch would slow the very photo being looked at.
function preloadAhead(seq) {
  const at = (i) => lb.photos[i] && lb.photos[i].type !== "video" && photoURL(lb.photos[i].asset_id, "preview");
  const ahead = at(lb.idx + lb.dir), behind = at(lb.idx - lb.dir);
  (ahead ? loadPreview(ahead) : Promise.resolve())
    .catch(() => {})
    .then(() => { if (seq === lb.seq && behind) loadPreview(behind).catch(() => {}); });
}

function lbStep(d) {
  const next = lb.idx + d;
  if (next >= 0 && next < lb.photos.length) { lb.idx = next; lb.dir = d; showLightbox(); }
}

$("lb-close").onclick = () => { lb.seq++; stopVideo(); modalClose("lightbox"); $("lb-img").src = ""; };
$("lb-prev").onclick = () => lbStep(-1);
// Takes the photo off the trip here, never out of Immich; it waits in the
// trip's "Set aside" list to be put back.
$("lb-aside").onclick = async () => {
  const ph = lb.photos[lb.idx];
  const btn = $("lb-aside");
  btn.disabled = true;
  try {
    await api("POST", `/api/photos/${ph.asset_id}/aside`);
    lb.photos.splice(lb.idx, 1);
    toast("Set aside — it's still in Immich. Put it back from the trip's story.");
    if (!lb.photos.length) $("lb-close").click();
    else { lb.idx = Math.min(lb.idx, lb.photos.length - 1); showLightbox(); }
    loadData();
  } catch (err) {
    toast(err.message);
  } finally {
    btn.disabled = false;
  }
};
$("lb-next").onclick = () => lbStep(1);
document.addEventListener("keydown", (e) => {
  if (!$("lightbox").hidden) {
    if (e.key === "Escape") $("lb-close").click();
    if (e.key === "ArrowLeft") lbStep(-1);
    if (e.key === "ArrowRight") lbStep(1);
  } else if (!$("overlay").hidden && e.key === "Escape") {
    $("overlay-close").click();
  } else if (story.hid !== null && e.key === "Escape") {
    closeStory();
  }
});
let touchX = null;
$("lightbox").addEventListener("touchstart", (e) => { touchX = e.touches[0].clientX; }, { passive: true });
$("lightbox").addEventListener("touchend", (e) => {
  if (touchX === null) return;
  const dx = e.changedTouches[0].clientX - touchX;
  if (Math.abs(dx) > 50) lbStep(dx < 0 ? 1 : -1);
  touchX = null;
}, { passive: true });

/* ---------- sign-in ---------- */

function showLogin() {
  $("login").hidden = false;
}

$("login-form").onsubmit = async (e) => {
  e.preventDefault();
  const err = $("login-error");
  err.hidden = true;
  try {
    const me = await api("POST", "/api/login", { username: $("login-user").value.trim(), password: $("login-pass").value });
    if (me.must_change_password) return showChoosePassword($("login-pass").value);
    location.reload();
  } catch (ex) {
    err.textContent = ex.message === "invalid credentials" ? "Wrong username or password." : ex.message;
    err.hidden = false;
  }
};

// The cover's second face: a temporary password (one an admin set) is good
// for exactly this. Straight after signing in the page still holds it, so
// only the two new fields show; after a reload it has to be typed again.
function showChoosePassword(temp) {
  $("login").hidden = false;
  $("login-form").hidden = true;
  $("choose-form").hidden = false;
  $("choose-temp").hidden = !!temp;
  $("choose-temp").required = !temp;
  $("choose-temp").value = temp || "";
  (temp ? $("choose-new") : $("choose-temp")).focus();
}

$("choose-form").onsubmit = async (e) => {
  e.preventDefault();
  const err = $("choose-error");
  err.hidden = true;
  if ($("choose-new").value !== $("choose-again").value) {
    err.textContent = "The two new passwords don't match.";
    err.hidden = false;
    return;
  }
  try {
    await api("POST", "/api/password", {
      current_password: $("choose-temp").value,
      new_password: $("choose-new").value,
    });
    location.reload();
  } catch (ex) {
    err.textContent = ex.message === "current password is wrong"
      ? "That isn't the password you were given." : ex.message;
    err.hidden = false;
    if ($("choose-temp").hidden && ex.message === "current password is wrong") {
      $("choose-temp").hidden = false; // shouldn't happen; let them type it
      $("choose-temp").required = true;
    }
  }
};

$("choose-signout").onclick = async () => {
  await api("POST", "/api/logout", {}).catch(() => {});
  try { localStorage.removeItem(SNAP_KEY); } catch {}
  location.reload();
};

/* ---------- family accounts (admins) ---------- */

// A temporary password to read out or text: no 0/O or 1/l/I to mistype.
function tempPassword() {
  const abc = "abcdefghjkmnpqrstuvwxyz23456789";
  const pick = crypto.getRandomValues(new Uint32Array(12));
  const s = Array.from(pick, (n) => abc[n % abc.length]).join("");
  return `${s.slice(0, 4)}-${s.slice(4, 8)}-${s.slice(8)}`;
}

function seenText(u) {
  if (!u.last_seen) return "Never signed in";
  const mins = Math.round((Date.now() - new Date(u.last_seen)) / 60000);
  if (mins < 10) return "Here now";
  if (mins < 60) return `Last seen ${mins} min ago`;
  const hrs = Math.round(mins / 60);
  if (hrs < 24) return `Last seen ${hrs} ${hrs === 1 ? "hour" : "hours"} ago`;
  const days = Math.round(hrs / 24);
  if (days < 45) return `Last seen ${days} ${days === 1 ? "day" : "days"} ago`;
  return `Last seen ${fmtDate(u.last_seen)}`;
}

async function openFamily() {
  let users;
  try {
    users = await api("GET", "/api/users");
  } catch (err) {
    toast(err.message);
    return;
  }
  const box = el("div", "manifest register");
  box.appendChild(el("p", "form-hint",
    "Everyone here sees the same trips. Removing an account only removes the sign-in; what they recorded stays."));

  const act = (label, fn, cls = "") => {
    const b = el("button", "linkish" + (cls ? " " + cls : ""), label);
    b.type = "button";
    b.onclick = () => busy(b, fn);
    return b;
  };

  const list = el("div", "fm-list");
  for (const u of users) {
    const row = el("div", "fm-row" + (u.locked_seconds > 0 ? " locked" : ""));
    const head = el("div", "fm-head");
    head.appendChild(el("span", "fm-name", u.username));
    if (u.is_admin) head.appendChild(el("span", "fm-role", "Admin"));
    if (u.locked_seconds > 0) {
      // the one loud mark on the page: it's what you're looking for when
      // someone says they can't get in
      head.appendChild(el("span", "fm-locked", "Locked"));
    }
    row.appendChild(head);

    const facts = [u.you ? "You" : seenText(u)];
    if (u.sessions > 1) facts.push(`signed in on ${u.sessions} devices`);
    if (u.locked_seconds > 0) facts.push(`too many wrong passwords, ${Math.ceil(u.locked_seconds / 60)} min left`);
    if (u.must_change_password) facts.push(u.last_seen ? "still on a temporary password" : "has a temporary password");
    row.appendChild(el("p", "fm-meta", facts.join(" · ")));

    const actions = el("div", "fm-actions");
    if (u.you) {
      if (u.sessions > 1) {
        actions.appendChild(act("Sign out my other devices", async () => {
          const r = await api("POST", `/api/users/${u.id}/signout`);
          toast(`Signed out ${r.signed_out} other ${r.signed_out === 1 ? "device" : "devices"}`);
          openFamily();
        }));
      }
    } else {
      if (u.locked_seconds > 0) {
        actions.appendChild(act("Unlock", async () => {
          await api("POST", `/api/users/${u.id}/unlock`);
          toast(`${u.username} can sign in again`);
          openFamily();
        }));
      }
      const reset = el("form", "fm-reset");
      reset.hidden = true;
      const temp = el("input");
      temp.type = "text";
      temp.autocomplete = "off";
      temp.spellcheck = false;
      temp.minLength = 8;
      temp.required = true;
      temp.setAttribute("aria-label", `Temporary password for ${u.username}`);
      const set = el("button", "primary", "Set temporary password");
      set.type = "submit";
      reset.append(temp, set, el("p", "form-hint",
        `Give ${u.username} this password. It signs them out everywhere, and they choose their own the next time they sign in.`));
      reset.onsubmit = async (e) => {
        e.preventDefault();
        try {
          await api("POST", `/api/users/${u.id}/password`, { password: temp.value });
          toast(`Temporary password set for ${u.username}`);
          openFamily();
        } catch (err) {
          toast(err.message);
        }
      };
      actions.appendChild(act("Reset password", () => {
        reset.hidden = !reset.hidden;
        if (!reset.hidden) { temp.value = tempPassword(); temp.select(); }
      }));
      if (u.sessions > 0) {
        actions.appendChild(act("Sign out everywhere", async () => {
          const r = await api("POST", `/api/users/${u.id}/signout`);
          toast(`${u.username} signed out of ${r.signed_out} ${r.signed_out === 1 ? "device" : "devices"}`);
          openFamily();
        }));
      }
      actions.appendChild(u.is_admin
        ? act("Remove admin", async () => {
          await api("PATCH", `/api/users/${u.id}`, { is_admin: false });
          toast(`${u.username} is no longer an admin`);
          openFamily();
        })
        : act("Make admin", async () => {
          if (!confirm(`Make ${u.username} an admin? They'll be able to manage every account, yours included.`)) return;
          await api("PATCH", `/api/users/${u.id}`, { is_admin: true });
          toast(`${u.username} is now an admin`);
          openFamily();
        }));
      actions.appendChild(act("Remove", async () => {
        if (!confirm(`Remove ${u.username}'s account? They're signed out, and the trips stay.`)) return;
        await api("DELETE", `/api/users/${u.id}`);
        toast(`Removed ${u.username}`);
        openFamily();
      }, "danger"));
      row.append(actions, reset);
      list.appendChild(row);
      continue;
    }
    row.appendChild(actions);
    list.appendChild(row);
  }
  box.appendChild(list);

  box.appendChild(el("h3", "day-head", "Add a family member"));
  const add = el("form", "fm-add");
  const name = el("input");
  name.type = "text";
  name.placeholder = "Username";
  name.autocomplete = "off";
  name.maxLength = 64;
  name.required = true;
  const pass = el("input");
  pass.type = "text";
  pass.autocomplete = "off";
  pass.spellcheck = false;
  pass.minLength = 8;
  pass.required = true;
  pass.value = tempPassword();
  pass.setAttribute("aria-label", "Temporary password");
  const create = el("button", "primary", "Create account");
  create.type = "submit";
  add.append(name, pass, create, el("p", "form-hint",
    "Give them this temporary password. They choose their own the first time they sign in."));
  add.onsubmit = async (e) => {
    e.preventDefault();
    try {
      await api("POST", "/api/users", { username: name.value.trim(), password: pass.value });
      toast(`Account created for ${name.value.trim()}`);
      openFamily();
    } catch (err) {
      toast(err.message);
    }
  };
  box.appendChild(add);
  // F9: linus's import module (admin only, as this page is)
  if (window.WSImport) {
    box.appendChild(el("h3", "day-head", "Restore from a backup"));
    const imp = el("button", "linkish", "Import a backup file");
    imp.type = "button";
    imp.onclick = () => window.WSImport.open();
    box.appendChild(imp);
  }
  openOverlay("Family", box);
}

/* ---------- boot ---------- */

/* ---------- modules ---------- */

// The feature modules (stats, tracks, import) live in their own files and
// reach the app only through this: they never edit app.js, and app.js calls
// them only where they exist, so a missing file just hides its feature.
const dataListeners = [];
window.WS = {
  map, state, api, el, toast,
  onData(fn) { dataListeners.push(fn); },
  // an empty sheet in the app's overlay; returns the element to fill
  openPanel(title) {
    const body = el("div", "ws-panel");
    openOverlay(title, body);
    return body;
  },
};

// Task 136: the TV's QR code opens /#pair=ABCD. Sign-in keeps the hash (it only
// reloads the page), and pairing always waits for the Confirm tap: a QR someone
// else put in front of the camera must not get a TV onto this account.
function readTVPair() {
  const m = /^#pair=([A-HJ-NP-Z]{4})(?:&(app|noapp)=1)?$/.exec(location.hash);
  return m ? { code: m[1], via: m[2] || "" } : null;
}

function clearTVPair() {
  history.replaceState(null, "", location.pathname + location.search);
  const b = $("pair-app");
  if (b) b.remove();
}

// A scan lands in the phone's browser. Android's Chrome won't jump to an app by
// itself, so it's a link; with no app installed it comes back here with noapp=1.
function offerTVPairApp() {
  const p = readTVPair();
  if (!p || p.via || !/Android/i.test(navigator.userAgent) || $("pair-app")) return;
  const back = `${location.origin}${location.pathname}#pair=${p.code}&noapp=1`;
  const a = el("a", "pair-app", "Open in the Wanderstamp app");
  a.id = "pair-app";
  a.href = `intent://pair?code=${p.code}#Intent;scheme=wanderstamp;package=app.wanderstamp;S.browser_fallback_url=${encodeURIComponent(back)};end`;
  document.body.appendChild(a);
}

function offerTVPair() {
  const p = readTVPair();
  if (!p) return;
  clearTVPair();
  const box = el("div", "tv-pair-confirm");
  box.appendChild(el("p", null, "This TV can then show every trip, previews only, until you unpair it from Show on TV."));
  box.appendChild(el("p", "tv-pair-code", p.code));
  const name = el("input");
  name.id = "tv-pair-name";
  name.value = "Living room";
  name.maxLength = 40;
  name.setAttribute("aria-label", "Name for this TV");
  const ok = el("button", "primary", "Confirm");
  ok.id = "tv-pair-ok";
  const no = el("button", "linkish", "Cancel");
  no.onclick = closeOverlay;
  ok.onclick = async () => {
    const n = name.value.trim();
    if (!n) { toast("Give the TV a name, e.g. Living room"); name.focus(); return; }
    if (ok.disabled) return;
    ok.disabled = true;
    try {
      await api("POST", "/api/tv/pair", { code: p.code, name: n });
      closeOverlay();
      toast(`Paired: ${n}`);
    } catch (err) {
      toast(err.status === 404 ? "That code has expired. Scan the TV again"
        : err.status === 429 ? "Too many wrong codes. Try again in 15 minutes"
        : err.message);
    } finally {
      ok.disabled = false;
    }
  };
  box.append(name, ok, no);
  openOverlay("Pair this TV?", box);
}

window.addEventListener("hashchange", () => {
  if (!readTVPair() || !$("login").hidden) return;
  offerTVPairApp();
  offerTVPair();
});

(async function boot() {
  if (SHARE) {
    document.body.classList.add("shared");
    try {
      const d = await api("GET", `/api/share/${SHARE}`);
      state.holidays = [d.holiday];
      state.pins = d.pins;
      renderMarkers();
      openStory(d.holiday);
    } catch (ex) {
      const expired = ex && ex.status === 410;
      openOverlay(expired ? "Link expired" : "Link not found", el("p", "empty-note", expired
        ? "This share link has expired — ask the sender for a fresh one."
        : "This share link is no longer active — ask the sender for a fresh one."));
      $("overlay-close").hidden = true;
    }
    return;
  }
  // Draw the last map this device saw straight away, then bring it up to
  // date: a returning visit shows its trips before the first request is back.
  const hadSnapshot = restoreSnapshot();
  offerTVPairApp();
  try {
    await loadData(true);
    offerTVPair();
  } catch (ex) {
    // A dead network (unlike a 401) means mid-holiday with no signal: the
    // snapshot already on screen is the map.
    if (isNetworkError(ex)) {
      if (hadSnapshot) toast("Offline — showing your last synced map");
    } else if (ex && ex.status === 401) {
      // WA-4: the session ran out. The saved map belongs to whoever signed in
      // last; forget it rather than keep it in storage and behind the cover.
      try { localStorage.removeItem(SNAP_KEY); } catch {}
      state.holidays = [];
      state.pins = [];
      state.snapUser = null;
      renderAll();
    } else if ($("login").hidden) {
      // (on 401 or 428 the cover is already up, asking to sign in or for a
      // new password — that says it better than a toast)
      toast("Couldn't load the map data: " + ex.message);
    }
  }
})();

// App-shell service worker: the map opens (from cache) even with no signal.
// navigator.serviceWorker only exists on HTTPS/localhost, so plain-HTTP LAN
// visits skip this silently — same caveat as the clipboard API.
if ("serviceWorker" in navigator && !SHARE) {
  navigator.serviceWorker.register("/sw.js").catch(() => {});
}
