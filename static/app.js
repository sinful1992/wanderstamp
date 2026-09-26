/* Holiday Map frontend — vanilla JS + Leaflet. All user text goes through
   textContent (never innerHTML) so titles/notes can't inject markup. */

"use strict";

const SWATCHES = ["#1d6fb8", "#c2452d", "#2e8b57", "#b8860b", "#7b4b94", "#d81b60", "#00838f", "#5d4037"];

const state = {
  me: null,
  holidays: [],
  pins: [],
  hidden: new Set(),      // holiday ids toggled off
  markers: new Map(),  // pin key -> Leaflet marker, reconciled in renderMarkers
  routes: [],             // one dotted itinerary line per visible trip
  dests: new Map(),       // holiday id -> destination stamp marker
  focused: null,          // holiday id spotlit by tapping its trip row
  placing: false,
  didFit: false,
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

// The trips sheet is a disclosure; one setter keeps the pill's announced
// state and the class in step.
function setSheet(open) {
  $("sheet").classList.toggle("collapsed", !open);
  $("sheet-pill").setAttribute("aria-expanded", String(open));
}

function toast(msg) {
  const t = $("toast");
  t.textContent = msg;
  t.hidden = false;
  clearTimeout(toast.timer);
  toast.timer = setTimeout(() => { t.hidden = true; }, 3500);
}

async function api(method, path, body) {
  const opts = { method, credentials: "same-origin" };
  if (body !== undefined) {
    opts.headers = { "Content-Type": "application/json" };
    opts.body = JSON.stringify(body);
  }
  const resp = await fetch(path, opts);
  if (resp.status === 401 && path !== "/api/login") {
    showLogin();
    throw new Error("not logged in");
  }
  if (resp.status === 428) {
    // signed in on a temporary password: the server opens nothing else
    showChoosePassword(null);
    throw new Error("choose your own password first");
  }
  const data = await resp.json().catch(() => ({}));
  if (!resp.ok) throw new Error(data.error || resp.statusText);
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
    localStorage.setItem(SNAP_KEY, JSON.stringify({ holidays: state.holidays, pins: state.pins, at: Date.now() }));
  } catch { /* storage full or blocked — the offline view just won't have data */ }
}

function restoreSnapshot() {
  try {
    const s = JSON.parse(localStorage.getItem(SNAP_KEY));
    if (!s || !Array.isArray(s.holidays)) return false;
    state.holidays = s.holidays;
    state.pins = s.pins || [];
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

function queuePin(pin) {
  pin.qid = Date.now() + ":" + Math.random().toString(36).slice(2, 8);
  setPinQueue([...pinQueue(), pin]);
  renderMarkers();
}

// Network failures (offline, server unreachable) surface as TypeError from
// fetch; HTTP-level failures come back as plain Errors from api(). Only the
// former mean "worth retrying later".
function isNetworkError(ex) {
  return ex instanceof TypeError;
}

let syncingQueue = false;
async function syncQueue() {
  if (syncingQueue || SHARE) return;
  let q = pinQueue();
  if (!q.length) return;
  syncingQueue = true;
  let sent = 0, dropped = 0;
  try {
    while (q.length) {
      const { qid, ...pin } = q[0];
      try {
        await api("POST", "/api/pins", pin);
        sent++;
      } catch (ex) {
        // still offline, or logged out mid-queue: stop and keep everything
        if (isNetworkError(ex) || ex.message === "not logged in") break;
        dropped++; // the server said no (trip deleted?) — retrying forever won't fix it
      }
      q = q.slice(1);
      setPinQueue(q);
    }
  } finally {
    syncingQueue = false;
  }
  if (!sent && !dropped) return;
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

// Pins wear three outfits by zoom: enamel dots at country/world zoom (so a
// hundred photo stops never blanket a continent), small prints at region
// zoom, full photo prints with counts up close. CSS reads data-zoom.
function applyZoomTier() {
  const z = map.getZoom();
  $("map").dataset.zoom = z >= 11 ? "near" : z >= 7 ? "mid" : "far";
}
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

async function loadData(fit) {
  const [holidays, pins] = await Promise.all([api("GET", "/api/holidays"), api("GET", "/api/pins")]);
  state.holidays = holidays;
  state.pins = pins;
  renderAll(fit);
  saveSnapshot();
  syncQueue(); // reaching the server just now proves any queued pins can go
}

function renderAll(fit) {
  if (story.hid !== null && !holidayById(story.hid)) closeStory();
  if (state.focused !== null && !holidayById(state.focused)) state.focused = null;
  renderMarkers();
  refreshStory();
  renderSheet();
  renderBanner();
  if (fit && !state.didFit) {
    state.didFit = true;
    fitAll();
  }
}

function visiblePins() {
  return state.pins.filter((p) => !state.hidden.has(p.holiday_id));
}

function byVisit(a, b) {
  return a.visited_at < b.visited_at ? -1 : a.visited_at > b.visited_at ? 1 : a.id - b.id;
}

function fitAll() {
  const pts = visiblePins().map((p) => [p.lat, p.lng]);
  // where a live or planned trip is headed belongs in the opening view too
  for (const h of state.holidays) {
    if (hasDest(h) && !h.end_at && !state.hidden.has(h.id)) pts.push([h.dest_lat, h.dest_lng]);
  }
  if (pts.length) map.fitBounds(pts, { padding: [50, 50], maxZoom: 12 });
}

/* ---------- the journey: the way there, then the trip itself ---------- */

// A pin has arrived when it's inside the destination's own area (the box
// place search gave it: a city, a national park, a whole country) or within
// ARRIVE_KM of its point — the car park, the hotel across town. The radius is
// tight on purpose: Warwick Services on the M40 is 8.5 km from the castle, and
// a services stop is still the drive.
const ARRIVE_KM = 5;

function inBox(p, b) {
  if (!b) return false;
  const [s, w, n, e] = b;
  if (p.lat < s || p.lat > n) return false;
  return w <= e ? p.lng >= w && p.lng <= e : p.lng >= w || p.lng <= e; // crosses 180°
}

function arrivedAt(h, p) {
  return inBox(p, h.dest_bbox) || kmBetween(p, { lat: h.dest_lat, lng: h.dest_lng }) <= ARRIVE_KM;
}

function hasDest(h) {
  return !!(h && h.dest_name);
}

// The destination's own name: "Warwick Castle, Castle Hill, …" -> "Warwick Castle".
function destShort(h) {
  return h.dest_name.split(",")[0].trim();
}

function kmBetween(a, b) {
  const rad = Math.PI / 180;
  const dLat = (b.lat - a.lat) * rad, dLng = (b.lng - a.lng) * rad;
  const x = Math.sin(dLat / 2) ** 2 + Math.cos(a.lat * rad) * Math.cos(b.lat * rad) * Math.sin(dLng / 2) ** 2;
  return 12742 * Math.asin(Math.sqrt(x));
}

// Splits a trip's pins (in visit order) at the first one that reached the
// destination. Everything before it was on the way there. A live trip that
// hasn't arrived is still on the way; a finished trip that never pinned near
// its destination isn't split at all — its destination was a region, or the
// plans changed, and calling the whole trip "on the way" would be wrong.
function journey(h, pins) {
  const none = { way: [], stay: pins, arrived: false, enRoute: false };
  if (!hasDest(h)) return none;
  const i = pins.findIndex((p) => arrivedAt(h, p));
  if (i >= 0) return { way: pins.slice(0, i), stay: pins.slice(i), arrived: true, enRoute: false };
  if (h.active) return { way: pins, stay: [], arrived: false, enRoute: true };
  return none;
}

/* ---------- markers ---------- */

function pinIcon(pin, color) {
  if (pin.kind === "photo") {
    // A small landscape photo print laid on the atlas: cream print border,
    // enamel edge in the trip's colour, count on the corner. The print is
    // centred in a fixed icon box so zoom-tier resizing stays anchored.
    const div = el("div", "photo-pin");
    div.style.setProperty("--c", color);
    const print = el("span", "print");
    // offline the thumb request can't succeed — the empty cream print reads
    // better than a broken-image glyph
    if (pin.cover_asset && navigator.onLine) {
      const img = el("img");
      img.src = photoURL(pin.cover_asset, "thumb");
      img.alt = "";
      print.appendChild(img);
    }
    div.appendChild(print);
    div.appendChild(el("span", "badge-count", String(pin.photo_count)));
    return L.divIcon({ html: div.outerHTML, iconSize: [48, 40], iconAnchor: [24, 20], popupAnchor: [0, -20] });
  }
  const div = el("div", "manual-pin");
  div.style.setProperty("--c", color);
  div.style.position = "relative";
  return L.divIcon({ html: div.outerHTML, iconSize: [22, 22], iconAnchor: [4, 20], popupAnchor: [7, -18] });
}

// Markers are reconciled, not rebuilt. Tearing the whole layer down on every
// loadData() meant every pin replayed its pop-in animation after any edit or
// reconnect, and it threw away Leaflet's DOM for pins that had not changed.
// Now a marker is only rebuilt when something it draws from actually differs.
function pinSig(pin, color) {
  return [pin.kind, color, pin.cover_asset, pin.photo_count, navigator.onLine ? 1 : 0].join("|");
}

function renderMarkers() {
  const want = new Map(); // key -> descriptor
  for (const pin of visiblePins()) {
    const h = holidayById(pin.holiday_id);
    want.set("p" + pin.id, { pin, color: h ? h.color : "#666" });
  }
  // Pins waiting to sync ride along as ghosts: same pushpin, dashed and
  // faded. They're local-only until the network returns; tap to discard.
  for (const qp of pinQueue()) {
    if (state.hidden.has(qp.holiday_id)) continue;
    const h = holidayById(qp.holiday_id);
    want.set("q" + qp.qid, { qp, color: h ? h.color : "#666" });
  }

  for (const [key, marker] of state.markers) {
    if (!want.has(key)) { marker.remove(); state.markers.delete(key); }
  }

  const byTrip = new Map();
  for (const [key, w] of want) {
    const pending = !w.pin;
    const src = w.pin || w.qp;
    const sig = pending ? "q|" + w.color : pinSig(w.pin, w.color);
    let marker = state.markers.get(key);
    if (!marker) {
      marker = L.marker([src.lat, src.lng], {
        icon: pending ? pendingIcon(w.color) : pinIcon(w.pin, w.color),
        riseOnHover: true,
      }).addTo(map);
      marker.sig = sig;
      state.markers.set(key, marker);
    } else {
      const ll = marker.getLatLng();
      if (ll.lat !== src.lat || ll.lng !== src.lng) marker.setLatLng([src.lat, src.lng]);
      if (marker.sig !== sig) {
        marker.setIcon(pending ? pendingIcon(w.color) : pinIcon(w.pin, w.color));
        marker.sig = sig;
      }
    }
    marker.tripId = src.holiday_id;
    marker.pinId = pending ? null : w.pin.id;
    marker.qp = pending ? w.qp : null;
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
    if (!pending) {
      if (!byTrip.has(w.pin.holiday_id)) byTrip.set(w.pin.holiday_id, []);
      byTrip.get(w.pin.holiday_id).push(w.pin);
    }
  }

  // The itinerary line: a dotted ink route joining each trip's stops in the
  // order they were visited (visited_at = first photo's taken time, so pins
  // added after the fact still land in the right leg of the journey). Lines
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
    const line = L.polyline(latlngs, {
      color: h ? h.color : "#666",
      weight: 2.5, dashArray: dash, lineCap: "round", opacity: 0.8,
      interactive: false, // never steal taps from pins or the map
      className: "route-line " + cls,
    }).addTo(map);
    line.tripId = hid;
    state.routes.push(line);
  };
  const at = (p) => [p.lat, p.lng];
  for (const [hid, pins] of byTrip) {
    pins.sort(byVisit);
    const h = holidayById(hid);
    const j = journey(h, pins);
    // the way-there line runs into the arrival pin, so the two legs join
    draw(hid, [...j.way, ...j.stay.slice(0, 1)].map(at), "route-way", "7 8");
    draw(hid, j.stay.map(at), "route-stay", "1 9");
    if (j.enRoute && pins.length) draw(hid, [at(pins[pins.length - 1]), [h.dest_lat, h.dest_lng]], "route-ahead", "7 8");
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
    const pins = state.pins.filter((p) => p.holiday_id === h.id).sort(byVisit);
    want.set(h.id, { h, arrived: journey(h, pins).arrived });
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
  div.style.position = "relative";
  return L.divIcon({ html: div.outerHTML, iconSize: [22, 22], iconAnchor: [4, 20], popupAnchor: [7, -18] });
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

const story = { hid: null, loadObserver: null, activeEl: null, holdUntil: 0 };

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
  const path = SHARE ? `/api/share/${SHARE}/pins/${grid.dataset.pin}/photos`
                     : `/api/pins/${grid.dataset.pin}/photos`;
  api("GET", path).then((photos) => {
    grid.textContent = "";
    photos.slice(0, STORY_THUMBS).forEach((ph, i) => {
      const img = el("img");
      img.loading = "lazy";
      img.src = photoURL(ph.asset_id, "thumb");
      img.alt = "";
      img.onclick = (e) => { e.stopPropagation(); openLightbox(photos, i); };
      grid.appendChild(img);
    });
    const more = sec.querySelector(".story-more");
    if (more) {
      more.disabled = false;
      more.textContent = `All ${photos.length} photos`;
      more.onclick = (e) => { e.stopPropagation(); openLightbox(photos, 0); };
    }
  }).catch(() => { grid.textContent = ""; grid.appendChild(el("span", "pop-sub", "Couldn't load photos")); });
}

function openStory(h, pinId) {
  closeStory();
  story.hid = h.id;
  state.focused = h.id;
  document.body.classList.add("storying");
  setSheet(false);
  $("story-title").textContent = h.name;
  $("story").style.setProperty("--c", h.color); // the cover bar's edge and the leg marks
  const scroll = $("story-scroll");
  scroll.textContent = "";
  scroll.scrollTop = 0;

  const pins = state.pins.filter((p) => p.holiday_id === h.id).sort(byVisit);
  const j = journey(h, pins);
  const secs = [];
  // Leg headings mark where the drive ends and the trip begins. They carry no
  // coordinates, so the scroll sync steps over them.
  const leg = (text, cls) => scroll.appendChild(el("h4", "story-leg " + cls, text));
  if (j.way.length) leg(j.arrived ? `The way to ${destShort(h)}` : `On the way to ${destShort(h)}`, "leg-way");
  for (const pin of pins) {
    if (j.arrived && pin === j.stay[0]) leg(`Arrived at ${destShort(h)}`, "leg-arrived");
    const sec = el("section", "story-sec");
    if (j.way.includes(pin)) sec.classList.add("on-the-way");
    sec.dataset.lat = pin.lat;
    sec.dataset.lng = pin.lng;
    sec.dataset.pinId = pin.id;
    const dayN = Math.max(1, Math.floor((new Date(pin.visited_at) - new Date(h.start_at)) / 86400000) + 1);
    sec.appendChild(el("p", "story-day", `Day ${dayN} · ${fmtDate(pin.visited_at)}`));
    sec.appendChild(el("h3", "story-place", pin.title || (pin.kind === "photo" ? "Photo stop" : "Pin")));
    if (pin.note) sec.appendChild(el("p", "story-note", pin.note));
    if (pin.photo_count > 0) {
      // A chapter shows a contact strip, not the whole roll. Day 1 of a trip
      // can carry 20+ photos, and an uncapped grid made every chapter several
      // screens tall — you scrolled photographs instead of chapters, so the
      // map never got to fly between the stops. The rest are one tap away.
      const shown = Math.min(pin.photo_count, STORY_THUMBS);
      const grid = el("div", "story-grid");
      grid.dataset.pin = pin.id;
      // Placeholder cells reserve the grid's final height before the photos
      // arrive — chapters must not grow later, or the open-at-pin scroll (and
      // the reader's place) slides as content above them expands.
      for (let i = 0; i < shown; i++) grid.appendChild(el("span", "ph"));
      sec.appendChild(grid);
      if (pin.photo_count > shown) {
        const more = el("button", "story-more", `All ${pin.photo_count} photos`);
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
      sec.appendChild(editForm(pin));
      const cancel = el("button", "linkish", "Cancel");
      cancel.onclick = (ev) => { ev.stopPropagation(); refreshStory(); };
      sec.appendChild(cancel);
    };
    const del = el("button", null, "Delete");
    del.onclick = async (e) => {
      e.stopPropagation();
      if (!confirm(pin.photo_count > 0 ? "Delete this pin? Its photos stay in Immich." : "Delete this pin?")) return;
      await api("DELETE", `/api/pins/${pin.id}`);
      loadData();
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
    const last = pins[pins.length - 1];
    const label = h.planned ? countdown(h)
      : last ? `Still to go · ${Math.round(kmBetween(last, { lat: h.dest_lat, lng: h.dest_lng }))} km as the crow flies`
      : "Heading here";
    sec.appendChild(el("p", "story-day", label));
    sec.appendChild(el("h3", "story-place", destShort(h)));
    if (h.dest_name !== destShort(h)) sec.appendChild(el("p", "story-dest-full", h.dest_name));
    sec.onclick = () => activateSection(sec);
    scroll.appendChild(sec);
    secs.push(sec);
  }
  if (h.journal) {
    const sec = el("section", "story-sec");
    sec.appendChild(el("h3", "story-place", "Journal"));
    sec.appendChild(el("p", "story-note", h.journal));
    scroll.appendChild(sec);
  }
  if (h.unplaced_count > 0) {
    const sec = el("section", "story-sec");
    sec.appendChild(el("h3", "story-place", "Photos without a location"));
    const file = el("button", "sub-link", `file ${h.unplaced_count} photos onto pins`);
    file.onclick = () => openUnplaced(h);
    sec.appendChild(file);
    scroll.appendChild(sec);
  }
  if (!secs.length && !h.unplaced_count) {
    scroll.appendChild(el("p", "empty-note", "No pins on this trip yet — the story writes itself as you pin places."));
  }
  $("story").hidden = false;
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
    ? secs.find((s) => s.dataset.dest) || secs.find((s) => j.stay[0] && s.dataset.pinId === String(j.stay[0].id))
    : pinId != null && secs.find((s) => s.dataset.pinId === String(pinId));
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
  const activePin = act ? (act.dataset.dest ? "dest" : act.dataset.pinId) : null;
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

function closeStory() {
  if (story.hid === null) return;
  story.loadObserver.disconnect();
  story.loadObserver = null;
  story.activeEl = null;
  story.hid = null;
  $("story").hidden = true;
  document.body.classList.remove("storying");
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

function editForm(pin) {
  const form = el("form", "pop-form");
  const title = el("input");
  title.type = "text"; title.placeholder = "Title"; title.value = pin.title; title.maxLength = 80;
  const note = el("textarea");
  note.placeholder = "Notes"; note.value = pin.note;
  const save = el("button", "primary", "Save");
  save.type = "submit";
  let cover = pin.cover_asset;
  form.append(title, note);
  if (pin.photo_count > 0) {
    form.appendChild(coverPick(api("GET", `/api/pins/${pin.id}/photos`), pin.cover_asset, (a) => { cover = a; }));
  }
  form.appendChild(save);
  form.onsubmit = async (e) => {
    e.preventDefault();
    const body = { title: title.value, note: note.value };
    if (cover && cover !== pin.cover_asset) body.cover_asset = cover;
    try {
      await api("PATCH", `/api/pins/${pin.id}`, body);
      loadData(); // refreshStory rebuilds the chapter with the new values
      toast("Pin saved");
    } catch (err) {
      toast(err.message);
    }
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
  const coords = el("p", "pop-coords", `${latlng.lat.toFixed(5)}, ${latlng.lng.toFixed(5)}`);
  coords.title = "Tap to copy the coordinates";
  coords.onclick = () => {
    navigator.clipboard?.writeText(coords.textContent)
      .then(() => toast("Coordinates copied"))
      .catch(() => {});
  };
  form.append(coords, title, note, trip, save);
  const popup = L.popup({ maxWidth: 300, minWidth: 220 }).setLatLng(latlng).setContent(form).openOn(map);
  title.focus();
  form.onsubmit = async (e) => {
    e.preventDefault();
    const pin = {
      holiday_id: Number(trip.value),
      lat: latlng.lat, lng: latlng.lng,
      title: title.value, note: note.value,
    };
    // created_at rides along so a pin synced hours later still lands on the
    // right day of the trip's story
    const offline = () => {
      queuePin({ ...pin, created_at: new Date().toISOString() });
      map.closePopup(popup);
      toast("No signal — pin saved, will sync when you're back online");
    };
    if (!navigator.onLine) { offline(); return; } // known-dead link: skip the doomed request
    try {
      await api("POST", "/api/pins", pin);
      map.closePopup(popup);
      loadData();
      toast("Pin added");
    } catch (err) {
      if (isNetworkError(err)) offline();
      else toast(err.message);
    }
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

/* ---------- trips sheet ---------- */

function fmtDate(s) {
  return new Date(s).toLocaleDateString(undefined, { day: "numeric", month: "short", year: "numeric" });
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
  const s = new Date(a), e = new Date(b);
  if (isNaN(s) || isNaN(e)) return fmtDate(a);
  if (rangeFmt) return rangeFmt.formatRange(s, e);
  return `${fmtDate(a)} – ${fmtDate(b)}`;
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
  if (h.active) return `since ${fmtDate(h.start_at)}`;
  return fmtRange(h.start_at, h.end_at);
}

function renderSheet() {
  const dots = $("pill-dots");
  dots.textContent = "";
  state.holidays.slice(0, 6).forEach((h) => {
    const i = el("i");
    i.style.background = h.color;
    dots.appendChild(i);
  });
  $("pill-label").textContent = state.holidays.length ? `Trips · ${state.holidays.length}` : "Trips";

  const list = $("trip-list");
  list.textContent = "";
  if (!state.holidays.length) {
    // the sheet's first page: the same blank stamp the passport shows
    const blank = el("div", "sheet-blank");
    blank.appendChild(el("div", "stamp-blank", "No entries yet"));
    blank.appendChild(el("p", "form-hint",
      "Start a holiday as you set off, or add a past trip by giving it both dates."));
    list.appendChild(blank);
  }
  for (const h of state.holidays) {
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
      : `${tripRange(h)} · ${h.pin_count} pins · ${h.photo_count} photos`));

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
      const pts = state.pins.filter((p) => p.holiday_id === h.id).map((p) => [p.lat, p.lng]);
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
  }
}

function tripEditForm(h) {
  const wrap = el("form", "trip-edit");
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
  const destRemove = el("button", "linkish", "Remove destination");
  destRemove.type = "button";
  const showDest = () => {
    const d = newDest === undefined ? (h.dest_name ? { name: h.dest_name } : null) : newDest;
    destNow.textContent = d ? `Destination: ${d.name}` : "No destination yet";
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
  destBox.append(destNow, destRow, destRes, destRemove);
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
    await api("DELETE", `/api/holidays/${h.id}`);
    toast("Trip deleted");
    loadData();
  };
  const btnRow = el("div", "form-row");
  btnRow.append(save, del);
  let cover = h.cover_asset;
  // A live or planned trip gets its last day by being ended.
  wrap.append(name, sw, startRow, h.active || h.planned ? el("span") : endRow, destBox);
  if (h.photo_count > 0 || h.unplaced_count > 0) {
    wrap.appendChild(coverPick(api("GET", `/api/holidays/${h.id}/timeline`), h.cover_asset, (a) => { cover = a; }));
  }
  const shareRow = el("div", "share-row");
  const mkShare = el("button", "linkish", h.shared ? "New share link (replaces the old one)" : "Share this trip — view-only link");
  mkShare.type = "button";
  mkShare.onclick = async () => {
    try {
      const res = await api("POST", `/api/holidays/${h.id}/share`);
      const url = location.origin + res.url;
      try {
        await navigator.clipboard.writeText(url);
        toast("View-only link copied — anyone with it can see this trip");
      } catch {
        window.prompt("Copy the view-only link:", url);
      }
      loadData();
    } catch (err) {
      toast(err.message);
    }
  };
  shareRow.appendChild(mkShare);
  if (h.shared) {
    const revoke = el("button", "linkish", "Revoke share link");
    revoke.type = "button";
    revoke.onclick = async () => {
      await api("DELETE", `/api/holidays/${h.id}/share`);
      toast("Share link revoked");
      loadData();
    };
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
    try {
      await api("PATCH", `/api/holidays/${h.id}`, body);
      toast("Trip saved");
      const dates = startChanged || endChanged;
      loadData();
      if (dates) api("POST", `/api/holidays/${h.id}/sync`).then(() => loadData()).catch(() => {});
    } catch (err) {
      toast(err.message);
    }
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
    const days = taken.reduce((sum, h) => {
      const end = h.end_at ? new Date(h.end_at) : new Date();
      return sum + Math.max(1, Math.round((end - new Date(h.start_at)) / 86400000) + 1);
    }, 0);
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
      card.appendChild(el("div", "stamp-admit", "Admitted \u00b7 " + fmtDate(s.start_at)));
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
    openOverlay("Passport", spread);
  } catch (err) {
    toast(err.message);
  }
};

// dayGroupedGallery lays photos out in taken order under day headings.
// onClick(photos, index) handles a tap; if absent, taps open the lightbox.
function dayGroupedGallery(photos, onClick) {
  const wrap = el("div");
  let lastDay = "";
  let grid = null;
  photos.forEach((ph, i) => {
    const day = fmtDate(ph.taken_at);
    if (day !== lastDay) {
      wrap.appendChild(el("h3", "day-head", day));
      grid = el("div", "gallery-grid");
      wrap.appendChild(grid);
      lastDay = day;
    }
    const img = el("img");
    img.loading = "lazy";
    img.src = photoURL(ph.asset_id, "thumb");
    img.alt = "";
    img.dataset.asset = ph.asset_id;
    img.onclick = () => (onClick ? onClick(photos, i, img) : openLightbox(photos, i));
    grid.appendChild(img);
  });
  return wrap;
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
        toast(`Moved ${r.moved} photos onto the pin`);
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
        await api("DELETE", `/api/packing/templates/${t.id}`);
        openMasterLists();
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
  const day = Math.max(1, Math.floor((Date.now() - new Date(active.start_at)) / 86400000) + 1);
  $("banner-stub").hidden = false;
  $("stub-n").textContent = day > 1 ? String(day) : "day";
  $("stub-unit").textContent = day > 1 ? "day" : "first";
  $("stub-n").classList.remove("word");
  $("banner-stub").setAttribute("aria-label", `Day ${day} of ${active.name}`);
  $("btn-end").onclick = async () => {
    if (!confirm(`End "${active.name}"? Photos taken from now on won't join it.`)) return;
    const r = await api("POST", `/api/holidays/${active.id}/end`);
    toast(r.sync_error ? "Holiday ended (photo sync failed — use Sync later)" : "Holiday ended — happy memories!");
    loadData();
  };
  $("btn-sync").onclick = async () => {
    toast("Syncing photos…");
    try {
      const r = await api("POST", `/api/holidays/${active.id}/sync`);
      const extra = r.unplaced ? ` (+${r.unplaced} without location)` : "";
      toast(`Synced: ${r.photos} photos across ${r.photo_pins} places${extra}`);
      loadData();
    } catch (err) {
      toast(err.message);
    }
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
    b.title = c;
    if (c === initial) b.classList.add("sel");
    b.onclick = () => {
      onPick(c);
      wrap.querySelectorAll("button").forEach((x) => x.classList.remove("sel"));
      b.classList.add("sel");
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
const COORD_RE = /^\s*(-?\d+(?:\.\d+)?)\s*[, ]\s*(-?\d+(?:\.\d+)?)\s*$/;

function resetDest() {
  chosenDest = null;
  autoName = "";
  $("trip-dest").value = "";
  $("dest-results").textContent = "";
}

// placeSearch runs one lookup into a results box and calls onPick with the
// chosen place: { name, lat, lng, bbox? }. Typed "lat, lng" is taken as-is.
// Shared by the new-trip form and trip editing.
async function placeSearch(q, res, onPick) {
  q = q.trim();
  if (!q) return;
  const m = q.match(COORD_RE);
  if (m) {
    const lat = +m[1], lng = +m[2];
    if (lat < -90 || lat > 90 || lng < -180 || lng > 180) { toast("Coordinates out of range"); return; }
    const d = { name: `${lat.toFixed(5)}, ${lng.toFixed(5)}`, lat, lng };
    res.textContent = "";
    res.appendChild(el("p", "form-hint", `Destination set: ${d.name}`));
    onPick(d); // coordinates make a poor trip name
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
        onPick({ name: hit.name.split(",").slice(0, 2).join(","), lat: hit.lat, lng: hit.lng, bbox: hit.bbox },
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
  if (d.bbox) b.dest_bbox = d.bbox;
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
$("new-trip-form").onsubmit = async (e) => {
  e.preventDefault();
  try {
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
      .then((r) => { toast(`Found ${r.photos} photos across ${r.photo_pins} places`); loadData(); })
      .catch(() => {});
  } catch (err) {
    toast(err.message);
  }
};

/* ---------- sheet + admin + logout ---------- */

$("sheet-pill").onclick = () => setSheet($("sheet").classList.contains("collapsed"));

$("btn-manifest").onclick = () => openMasterLists();

$("btn-family").onclick = () => openFamily();

$("password-form").onsubmit = async (e) => {
  e.preventDefault();
  try {
    await api("POST", "/api/password", {
      current_password: $("pass-current").value,
      new_password: $("pass-new").value,
    });
    $("pass-current").value = ""; $("pass-new").value = "";
    $("pass-box").open = false;
    toast("Password changed — other devices were signed out");
  } catch (err) {
    toast(err.message);
  }
};

$("btn-logout").onclick = async () => {
  await api("POST", "/api/logout", {});
  location.reload();
};

/* ---------- lightbox ---------- */

const lb = { photos: [], idx: 0 };

function openLightbox(photos, idx) {
  lb.photos = photos;
  lb.idx = idx;
  modalOpen("lightbox", $("lb-close"));
  showLightbox();
}

function showLightbox() {
  const ph = lb.photos[lb.idx];
  $("lb-img").src = photoURL(ph.asset_id, "preview");
  $("lb-count").textContent = `${lb.idx + 1} / ${lb.photos.length}`;
  $("lb-date").textContent = fmtDate(ph.taken_at);
  $("lb-orig").href = photoURL(ph.asset_id, "original");
  $("lb-prev").style.visibility = lb.idx > 0 ? "visible" : "hidden";
  $("lb-next").style.visibility = lb.idx < lb.photos.length - 1 ? "visible" : "hidden";
}

function lbStep(d) {
  const next = lb.idx + d;
  if (next >= 0 && next < lb.photos.length) { lb.idx = next; showLightbox(); }
}

$("lb-close").onclick = () => { modalClose("lightbox"); $("lb-img").src = ""; };
$("lb-prev").onclick = () => lbStep(-1);
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
    b.onclick = async () => {
      try {
        await fn();
      } catch (err) {
        toast(err.message);
      }
    };
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
  openOverlay("Family", box);
}

/* ---------- boot ---------- */

(async function boot() {
  if (SHARE) {
    document.body.classList.add("shared");
    try {
      const d = await api("GET", `/api/share/${SHARE}`);
      state.holidays = [d.holiday];
      state.pins = d.pins;
      renderMarkers();
      openStory(d.holiday);
    } catch {
      openOverlay("Link expired", el("p", "empty-note",
        "This share link is no longer active — ask the sender for a fresh one."));
      $("overlay-close").hidden = true;
    }
    return;
  }
  try {
    state.me = await api("GET", "/api/me");
  } catch (ex) {
    // A dead network (unlike a 401) means mid-holiday with no signal: open
    // the last synced map from the snapshot instead of a blank atlas.
    if (isNetworkError(ex) && restoreSnapshot()) {
      toast("Offline — showing your last synced map");
    }
    return; // on 401 the login overlay is already shown
  }
  if (state.me.must_change_password) return showChoosePassword(null);
  $("btn-family").hidden = !state.me.is_admin;
  $("app-version").textContent = state.me.version === "dev" ? "dev build" : state.me.version;
  try {
    await loadData(true);
  } catch (ex) {
    toast("Couldn't load the map data: " + ex.message);
  }
})();

// App-shell service worker: the map opens (from cache) even with no signal.
// navigator.serviceWorker only exists on HTTPS/localhost, so plain-HTTP LAN
// visits skip this silently — same caveat as the clipboard API.
if ("serviceWorker" in navigator && !SHARE) {
  navigator.serviceWorker.register("/sw.js").catch(() => {});
}
