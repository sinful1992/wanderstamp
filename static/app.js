/* Holiday Map frontend — vanilla JS + Leaflet. All user text goes through
   textContent (never innerHTML) so titles/notes can't inject markup. */

"use strict";

const SWATCHES = ["#1d6fb8", "#c2452d", "#2e8b57", "#b8860b", "#7b4b94", "#d81b60", "#00838f", "#5d4037"];

const state = {
  me: null,
  holidays: [],
  pins: [],
  hidden: new Set(),      // holiday ids toggled off
  markers: [],
  routes: [],             // one dotted itinerary line per visible trip
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
  const data = await resp.json().catch(() => ({}));
  if (!resp.ok) throw new Error(data.error || resp.statusText);
  return data;
}

/* ---------- map ---------- */

const map = L.map("map", { zoomControl: false, worldCopyJump: true });
L.control.zoom({ position: "bottomright" }).addTo(map);
// CARTO Voyager: warm muted colours + quiet labels, much closer to an atlas
// plate than stock OSM. Dark scheme swaps to CARTO's night plate (Dark
// Matter), matching the CSS token flip in style.css.
const darkScheme = matchMedia("(prefers-color-scheme: dark)");
const tileURL = () =>
  `https://{s}.basemaps.cartocdn.com/${darkScheme.matches ? "dark_all" : "rastertiles/voyager"}/{z}/{x}/{y}{r}.png`;
const tiles = L.tileLayer(tileURL(), {
  maxZoom: 20,
  subdomains: "abcd",
  attribution: '&copy; <a href="https://www.openstreetmap.org/copyright">OpenStreetMap</a> &copy; <a href="https://carto.com/attributions">CARTO</a>',
}).addTo(map);
darkScheme.addEventListener("change", () => tiles.setUrl(tileURL()));
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
  $("sheet").classList.add("collapsed"); // tap the atlas to tuck the trips sheet away
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
  if (pts.length) map.fitBounds(pts, { padding: [50, 50], maxZoom: 12 });
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
    if (pin.cover_asset) {
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

function renderMarkers() {
  for (const m of state.markers) m.remove();
  state.markers = [];
  for (const r of state.routes) r.remove();
  state.routes = [];
  const byTrip = new Map();
  for (const pin of visiblePins()) {
    const h = holidayById(pin.holiday_id);
    const color = h ? h.color : "#666";
    const marker = L.marker([pin.lat, pin.lng], { icon: pinIcon(pin, color), riseOnHover: true }).addTo(map);
    // Tapping a pin opens its trip's story at that chapter — the chapter
    // carries everything the old popup did (photos, note, edit, delete).
    marker.on("click", () => {
      const owner = holidayById(pin.holiday_id);
      if (owner) openStory(owner, pin.id);
    });
    marker.tripId = pin.holiday_id;
    marker.pinId = pin.id;
    state.markers.push(marker);
    if (!byTrip.has(pin.holiday_id)) byTrip.set(pin.holiday_id, []);
    byTrip.get(pin.holiday_id).push(pin);
  }
  // The itinerary line: a dotted ink route joining each trip's stops in the
  // order they were visited (visited_at = first photo's taken time, so pins
  // added after the fact still land in the right leg of the journey). Lines
  // render in Leaflet's overlay pane, under the marker pane — photo prints
  // always sit on top of the ink.
  for (const [hid, pins] of byTrip) {
    if (pins.length < 2) continue;
    const h = holidayById(hid);
    pins.sort(byVisit);
    const line = L.polyline(pins.map((p) => [p.lat, p.lng]), {
      color: h ? h.color : "#666",
      weight: 2.5, dashArray: "1 9", lineCap: "round", opacity: 0.8,
      interactive: false, // never steal taps from pins or the map
      className: "route-line",
    }).addTo(map);
    line.tripId = hid;
    state.routes.push(line);
  }
  applyFocus();
}

/* ---------- focus: spotlight one trip, fade the rest ---------- */

// Classes are toggled on the live elements rather than re-rendering, so
// pins keep their position and don't replay the pop-in animation.
function applyFocus() {
  const activePin = story.activeEl ? story.activeEl.dataset.pinId : null;
  for (const layer of [...state.markers, ...state.routes]) {
    const elm = layer.getElement && layer.getElement();
    if (!elm) continue;
    elm.classList.toggle("dimmed", state.focused !== null && layer.tripId !== state.focused);
    elm.classList.toggle("story-active", activePin !== null && String(layer.pinId) === activePin);
  }
}

function setFocus(id) {
  state.focused = id;
  applyFocus();
  renderSheet();
}

/* ---------- trip story: scroll the chapters, the map follows ---------- */

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
    photos.forEach((ph, i) => {
      const img = el("img");
      img.loading = "lazy";
      img.src = photoURL(ph.asset_id, "thumb");
      img.alt = "";
      img.onclick = (e) => { e.stopPropagation(); openLightbox(photos, i); };
      grid.appendChild(img);
    });
  }).catch(() => { grid.textContent = ""; grid.appendChild(el("span", "pop-sub", "Couldn't load photos")); });
}

function openStory(h, pinId) {
  closeStory();
  story.hid = h.id;
  state.focused = h.id;
  document.body.classList.add("storying");
  $("sheet").classList.add("collapsed");
  $("story-title").textContent = h.name;
  const scroll = $("story-scroll");
  scroll.textContent = "";
  scroll.scrollTop = 0;

  const pins = state.pins.filter((p) => p.holiday_id === h.id).sort(byVisit);
  const secs = [];
  for (const pin of pins) {
    const sec = el("section", "story-sec");
    sec.dataset.lat = pin.lat;
    sec.dataset.lng = pin.lng;
    sec.dataset.pinId = pin.id;
    const dayN = Math.max(1, Math.floor((new Date(pin.visited_at) - new Date(h.start_at)) / 86400000) + 1);
    sec.appendChild(el("p", "story-day", `Day ${dayN} · ${fmtDate(pin.visited_at)}`));
    sec.appendChild(el("h3", "story-place", pin.title || (pin.kind === "photo" ? "Photo stop" : "Pin")));
    if (pin.note) sec.appendChild(el("p", "story-note", pin.note));
    if (pin.photo_count > 0) {
      const grid = el("div", "story-grid");
      grid.dataset.pin = pin.id;
      // Placeholder cells reserve the grid's final height before the photos
      // arrive — chapters must not grow later, or the open-at-pin scroll (and
      // the reader's place) slides as content above them expands.
      for (let i = 0; i < pin.photo_count; i++) grid.appendChild(el("span", "ph"));
      sec.appendChild(grid);
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
  const target = pinId != null && secs.find((s) => s.dataset.pinId === String(pinId));
  if (target) {
    // Jump straight to the tapped pin's chapter. The scroll this causes must
    // not re-derive the active chapter (a bottom-clamped scroll would pick a
    // later one), so the sync handler holds off briefly.
    story.holdUntil = performance.now() + 600;
    scroll.scrollTop = Math.max(0, target.offsetTop - scroll.offsetTop - 8);
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
  const activePin = story.activeEl ? story.activeEl.dataset.pinId : null;
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
    const opt = el("option", null, h.active ? h.name + " (now)" : h.name);
    opt.value = h.id;
    trip.appendChild(opt);
  }
  trip.value = String((active || state.holidays[0]).id);
  const save = el("button", "primary", "Add pin");
  save.type = "submit";
  form.append(title, note, trip, save);
  const popup = L.popup({ maxWidth: 300, minWidth: 220 }).setLatLng(latlng).setContent(form).openOn(map);
  title.focus();
  form.onsubmit = async (e) => {
    e.preventDefault();
    await api("POST", "/api/pins", {
      holiday_id: Number(trip.value),
      lat: latlng.lat, lng: latlng.lng,
      title: title.value, note: note.value,
    }).then(() => {
      map.closePopup(popup);
      loadData();
      toast("Pin added");
    }).catch((err) => toast(err.message));
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
    list.appendChild(el("p", "empty-note", "No trips yet — start your first holiday below."));
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
    if (h.cover_asset) {
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
    const range = h.active ? `since ${fmtDate(h.start_at)}` : `${fmtDate(h.start_at)} – ${fmtDate(h.end_at)}`;
    const sub = el("div", "trip-sub", `${range} · ${h.pin_count} pins · ${h.photo_count} photos`);
    if (h.pin_count > 0 || h.unplaced_count > 0) {
      const st = el("button", "sub-link", "story");
      st.onclick = (e) => { e.stopPropagation(); openStory(h); };
      sub.append(" · ", st);
    }
    if (h.unplaced_count > 0) {
      const un = el("button", "sub-link", `${h.unplaced_count} without location`);
      un.onclick = (e) => { e.stopPropagation(); openUnplaced(h); };
      sub.append(" · ", un);
    }
    if (h.journal) {
      const jr = el("button", "sub-link", "journal");
      jr.onclick = (e) => { e.stopPropagation(); openJournal(h); };
      sub.append(" · ", jr);
    }
    const mfLeft = (h.pack_total || 0) - (h.pack_done || 0);
    const mf = el("button", "sub-link",
      !h.pack_total ? "manifest" : mfLeft ? `manifest · ${mfLeft} to pack` : "manifest ✓");
    mf.onclick = (e) => { e.stopPropagation(); openManifest(h); };
    sub.append(" · ", mf);
    info.append(name, sub);
    const edit = el("button", "trip-eye", "✎");
    edit.title = "Edit this trip";
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
    eye.title = "Show or hide this trip's pins";
    eye.onclick = (e) => {
      e.stopPropagation();
      state.hidden.has(h.id) ? state.hidden.delete(h.id) : state.hidden.add(h.id);
      if (state.hidden.has(h.id) && state.focused === h.id) state.focused = null;
      renderMarkers();
      renderSheet();
    };
    row.append(cover, info, edit, eye);
    row.onclick = () => {
      // First tap spotlights the trip (others fade) and flies to it;
      // tapping the spotlit row again releases the focus.
      if (state.focused === h.id) {
        setFocus(null);
        return;
      }
      const pts = state.pins.filter((p) => p.holiday_id === h.id).map((p) => [p.lat, p.lng]);
      if (pts.length) {
        setFocus(h.id);
        map.fitBounds(pts, { padding: [50, 50], maxZoom: 13 });
        $("sheet").classList.add("collapsed");
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
  wrap.append(name, sw, startRow, h.active ? el("span") : endRow);
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
  wrap.append(journal, shareRow, btnRow);
  wrap.onsubmit = async (e) => {
    e.preventDefault();
    const body = { name: name.value, color, start_at: start.value, journal: journal.value };
    if (cover && cover !== h.cover_asset) body.cover_asset = cover;
    if (!h.active && end.value) body.end_at = end.value;
    try {
      await api("PATCH", `/api/holidays/${h.id}`, body);
      toast("Trip saved");
      const dates = (!h.active && end.value !== (h.end_at || "").slice(0, 10)) || start.value !== h.start_at.slice(0, 10);
      loadData();
      if (dates) api("POST", `/api/holidays/${h.id}/sync`).then(() => loadData()).catch(() => {});
    } catch (err) {
      toast(err.message);
    }
  };
  return wrap;
}

/* ---------- overlays: passport, unplaced, journal ---------- */

function openOverlay(title, contentNode) {
  $("overlay-title").textContent = title;
  const c = $("overlay-content");
  c.textContent = "";
  c.appendChild(contentNode);
  $("overlay").hidden = false;
}
$("overlay-close").onclick = () => { $("overlay").hidden = true; };

$("btn-passport").onclick = async () => {
  try {
    const stamps = await api("GET", "/api/stamps");
    const box = el("div");

    // the data page: a life of travel in four numbers
    const stats = el("div", "passport-stats");
    const days = state.holidays.reduce((sum, h) => {
      const end = h.end_at ? new Date(h.end_at) : new Date();
      return sum + Math.max(1, Math.round((end - new Date(h.start_at)) / 86400000) + 1);
    }, 0);
    for (const [n, label] of [
      [new Set(stamps.map((s) => s.country)).size, "countries"],
      [state.holidays.length, "trips"],
      [days, "days away"],
      [state.holidays.reduce((s, h) => s + h.pin_count, 0), "places"],
    ]) {
      const t = el("div", "stat");
      t.appendChild(el("div", "stat-n", String(n)));
      t.appendChild(el("div", "stat-label", label));
      stats.appendChild(t);
    }
    box.appendChild(stats);

    const wrap = el("div", "stamp-grid");
    if (!stamps.length) {
      wrap.appendChild(el("p", "empty-note", "No stamps yet — countries appear here once trips have photos."));
    }
    stamps.forEach((s, i) => {
      const card = el("div", "stamp-card");
      card.style.setProperty("--c", s.color);
      card.style.setProperty("--r", ((i % 5) - 2) * 1.6 + "deg");
      card.style.setProperty("--i", i % 12);
      card.appendChild(el("div", "stamp-country", s.country));
      card.appendChild(el("div", "stamp-trip", s.name));
      card.appendChild(el("div", "stamp-admit", "Admitted · " + fmtDate(s.start_at)));
      card.title = "Open this trip's story";
      card.onclick = () => {
        const h = holidayById(s.holiday_id);
        if (!h) return;
        $("overlay").hidden = true;
        openStory(h);
      };
      wrap.appendChild(card);
    });
    box.appendChild(wrap);
    openOverlay("Passport", box);
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
        $("overlay").hidden = true;
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
  const range = h.active ? `since ${fmtDate(h.start_at)}` : `${fmtDate(h.start_at)} – ${fmtDate(h.end_at)}`;
  box.appendChild(el("p", "pop-sub", range));
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
      star.title = "Always pack this — add it to a master list";
      star.onclick = (e) => {
        e.stopPropagation();
        if (!templates.length) { toast("No master lists yet — Manifest in the trips sheet creates them"); return; }
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
        list.appendChild(el("p", "empty-note", "An empty manifest — add items below, or bring in a master list."));
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
      const ph = el("option", null, "Bring in a master list…");
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

    const editLists = el("button", "linkish", "Edit master lists");
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
      "Master lists hold the things you always pack. Bring one onto a trip from its manifest, and tick items off there."));

    if (!templates.length) {
      box.appendChild(el("p", "empty-note", "No master lists yet."));
      const starter = el("button", "primary", "Create starter lists — caravan · beach · city break");
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
        if (!confirm(`Delete the "${t.name}" master list? Trips keep their own copies.`)) return;
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
      inp.type = "text"; inp.placeholder = "New master list — e.g. Ski"; inp.maxLength = 80; inp.required = true;
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

    openOverlay("Manifest — master lists", box);
  } catch (err) {
    toast(err.message);
  }
}

/* ---------- banner ---------- */

function renderBanner() {
  const banner = $("banner");
  const active = state.holidays.find((h) => h.active);
  if (!active) { banner.hidden = true; return; }
  banner.hidden = false;
  banner.style.setProperty("--c", active.color);
  $("banner-name").textContent = active.name;
  const day = Math.floor((Date.now() - new Date(active.start_at)) / 86400000) + 1;
  $("banner-day").textContent = `day ${day}`;
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

$("btn-new-trip").onclick = () => {
  buildSwatches();
  $("trip-start").value = new Date().toISOString().slice(0, 10);
  $("new-trip-form").hidden = false;
  $("btn-new-trip").hidden = true;
  $("trip-name").focus();
};
$("btn-cancel-trip").onclick = () => {
  $("new-trip-form").hidden = true;
  $("btn-new-trip").hidden = false;
};
$("new-trip-form").onsubmit = async (e) => {
  e.preventDefault();
  try {
    const created = await api("POST", "/api/holidays", {
      name: $("trip-name").value.trim(),
      color: chosenColor,
      start_at: $("trip-start").value,
      end_at: $("trip-end").value,
    });
    $("new-trip-form").hidden = true;
    $("btn-new-trip").hidden = false;
    $("trip-name").value = "";
    $("trip-end").value = "";
    toast(created.active ? "Holiday started — pins and photos now attach to it" : "Past trip added — pulling its photos…");
    await loadData();
    api("POST", `/api/holidays/${created.id}/sync`)
      .then((r) => { toast(`Found ${r.photos} photos across ${r.photo_pins} places`); loadData(); })
      .catch(() => {});
  } catch (err) {
    toast(err.message);
  }
};

/* ---------- sheet + admin + logout ---------- */

$("sheet-pill").onclick = () => $("sheet").classList.toggle("collapsed");

$("btn-manifest").onclick = () => openMasterLists();

$("new-user-form").onsubmit = async (e) => {
  e.preventDefault();
  try {
    await api("POST", "/api/users", { username: $("user-name").value.trim(), password: $("user-pass").value });
    $("user-name").value = ""; $("user-pass").value = "";
    toast("Account created");
  } catch (err) {
    toast(err.message);
  }
};

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
  $("lightbox").hidden = false;
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

$("lb-close").onclick = () => { $("lightbox").hidden = true; $("lb-img").src = ""; };
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
    await api("POST", "/api/login", { username: $("login-user").value.trim(), password: $("login-pass").value });
    location.reload();
  } catch (ex) {
    err.textContent = ex.message === "invalid credentials" ? "Wrong username or password." : ex.message;
    err.hidden = false;
  }
};

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
  } catch {
    return; // login overlay already shown
  }
  $("admin-box").hidden = !state.me.is_admin;
  $("app-version").textContent = state.me.version === "dev" ? "dev build" : state.me.version;
  try {
    await loadData(true);
  } catch (ex) {
    toast("Couldn't load the map data: " + ex.message);
  }
})();
