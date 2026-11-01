// F4 travel stats: per year, the longest trips, and every country with its
// first visit. app.js calls WSStats.open() from the passport.
(function () {
  "use strict";
  const style = document.createElement("style");
  style.textContent = `
    .ws-stats h3 { margin: 20px 0 8px; font-size: 1rem; }
    .ws-stats-totals { display: grid; grid-template-columns: repeat(auto-fit, minmax(90px, 1fr)); gap: 8px; }
    .ws-stats-total { background: var(--paper-2); border-radius: 8px; padding: 10px; text-align: center; }
    .ws-stats-total b { display: block; font-size: 1.5rem; font-variant-numeric: tabular-nums; }
    .ws-stats-total span { color: var(--muted); font-size: .85rem; }
    .ws-bars { list-style: none; padding: 0; margin: 0; }
    .ws-bars li { display: grid; grid-template-columns: minmax(4.5em, 30%) 1fr auto; gap: 8px;
      align-items: center; min-height: 32px; }
    .ws-bars .lbl { overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
    .ws-bars .bar { height: 10px; border-radius: 5px; background: var(--navy-ink); min-width: 2px; }
    .ws-bars .val { color: var(--muted); font-variant-numeric: tabular-nums; font-size: .9rem; }
    .ws-countries { list-style: none; padding: 0; margin: 0; }
    .ws-countries li { display: flex; justify-content: space-between; gap: 8px; padding: 8px 0;
      border-bottom: 1px solid var(--line); }
    .ws-countries small { color: var(--muted); }
    .ws-stats .empty-note { color: var(--muted); }
  `;
  document.head.appendChild(style);

  const km = (n) => `${Math.round(n).toLocaleString("en-GB")} km`;
  const plural = (n, one, many = one + "s") => `${n} ${n === 1 ? one : many}`;
  const monthYear = (iso) => new Date(iso).toLocaleDateString("en-GB", { month: "short", year: "numeric", timeZone: "UTC" });

  function total(ws, n, label) {
    const d = ws.el("div", "ws-stats-total");
    d.append(ws.el("b", "", n), ws.el("span", "", label));
    return d;
  }

  // one labelled bar per row, scaled to the largest value
  function bars(ws, rows) {
    const ul = ws.el("ul", "ws-bars");
    const max = Math.max(1, ...rows.map((r) => r.value));
    for (const r of rows) {
      const li = ws.el("li");
      const bar = ws.el("div", "bar");
      bar.style.width = `${(r.value / max) * 100}%`;
      bar.setAttribute("aria-hidden", "true");
      const lbl = ws.el("span", "lbl", r.label);
      lbl.title = r.label;
      li.append(lbl, bar, ws.el("span", "val", r.text));
      ul.appendChild(li);
    }
    return ul;
  }

  async function open() {
    const ws = window.WS;
    const body = ws.openPanel("Travel stats");
    body.classList.add("ws-stats");
    body.appendChild(ws.el("p", "empty-note", "Counting…"));
    let s;
    try {
      s = await ws.api("GET", "/api/stats");
    } catch (ex) {
      body.textContent = "";
      body.appendChild(ws.el("p", "empty-note", `Couldn't load the stats: ${ex.message}`));
      return;
    }
    body.textContent = "";
    const years = s.years || [], trips = s.trips || [], countries = s.countries || [];
    if (!trips.length) {
      body.appendChild(ws.el("p", "empty-note", "No trips yet. The stats fill in after your first holiday."));
      return;
    }
    const sum = (k) => years.reduce((a, y) => a + (y[k] || 0), 0);
    const totals = ws.el("div", "ws-stats-totals");
    totals.append(
      total(ws, String(sum("trips")), sum("trips") === 1 ? "trip" : "trips"),
      total(ws, String(sum("days")), sum("days") === 1 ? "day away" : "days away"),
      total(ws, Math.round(sum("km")).toLocaleString("en-GB"), "km between stops"),
      total(ws, String(countries.length), countries.length === 1 ? "country" : "countries"),
    );
    body.appendChild(totals);

    if (years.length) {
      body.appendChild(ws.el("h3", "", "By year"));
      body.appendChild(bars(ws, [...years].sort((a, b) => b.year - a.year).map((y) => ({
        label: String(y.year), value: y.km,
        text: `${plural(y.trips, "trip")} · ${plural(y.days, "day")} · ${km(y.km)}`,
      }))));
    }

    const far = [...trips].filter((t) => t.km > 0).sort((a, b) => b.km - a.km).slice(0, 10);
    if (far.length) {
      body.appendChild(ws.el("h3", "", "Furthest trips"));
      body.appendChild(bars(ws, far.map((t) => ({ label: `${t.name} (${t.year})`, value: t.km, text: km(t.km) }))));
    }

    if (countries.length) {
      body.appendChild(ws.el("h3", "", "Countries"));
      const ul = ws.el("ul", "ws-countries");
      for (const c of [...countries].sort((a, b) => b.visits - a.visits || a.country.localeCompare(b.country))) {
        const li = ws.el("li");
        const first = trips.find((t) => t.id === c.first_trip_id);
        const left = ws.el("span", "", c.country);
        const right = ws.el("small", "", `${plural(c.visits, "visit")} · first ${monthYear(c.first_at)}${first ? ", " + first.name : ""}`);
        li.append(left, right);
        ul.appendChild(li);
      }
      body.appendChild(ul);
    }
    body.appendChild(ws.el("p", "empty-note", "Distances are straight lines between stops, in the order you visited them."));
  }

  window.WSStats = { open };
})();
