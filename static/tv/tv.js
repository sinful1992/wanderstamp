// Wanderstamp TV: the /tv page on the home-network door (CONTRACT.md, task 133).
// Screens: pair (C), shelf (B), journey (A), photos, offline. Driven by the
// remote's keys only; the TV app calls WSTV.back() for its Back button.
"use strict";

const PHOTO_MS = 6000, CARD_MS = 5000, POLL_GAP_MS = 1500, RETRY_MS = 5000;
const TILES = "https://server.arcgisonline.com/ArcGIS/rest/services/World_Topo_Map/MapServer/tile/{z}/{y}/{x}";
const TILE_ATTR = "Tiles &copy; Esri";
const ROUTE_MAX = 10; // stop dots shown at once in the journey's route bar

const $ = (id) => document.getElementById(id);
function el(tag, cls, text) {
  const n = document.createElement(tag);
  if (cls) n.className = cls;
  if (text !== undefined) n.textContent = text;
  return n;
}
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

/* ---------- stage ---------- */

function fitStage() {
  const h = document.documentElement;
  const s = Math.min(h.clientWidth / 1920, h.clientHeight / 1080);
  $("stage").style.transform = `scale(${s})`;
}
window.addEventListener("resize", fitStage);
fitStage();

/* ---------- api ---------- */

class NotPaired extends Error {}
class Offline extends Error {}

async function api(path, opts) {
  let r;
  try {
    r = await fetch(path, { credentials: "same-origin", cache: "no-store", ...opts });
  } catch {
    throw new Offline("network");
  }
  if (r.status === 401) throw new NotPaired("not paired");
  let data = null;
  try { data = await r.json(); } catch { /* empty or not JSON */ }
  if (!r.ok && r.status !== 202) {
    const e = new Error((data && data.error) || r.statusText);
    e.status = r.status;
    throw e;
  }
  return { status: r.status, data };
}

const photoURL = (asset, kind) => `/api/tv/photo/${encodeURIComponent(asset)}/${kind}`;

/* ---------- dates (same rules as the app: app.js tripDay/calDay) ---------- */

function calDay(s) {
  if (/^\d{4}-\d{2}-\d{2}(T(00:00:00|23:59:59)(\.\d+)?(Z|[+-]00:00))?$/.test(s)) {
    const [y, m, d] = s.slice(0, 10).split("-").map(Number);
    return new Date(y, m - 1, d);
  }
  return new Date(s);
}

function tripDay(h, at) {
  const t = new Date(new Date(at).getTime() - 4 * 3600000);
  const s0 = calDay(h.start_at);
  const first = new Date(s0.getFullYear(), s0.getMonth(), s0.getDate());
  const today = new Date(t.getFullYear(), t.getMonth(), t.getDate());
  const n = Math.max(1, Math.round((today - first) / 86400000) + 1);
  const label = t.toLocaleDateString(undefined, { weekday: "long", day: "numeric", month: "long" });
  return { n, label };
}

// Text with its numbers in the body face (the stamp face draws "1" like "I").
function numbered(text) {
  return String(text).split(/(\d+)/).filter(Boolean).map((part) => (/^\d+$/.test(part) ? el("span", "num", part) : document.createTextNode(part)));
}

function monthYear(s) {
  const d = calDay(s);
  return isNaN(d) ? "" : d.toLocaleDateString(undefined, { month: "long", year: "numeric" });
}

/* ---------- screens + keys ---------- */

const tv = {
  screen: null,   // id of the screen on show
  gen: 0,         // bumps on every screen change: loops of an old screen stop
  trips: [],
  focus: 0,       // shelf: focused trip
  row: 0,         // shelf: 0 = trips, 1 = actions
  act: 0,         // shelf: focused action
  details: new Map(), // trip id -> {holiday, pins, photos}
};

function show(id) {
  tv.gen++;
  stopJourney();
  for (const s of document.querySelectorAll(".screen")) s.hidden = s.id !== id;
  tv.screen = id;
  document.body.dataset.screen = id;
  return tv.gen;
}

// The shelf, or the offline screen (which retries) when it can't load: never a
// blank shelf after a failed call.
function shelfOrRetry(reload) {
  return openShelf(reload).catch((e) => {
    if (e instanceof NotPaired) return startPair();
    console.error(e);
    goOffline();
  });
}

// What happens when any call says the TV isn't paired (any more): back to the code.
function handleError(e) {
  if (e instanceof NotPaired) return startPair();
  if (e instanceof Offline) return goOffline();
  console.error(e);
}

const KEYS = {
  ArrowLeft: "left", ArrowRight: "right", ArrowUp: "up", ArrowDown: "down",
  Left: "left", Right: "right", Up: "up", Down: "down",
  Enter: "ok", " ": "playpause", MediaPlayPause: "playpause", MediaPlay: "playpause", MediaPause: "playpause",
  MediaTrackNext: "right", MediaFastForward: "right", MediaTrackPrevious: "left", MediaRewind: "left",
  Escape: "back", Backspace: "back", BrowserBack: "back", GoBack: "back",
};

document.addEventListener("keydown", (e) => {
  const k = KEYS[e.key];
  if (!k) return;
  e.preventDefault();
  if (k === "back") { back(); return; }
  const h = handlers[tv.screen];
  if (h) h(k);
});

// Back: true when the page used it, false when the TV app should close.
function back() {
  switch (tv.screen) {
    case "journey": shelfOrRetry(true); return true;
    case "photos":
      if (!$("p-viewer").hidden) { closeViewer(); return true; }
      shelfOrRetry(true);
      return true;
    case "shelf":
      if (tv.row === 1) { tv.row = 0; drawShelfFocus(); return true; }
      return false;
    default: return false;
  }
}

const handlers = {};

/* ---------- start ---------- */

async function start() {
  try {
    await api("/api/tv/me");
  } catch (e) {
    if (e instanceof NotPaired || e instanceof Offline) return handleError(e);
    console.error(e);
    return goOffline(); // a server error at start: try again, never a blank screen
  }
  await shelfOrRetry(true);
}

async function goOffline() {
  const g = show("offline");
  await sleep(RETRY_MS);
  if (g === tv.gen) start();
}

/* ---------- C: pairing ---------- */

async function startPair() {
  const g = show("pair");
  // another account's trips must not show while (or after) this TV pairs again
  tv.trips = [];
  tv.details.clear();
  const codeBox = $("pair-code"), state = $("pair-state");
  while (g === tv.gen) {
    let pair;
    try {
      const r = await api("/tv/pair", { method: "POST" });
      pair = r.data;
    } catch (e) {
      codeBox.textContent = "";
      state.textContent = e instanceof Offline ? "Can't reach Wanderstamp, trying again…" : "Waiting for a new code…";
      await sleep(e.status === 429 ? 15000 : RETRY_MS);
      continue;
    }
    if (g !== tv.gen) return;
    drawCode(pair.code);
    state.textContent = "Waiting for your phone…";
    // poll until paired, or the code is gone (410) and a new one is due
    for (;;) {
      if (g !== tv.gen) return;
      let r;
      try {
        r = await api(`/tv/pair/${encodeURIComponent(pair.poll)}`);
      } catch (e) {
        if (e.status === 410) break;
        await sleep(e instanceof Offline ? RETRY_MS : POLL_GAP_MS);
        continue;
      }
      if (r.status === 200) {
        if (g !== tv.gen) return;
        state.textContent = `Paired as ${(r.data && r.data.name) || "this TV"}`;
        await shelfOrRetry(true);
        return;
      }
      // 202: still waiting; the server held the request, ask again
    }
  }
}

function drawCode(code) {
  const box = $("pair-code");
  box.textContent = "";
  const tilt = [-3, 2, -1, 3, -2, 1];
  [...String(code || "")].forEach((c, i) => {
    const s = el("span", null, c);
    s.style.setProperty("--r", `${tilt[i % tilt.length]}deg`);
    box.appendChild(s);
  });
  box.setAttribute("aria-label", `Code ${[...String(code || "")].join(" ")}`);
}

/* ---------- maps ---------- */

let shelfMap = null, journeyMap = null, shelfLayer = null, journeyLayer = null;

function makeMap(id) {
  const m = L.map(id, { zoomControl: false, attributionControl: true, keyboard: false, dragging: false,
    scrollWheelZoom: false, doubleClickZoom: false, boxZoom: false, touchZoom: false, zoomSnap: 1 });
  m.attributionControl.setPrefix(false);
  L.tileLayer(TILES, { attribution: TILE_ATTR, maxZoom: 17, keepBuffer: 4 }).addTo(m);
  m.setView([50, 5], 4);
  return m;
}

/* ---------- B: shelf ---------- */

async function openShelf(reload) {
  const g = show("shelf");
  if (!shelfMap) shelfMap = makeMap("shelf-map");
  shelfMap.invalidateSize();
  if (reload || !tv.trips.length) {
    tv.details.clear(); // a live trip gains stops and photos: fetch them again
    const r = await api("/api/tv/trips");
    if (g !== tv.gen) return;
    const prev = tv.trips[tv.focus];
    tv.trips = Array.isArray(r.data) ? r.data : [];
    const keep = prev ? tv.trips.findIndex((t) => t.id === prev.id) : -1;
    tv.focus = keep >= 0 ? keep : 0;
  }
  tv.row = 0;
  drawShelf();
}

function drawShelf() {
  const track = $("shelf-track");
  track.textContent = "";
  const n = tv.trips.length;
  $("shelf-empty").hidden = n > 0;
  $("shelf-actions").hidden = n === 0;
  const photos = tv.trips.reduce((a, t) => a + (t.photo_count || 0), 0);
  $("shelf-tally").textContent = n ? `${n} ${n === 1 ? "trip" : "trips"}, ${photos.toLocaleString()} ${photos === 1 ? "photo" : "photos"}` : "";
  const tilt = [0, -2, 1.5, -1, 2, -1.5];
  tv.trips.forEach((t, i) => {
    const s = el("div", "tripstamp");
    s.style.setProperty("--r", `${tilt[i % tilt.length]}deg`);
    s.style.setProperty("--c", t.color || "#1c3652");
    s.dataset.id = t.id;
    const face = el("div", "face");
    if (t.cover_asset) {
      const img = el("img");
      img.src = photoURL(t.cover_asset, "thumb");
      img.alt = "";
      img.onerror = () => { img.remove(); face.prepend(el("span", "blank", "✦")); };
      face.appendChild(img);
    } else {
      face.appendChild(el("span", "blank", "✦"));
    }
    face.appendChild(el("div", "band", t.name || "Trip"));
    s.appendChild(face);
    const meta = el("div", "meta num");
    const bits = [monthYear(t.start_at)];
    if (t.pin_count) bits.push(`${t.pin_count} ${t.pin_count === 1 ? "stop" : "stops"}`);
    if (t.active) bits.push("now");
    meta.textContent = bits.filter(Boolean).join(", ");
    s.appendChild(meta);
    track.appendChild(s);
  });
  drawShelfFocus();
}

function drawShelfFocus() {
  const stamps = $("shelf-track").children;
  [...stamps].forEach((s, i) => s.classList.toggle("focus", i === tv.focus));
  // keep the focused stamp on screen, with the one before it peeking in
  const step = 270 + 46;
  const shift = Math.max(0, tv.focus - 1) * step;
  $("shelf-track").style.transform = `translateX(${-shift}px)`;
  $("shelf").classList.toggle("on-actions", tv.row === 1);
  [...$("shelf-actions").children].forEach((b, i) => b.classList.toggle("focus", tv.row === 1 && i === tv.act));
  scheduleShelfMap();
}

// A TV left on the shelf picks up a new trip, or new photos on a live one,
// without moving the focus; nothing redraws when nothing changed.
async function refreshShelf() {
  const g = tv.gen;
  const r = await api("/api/tv/trips");
  if (g !== tv.gen || tv.screen !== "shelf") return;
  const fresh = Array.isArray(r.data) ? r.data : [];
  if (JSON.stringify(fresh) === JSON.stringify(tv.trips)) return;
  const prev = tv.trips[tv.focus];
  tv.trips = fresh;
  tv.details.clear();
  const keep = prev ? fresh.findIndex((t) => t.id === prev.id) : -1;
  tv.focus = keep >= 0 ? keep : Math.min(tv.focus, Math.max(0, fresh.length - 1));
  if (!fresh.length) tv.row = 0;
  drawShelf();
}

let shelfMapTimer = 0;
function scheduleShelfMap() {
  clearTimeout(shelfMapTimer);
  const t = tv.trips[tv.focus];
  if (!t) return;
  const g = tv.gen;
  shelfMapTimer = setTimeout(async () => {
    try {
      const d = await tripDetail(t.id);
      if (g !== tv.gen || tv.trips[tv.focus] !== t) return;
      drawTripOnMap(shelfMap, d, "shelf");
    } catch (e) {
      handleError(e);
    }
  }, 250);
}

async function tripDetail(id) {
  if (tv.details.has(id)) return tv.details.get(id);
  const r = await api(`/api/tv/trips/${id}`);
  const d = r.data || {};
  d.pins = Array.isArray(d.pins) ? d.pins : [];
  d.photos = Array.isArray(d.photos) ? d.photos : [];
  tv.details.set(id, d);
  return d;
}

function drawTripOnMap(m, d, which) {
  const layer = L.layerGroup();
  const color = (d.holiday && d.holiday.color) || "#2a7f62";
  const stops = buildStops(d);
  const pts = stops.map((s) => [s.pin.lat, s.pin.lng]);
  if (pts.length > 1) L.polyline(pts, { color, weight: 6, opacity: 0.9 }).addTo(layer);
  const seen = new Set();
  for (const s of stops) {
    if (seen.has(s.pin.id)) continue;
    seen.add(s.pin.id);
    L.circleMarker([s.pin.lat, s.pin.lng], { radius: 10, color: "#f6f1e3", weight: 4, fillColor: color, fillOpacity: 1 }).addTo(layer);
  }
  if (which === "shelf") {
    if (shelfLayer) shelfLayer.remove();
    shelfLayer = layer.addTo(m);
  }
  const h = d.holiday || {};
  if (pts.length) {
    m.fitBounds(pts, { paddingTopLeft: [520, 90], paddingBottomRight: [120, 200], maxZoom: 12, animate: true });
  } else if (h.dest_name && (h.dest_lat || h.dest_lng)) {
    m.setView([h.dest_lat, h.dest_lng], 9);
  } else {
    m.setView([50, 5], 4);
  }
}

handlers.shelf = (k) => {
  const n = tv.trips.length;
  if (!n) return;
  if (tv.row === 0) {
    if (k === "left" && tv.focus > 0) { tv.focus--; drawShelfFocus(); }
    else if (k === "right" && tv.focus < n - 1) { tv.focus++; drawShelfFocus(); }
    else if (k === "down") { tv.row = 1; tv.act = 0; drawShelfFocus(); }
    else if (k === "ok" || k === "playpause") openJourney(tv.trips[tv.focus]);
  } else {
    const acts = $("shelf-actions").children.length;
    if (k === "left" && tv.act > 0) { tv.act--; drawShelfFocus(); }
    else if (k === "right" && tv.act < acts - 1) { tv.act++; drawShelfFocus(); }
    else if (k === "up") { tv.row = 0; drawShelfFocus(); }
    else if (k === "ok") {
      const a = $("shelf-actions").children[tv.act].dataset.act;
      if (a === "play") openJourney(tv.trips[tv.focus]);
      else if (a === "photos") openPhotos(tv.trips[tv.focus]);
    }
  }
};

/* ---------- A: journey ---------- */

// A trip's stops in the order they happened: one per visit (app.js tripStops),
// each with the photos taken during that visit.
function buildStops(d) {
  const stops = [];
  for (const pin of d.pins) {
    const vs = pin.visits && pin.visits.length ? pin.visits
      : [{ at: pin.visited_at, until: pin.visited_at, whole: true }];
    for (const v of vs) stops.push({ pin, at: v.at, until: v.until, whole: !!v.whole, photos: [] });
  }
  stops.sort((a, b) => (a.at < b.at ? -1 : a.at > b.at ? 1 : a.pin.id - b.pin.id));
  const byPin = new Map();
  for (const s of stops) {
    if (!byPin.has(s.pin.id)) byPin.set(s.pin.id, []);
    byPin.get(s.pin.id).push(s);
  }
  const photos = [...d.photos].sort((a, b) => (a.taken_at < b.taken_at ? -1 : a.taken_at > b.taken_at ? 1 : 0));
  for (const ph of photos) {
    const mine = byPin.get(ph.pin_id);
    if (!mine) continue; // not on a pin the TV shows
    let home = mine.find((s) => s.whole || (ph.taken_at >= s.at && ph.taken_at <= s.until));
    if (!home) {
      // between visits: the visit that began last before it, else the first
      home = mine.filter((s) => s.at <= ph.taken_at).pop() || mine[0];
    }
    home.photos.push(ph);
  }
  return stops;
}

const jr = { trip: null, d: null, stops: [], slides: [], i: 0, paused: false, timer: 0, ended: false, lastStop: -1 };

async function openJourney(t) {
  if (!t) return;
  let d;
  try {
    d = await tripDetail(t.id);
  } catch (e) {
    return handleError(e);
  }
  show("journey");
  if (!journeyMap) journeyMap = makeMap("journey-map");
  journeyMap.invalidateSize();
  jr.trip = t;
  jr.d = d;
  jr.stops = buildStops(d);
  jr.slides = [];
  jr.stops.forEach((s, si) => {
    if (s.photos.length) s.photos.forEach((ph, pi) => jr.slides.push({ si, ph, pi }));
    else jr.slides.push({ si, ph: null, pi: 0 });
  });
  jr.i = 0;
  jr.paused = false;
  jr.ended = false;
  jr.lastStop = -1;
  // nothing of the last trip played may stay on screen (an empty trip draws no slide)
  for (const id of ["j-stop", "j-note", "j-count", "j-day", "j-date", "j-route"]) $(id).textContent = "";
  $("j-card").classList.add("nophoto");
  $("j-img").removeAttribute("src");
  $("journey").classList.add("card-only");
  $("j-trip").textContent = t.name || "";
  drawJourneyRoute();
  if (!jr.slides.length) { endJourney(); return; }
  drawSlide();
}

function stopJourney() {
  clearTimeout(jr.timer);
  jr.timer = 0;
}

function drawJourneyRoute() {
  if (journeyLayer) journeyLayer.remove();
  journeyLayer = L.layerGroup().addTo(journeyMap);
  jr.markers = [];
  const pts = jr.stops.map((s) => [s.pin.lat, s.pin.lng]);
  jr.doneLine = L.polyline([], { color: "#2a7f62", weight: 7 }).addTo(journeyLayer);
  jr.restLine = L.polyline(pts, { color: "#f6f1e3", weight: 5, dashArray: "2 12", lineCap: "round" }).addTo(journeyLayer);
  const seen = new Map();
  jr.stops.forEach((s, si) => {
    if (seen.has(s.pin.id)) return;
    const icon = L.divIcon({ className: "jpin" + (si === 0 ? " start" : ""), html: `<span>${si + 1}</span>`, iconSize: [40, 40] });
    const m = L.marker([s.pin.lat, s.pin.lng], { icon, keyboard: false, interactive: false }).addTo(journeyLayer);
    m.firstStop = si + 1;
    seen.set(s.pin.id, m);
  });
  jr.pinMarkers = seen;
  if (pts.length) journeyMap.fitBounds(pts, { paddingTopLeft: [90, 430], paddingBottomRight: [1150, 190], maxZoom: 13, animate: false });
}

function drawSlide() {
  stopJourney();
  $("j-end").hidden = true;
  jr.ended = false;
  const sl = jr.slides[jr.i];
  const stop = jr.stops[sl.si];
  const pin = stop.pin;
  const h = jr.d.holiday || jr.trip;
  $("j-stop").textContent = pin.title || (pin.kind === "photo" ? "Photo stop" : "Pin");
  // the note is about the place: shown at its first visit only, as the app does
  const firstVisit = jr.stops.findIndex((s) => s.pin === pin) === sl.si;
  $("j-note").textContent = firstVisit ? (pin.note || "") : "";
  const card = $("j-card");
  $("journey").classList.toggle("card-only", !sl.ph);
  card.classList.toggle("nophoto", !sl.ph);
  const day = tripDay(h, sl.ph ? sl.ph.taken_at : stop.at);
  $("j-day").textContent = "";
  $("j-day").append("Day ", el("span", "num", String(day.n)));
  $("j-date").textContent = "";
  $("j-date").append(...numbered(day.label));
  if (sl.ph) {
    const img = $("j-img");
    const fresh = img.cloneNode(false); // restarts the drift animation
    fresh.classList.remove("contain");
    fresh.onerror = () => { fresh.removeAttribute("src"); fresh.alt = "Photo not available"; };
    fresh.src = photoURL(sl.ph.asset_id, "preview");
    img.replaceWith(fresh);
    $("j-vmark").hidden = sl.ph.type !== "video";
    const n = stop.photos.length;
    $("j-count").textContent = n > 1 ? `${sl.pi + 1} of ${n} photos here` : "";
    preloadNext();
  }
  if (sl.si !== jr.lastStop) {
    moveToStop(sl.si);
    jr.lastStop = sl.si;
  }
  drawRouteBar(sl.si);
  $("j-paused").hidden = !jr.paused;
  if (!jr.paused) jr.timer = setTimeout(nextSlide, sl.ph ? PHOTO_MS : CARD_MS);
}

function preloadNext() {
  const nx = jr.slides[jr.i + 1];
  if (nx && nx.ph) { const im = new Image(); im.src = photoURL(nx.ph.asset_id, "preview"); }
}

function moveToStop(si) {
  const s = jr.stops[si];
  const pts = jr.stops.slice(0, si + 1).map((x) => [x.pin.lat, x.pin.lng]);
  jr.doneLine.setLatLngs(pts);
  const done = new Set(jr.stops.slice(0, si).map((x) => x.pin.id));
  for (const [id, m] of jr.pinMarkers) {
    const now = id === s.pin.id;
    const dot = m.getElement();
    dot.classList.toggle("now", now);
    dot.classList.toggle("done", !now && done.has(id));
    // a place visited twice shows the bar's number while it is the current stop
    dot.firstChild.textContent = String(now ? si + 1 : m.firstStop);
    m.setZIndexOffset(now ? 1000 : 0);
  }
}

function drawRouteBar(si) {
  const bar = $("j-route");
  bar.textContent = "";
  const n = jr.stops.length;
  if (n < 2) return;
  bar.appendChild(el("span", "rlabel", "Stops"));
  let from = Math.max(0, si - Math.floor(ROUTE_MAX / 2));
  const to = Math.min(n, from + ROUTE_MAX);
  from = Math.max(0, to - ROUTE_MAX);
  if (from > 0) bar.appendChild(el("span", "more", "…"));
  for (let i = from; i < to; i++) {
    if (i > from) bar.appendChild(el("span", "leg" + (i <= si ? " done" : "")));
    bar.appendChild(el("span", "s num" + (i < si ? " done" : i === si ? " now" : ""), String(i + 1)));
  }
  if (to < n) bar.appendChild(el("span", "more", "…"));
}

function nextSlide() {
  if (jr.i < jr.slides.length - 1) { jr.i++; drawSlide(); }
  else endJourney();
}

function endJourney() {
  stopJourney();
  jr.ended = true;
  const photos = jr.slides.filter((s) => s.ph).length;
  const n = jr.stops.length;
  $("j-end-sum").textContent = n
    ? `${n} ${n === 1 ? "stop" : "stops"}, ${photos} ${photos === 1 ? "photo" : "photos"}`
    : "This trip has no stops yet";
  $("j-end").hidden = false;
  $("j-paused").hidden = true;
}

function stopStart(si) {
  return jr.slides.findIndex((s) => s.si === si);
}

handlers.journey = (k) => {
  if (jr.ended) {
    if (k === "ok" || k === "playpause") {
      if (!jr.slides.length) return;
      jr.i = 0; jr.paused = false; jr.lastStop = -1; drawSlide();
    } else if (k === "left" && jr.slides.length) { jr.i = jr.slides.length - 1; jr.lastStop = -1; drawSlide(); }
    return;
  }
  const sl = jr.slides[jr.i];
  if (k === "ok" || k === "playpause") {
    jr.paused = !jr.paused;
    drawSlide();
  } else if (k === "right") {
    if (jr.i < jr.slides.length - 1) { jr.i++; drawSlide(); } else endJourney();
  } else if (k === "left") {
    if (jr.i > 0) { jr.i--; drawSlide(); }
  } else if (k === "down") {
    if (sl.si < jr.stops.length - 1) { jr.i = stopStart(sl.si + 1); drawSlide(); } else endJourney();
  } else if (k === "up") {
    // to the start of this stop, or the one before when already there
    const here = stopStart(sl.si);
    jr.i = jr.i > here || sl.si === 0 ? here : stopStart(sl.si - 1);
    drawSlide();
  }
};

/* ---------- all photos ---------- */

const ph = { trip: null, list: [], i: 0, cols: 6 };

async function openPhotos(t) {
  if (!t) return;
  let d;
  try {
    d = await tripDetail(t.id);
  } catch (e) {
    return handleError(e);
  }
  show("photos");
  ph.trip = t;
  ph.list = buildStops(d).flatMap((s) => s.photos.map((p) => ({ p, s })));
  ph.i = 0;
  $("p-title").textContent = `${t.name || "Trip"}: ${ph.list.length} ${ph.list.length === 1 ? "photo" : "photos"}`;
  const grid = $("p-grid");
  grid.textContent = "";
  ph.list.forEach(({ p }) => {
    const c = el("div", "cell");
    const img = el("img");
    img.loading = "lazy";
    img.alt = "";
    img.src = photoURL(p.asset_id, "thumb");
    c.appendChild(img);
    if (p.type === "video") c.appendChild(el("span", "vmark", "Video"));
    grid.appendChild(c);
  });
  $("p-viewer").hidden = true;
  drawPhotoFocus();
}

function drawPhotoFocus() {
  const cells = $("p-grid").children;
  [...cells].forEach((c, i) => c.classList.toggle("focus", i === ph.i));
  const cell = cells[ph.i];
  if (!cell) return;
  // scroll by whole rows: the focused row stays in the upper part of the screen
  const row = Math.floor(ph.i / ph.cols);
  const rowH = cell.offsetHeight + 24;
  const shift = Math.max(0, row - 1) * rowH;
  $("p-grid").style.transform = `translateY(${-shift}px)`;
}

function openViewer() {
  const it = ph.list[ph.i];
  if (!it) return;
  $("p-big").src = photoURL(it.p.asset_id, "preview");
  const day = tripDay((tv.details.get(ph.trip.id) || {}).holiday || ph.trip, it.p.taken_at);
  $("p-cap").textContent = `${it.s.pin.title || "Photo stop"}, Day ${day.n}, ${day.label}  ·  ${ph.i + 1} of ${ph.list.length}`;
  $("p-viewer").hidden = false;
}

function closeViewer() {
  $("p-viewer").hidden = true;
  $("p-big").removeAttribute("src");
  drawPhotoFocus();
}

handlers.photos = (k) => {
  const n = ph.list.length;
  if (!n) return;
  if (!$("p-viewer").hidden) {
    if (k === "left" && ph.i > 0) { ph.i--; openViewer(); }
    else if (k === "right" && ph.i < n - 1) { ph.i++; openViewer(); }
    else if (k === "ok") closeViewer();
    return;
  }
  const c = ph.cols;
  if (k === "left" && ph.i > 0) ph.i--;
  else if (k === "right" && ph.i < n - 1) ph.i++;
  else if (k === "up" && ph.i - c >= 0) ph.i -= c;
  else if (k === "down") ph.i = Math.min(n - 1, ph.i + c);
  else if (k === "ok") { openViewer(); return; }
  drawPhotoFocus();
};

/* ---------- heartbeat ---------- */

// A TV left on a journey or the shelf makes no calls by itself, so an unpair
// from the phone would go unseen: ask once a minute (it also keeps last_seen).
const HEARTBEAT_MS = 60000;
setInterval(async () => {
  if (!["shelf", "journey", "photos"].includes(tv.screen)) return;
  try {
    await api("/api/tv/me");
    if (tv.screen === "shelf") await refreshShelf();
  } catch (e) {
    if (e instanceof NotPaired && tv.screen !== "pair") startPair();
    // offline or a server error: keep showing what's on screen
  }
}, HEARTBEAT_MS);

/* ---------- for the TV app and the tests ---------- */

window.WSTV = {
  back,
  get screen() { return tv.screen; },
  get journey() { return { i: jr.i, slides: jr.slides.length, stop: jr.slides[jr.i] ? jr.slides[jr.i].si : -1, paused: jr.paused, ended: jr.ended }; },
  get shelf() { return { focus: tv.focus, row: tv.row, act: tv.act, trips: tv.trips.length }; },
  get photos() { return { i: ph.i, n: ph.list.length, viewer: !$("p-viewer").hidden }; },
};

start();
