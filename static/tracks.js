// F10 GPX tracks: the route walked or driven, drawn under a trip's pins.
// app.js calls WSTracks.draw(holidayId) when a story opens or refreshes, and
// draw(null) when it closes; the lines and the "Routes" button go with it.
// A refresh of the trip already drawn costs nothing. Never on a
// share link: tracks start at home.
(function () {
  "use strict";
  if (location.pathname.startsWith("/share/")) {
    window.WSTracks = { draw() {}, clear() {} };
    return;
  }
  const WS = () => window.WS;
  let layer = null;   // L.layerGroup of the open trip's tracks
  let control = null; // the "Routes" button on the map
  let current = null; // holiday id drawn
  let seq = 0;        // drops a slow answer for a trip no longer open

  const style = document.createElement("style");
  style.textContent = `
    .ws-routes-btn { min-width: 44px; min-height: 44px; padding: 0 12px; border: 0; border-radius: 8px;
      background: var(--paper); color: var(--navy-ink); font: inherit; box-shadow: var(--shadow); cursor: pointer; }
    .ws-track-list { list-style: none; padding: 0; margin: 0 0 16px; }
    .ws-track-list li { display: flex; align-items: center; gap: 8px; min-height: 44px;
      border-bottom: 1px solid var(--line); }
    .ws-track-list li span { flex: 1; overflow-wrap: anywhere; }
    .ws-track-list button, .ws-track-up { min-height: 44px; }
    .ws-track-note { color: var(--muted); }
  `;
  document.head.appendChild(style);

  function color(id) {
    const h = (WS().state.holidays || []).find((x) => x.id === id);
    return h && h.color ? h.color : "#1c3652";
  }

  function clear() {
    seq++;
    current = null;
    if (layer) { layer.remove(); layer = null; }
    if (control) { control.remove(); control = null; }
  }

  async function draw(holidayId, force) {
    if (holidayId == null) { clear(); return; }
    if (!force && holidayId === current && control) return; // a story refresh: already drawn or loading
    clear();
    if (!window.L || !WS() || !WS().map) return;
    current = holidayId;
    const mine = seq;
    addControl(holidayId);
    let tracks;
    try {
      tracks = await WS().api("GET", `/api/holidays/${holidayId}/tracks`);
    } catch {
      // offline or gone: the story works without its routes, and the next
      // draw of this trip tries again instead of thinking it's drawn
      if (mine === seq) current = null;
      return;
    }
    if (mine !== seq) return;
    layer = L.layerGroup();
    const c = color(holidayId);
    for (const t of tracks) {
      if (!t.points || t.points.length < 2) continue;
      // a pale casing under the line keeps it readable on any map tile
      L.polyline(t.points, { color: "#fff", weight: 7, opacity: 0.7, interactive: false }).addTo(layer);
      L.polyline(t.points, { color: c, weight: 4, opacity: 0.9, dashArray: "1 7", lineCap: "round" })
        // a node, not a string: Leaflet puts a string in as HTML, and the
        // name comes from whoever made the GPX file
        .bindTooltip(WS().el("span", "", t.name || "Route"), { sticky: true }).addTo(layer);
    }
    layer.addTo(WS().map);
    // keep the lines under the pins
    layer.eachLayer((l) => l.bringToBack && l.bringToBack());
  }

  function addControl(holidayId) {
    const Ctl = L.Control.extend({
      options: { position: "topright" },
      onAdd() {
        const b = L.DomUtil.create("button", "ws-routes-btn");
        b.type = "button";
        b.textContent = "Routes";
        b.setAttribute("aria-label", "Routes (GPX tracks) for this trip");
        L.DomEvent.disableClickPropagation(b);
        b.onclick = () => manage(holidayId);
        return b;
      },
    });
    control = new Ctl().addTo(WS().map);
  }

  async function manage(holidayId) {
    const ws = WS();
    const body = ws.openPanel("Routes");
    body.appendChild(ws.el("p", "ws-track-note",
      "Add a GPX file from a running, walking or sat-nav app. It shows on this trip's map, never on a share link."));
    const list = ws.el("ul", "ws-track-list");
    body.appendChild(list);

    const refresh = async () => {
      list.textContent = "";
      let tracks = [];
      try { tracks = await ws.api("GET", `/api/holidays/${holidayId}/tracks`); }
      catch (ex) { list.appendChild(ws.el("li", "ws-track-note", ex.message)); return; }
      if (!tracks.length) list.appendChild(ws.el("li", "ws-track-note", "No routes yet"));
      for (const t of tracks) {
        const li = ws.el("li");
        li.appendChild(ws.el("span", "", t.name || "Route"));
        const del = ws.el("button", "", "Delete");
        del.type = "button";
        del.setAttribute("aria-label", `Delete route ${t.name || ""}`.trim());
        del.onclick = async () => {
          if (!confirm(`Delete the route "${t.name || "Route"}"?`)) return;
          del.disabled = true;
          try {
            await ws.api("DELETE", `/api/tracks/${t.id}`);
            ws.toast("Route deleted");
            await refresh();
            if (current === holidayId) draw(holidayId, true);
          } catch (ex) { del.disabled = false; ws.toast(`Couldn't delete: ${ex.message}`); }
        };
        li.appendChild(del);
        list.appendChild(li);
      }
    };

    const input = document.createElement("input");
    input.type = "file";
    input.accept = ".gpx,application/gpx+xml";
    input.hidden = true;
    const up = ws.el("button", "primary ws-track-up", "Add a GPX file");
    up.type = "button";
    up.onclick = () => input.click();
    input.onchange = async () => {
      const f = input.files && input.files[0];
      input.value = "";
      if (!f) return;
      if (f.size > 10 * 1024 * 1024) { ws.toast("That file is over 10 MB"); return; }
      up.disabled = true;
      up.textContent = "Adding…";
      try {
        const fd = new FormData();
        fd.append("file", f);
        // multipart, so not through WS.api (that sends JSON)
        const r = await fetch(`/api/holidays/${holidayId}/tracks`, { method: "POST", body: fd, credentials: "same-origin" });
        const d = await r.json().catch(() => ({}));
        if (!r.ok) throw new Error(d.error || r.statusText);
        ws.toast(`Added ${d.name || "route"} (${d.points} points)`);
        await refresh();
        if (current === holidayId) draw(holidayId, true);
      } catch (ex) {
        ws.toast(`Couldn't add it: ${ex.message}`);
      } finally {
        up.disabled = false;
        up.textContent = "Add a GPX file";
      }
    };
    body.append(up, input);
    refresh();
  }

  // the story closing takes its routes with it
  const story = document.getElementById("story");
  if (story) {
    new MutationObserver(() => { if (story.hidden) clear(); })
      .observe(story, { attributes: true, attributeFilter: ["hidden"] });
  }

  window.WSTracks = { draw: (id) => draw(id, false), clear };
})();
