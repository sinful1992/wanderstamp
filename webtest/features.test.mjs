// v1.14.0 features, checked in the real app.
import assert from "node:assert/strict";
import { signedIn, api } from "./harness.mjs";

async function trips(page, list) {
  const have = (await api(page, "GET", "/api/holidays")).body;
  const out = {};
  for (const t of list) {
    let h = have.find((x) => x.name === t.name);
    if (!h) {
      const r = await api(page, "POST", "/api/holidays", { color: "#1d6fb8", ...t });
      assert.ok(r.status < 300, JSON.stringify(r));
      h = r.body;
    }
    out[t.name] = h;
  }
  return out;
}

const F1 = [
  { name: "F1 Côte d'Azur", start_at: "2023-07-01", end_at: "2023-07-09" },
  { name: "F1 Lisbon", start_at: "2024-04-02", end_at: "2024-04-06" },
  { name: "F1 New Year Rome", start_at: "2024-12-30", end_at: "2025-01-02" },
  { name: "F1 Bath", start_at: "2025-06-10", end_at: "2025-06-12" },
  { name: "F1 Skye", start_at: "2025-08-01", end_at: "2025-08-08" },
];

const rowNames = (page) => page.$$eval("#trip-list .trip-row .trip-name", (ns) => ns.map((n) => n.textContent.trim()));

export const tests = [
  ["F1: the search finds a trip without its accents, and 'No trips match' clears", async ({ browser, base }) => {
    const { ctx, page } = await signedIn(browser, base);
    await trips(page, F1);
    await page.reload({ waitUntil: "networkidle" });
    await page.evaluate(() => setSheet(true));
    assert.equal(await page.isVisible("#trip-search"), true);
    await page.fill("#trip-search", "cote");
    assert.deepEqual(await rowNames(page), ["F1 Côte d'Azur"]);
    await page.fill("#trip-search", "rome new");
    assert.deepEqual(await rowNames(page), ["F1 New Year Rome"]);
    await page.fill("#trip-search", "zzzz");
    assert.deepEqual(await rowNames(page), []);
    assert.ok((await page.textContent("#trip-list")).includes("No trips match"));
    await page.click("#trip-list .sheet-none button");
    assert.equal(await page.inputValue("#trip-search"), "");
    assert.ok((await rowNames(page)).length >= 5);
    // a background reload keeps the search
    await page.fill("#trip-search", "lisbon");
    await page.evaluate(() => loadData());
    assert.deepEqual(await rowNames(page), ["F1 Lisbon"]);
    await ctx.close();
  }],
  ["F1: a year chip shows the trips that touch that year, a New Year trip in both", async ({ browser, base }) => {
    const { ctx, page } = await signedIn(browser, base);
    await trips(page, F1);
    await page.reload({ waitUntil: "networkidle" });
    await page.evaluate(() => setSheet(true));
    const chips = await page.$$eval("#year-chips .chip", (cs) => cs.map((c) => c.textContent));
    assert.equal(chips[0], "All");
    for (const y of ["2025", "2024", "2023"]) assert.ok(chips.includes(y), chips.join(","));
    const pick = async (y) => {
      await page.click(`#year-chips .chip:text-is("${y}")`);
      return (await rowNames(page)).filter((n) => n.startsWith("F1 "));
    };
    assert.deepEqual((await pick("2025")).sort(), ["F1 Bath", "F1 New Year Rome", "F1 Skye"]);
    assert.deepEqual((await pick("2024")).sort(), ["F1 Lisbon", "F1 New Year Rome"]);
    assert.equal(await page.getAttribute(`#year-chips .chip:text-is("2024")`, "aria-pressed"), "true");
    // year and search together
    await page.fill("#trip-search", "rome");
    assert.deepEqual(await rowNames(page), ["F1 New Year Rome"]);
    await page.fill("#trip-search", "");
    assert.equal((await pick("All")).length, 5);
    await ctx.close();
  }],
  ["F1: a hidden trip stays hidden after a reload, and only for that user", async ({ browser, base }) => {
    const { ctx, page } = await signedIn(browser, base);
    const t = await trips(page, F1);
    await api(page, "POST", "/api/pins", { holiday_id: t["F1 Bath"].id, lat: 51.38, lng: -2.36, title: "F1 Bath pin", note: "" });
    await page.reload({ waitUntil: "networkidle" });
    await page.evaluate(() => setSheet(true));
    const eye = (page) => page.evaluateHandle(() =>
      [...document.querySelectorAll("#trip-list .trip-row")].find((r) => r.textContent.includes("F1 Bath")).querySelector("button[title^='Show or hide']"));
    await (await eye(page)).click();
    const visible = () => page.evaluate(() => visiblePins().some((p) => p.title === "F1 Bath pin"));
    assert.equal(await visible(), false);
    await page.reload({ waitUntil: "networkidle" });
    assert.equal(await visible(), false, "hidden after reload");
    assert.equal(await page.evaluate((id) => state.hidden.has(id), t["F1 Bath"].id), true);
    // another user on the same browser starts with nothing hidden
    const other = await page.evaluate(() => localStorage.getItem("hm-hidden:someone-else"));
    assert.equal(other, null);
    // and showing it again is remembered too
    await page.evaluate(() => setSheet(true));
    await (await eye(page)).click();
    await page.reload({ waitUntil: "networkidle" });
    assert.equal(await visible(), true);
    await ctx.close();
  }],
  ["F1: a short list has no search box, and junk in storage doesn't break the map", async ({ browser, base }) => {
    const { ctx, page } = await signedIn(browser, base);
    await page.evaluate(() => {
      localStorage.setItem(`hm-hidden:${state.me.username}`, "{not json");
    });
    await page.reload({ waitUntil: "networkidle" });
    assert.equal(await page.evaluate(() => state.hidden.size), 0);
    await page.evaluate(() => setSheet(true));
    const n = await page.evaluate(() => state.holidays.length);
    assert.equal(await page.isHidden("#trip-find"), n < 5);
    await page.evaluate(() => { state.holidays = state.holidays.slice(0, 2); renderSheet(); });
    assert.equal(await page.isHidden("#trip-find"), true);
    await ctx.close();
  }],
  // F2 server (linus) isn't on this branch yet: its contract answers are stubbed here.
  ["F2: deleting a trip offers Undo, and Undo restores it", async ({ browser, base }) => {
    const { ctx, page } = await signedIn(browser, base);
    const t = await trips(page, [{ name: "F2 Undo me", start_at: "2022-05-01", end_at: "2022-05-03" }]);
    const id = t["F2 Undo me"].id;
    let undone = 0;
    await page.route(`**/api/holidays/${id}`, (r) => r.request().method() === "DELETE"
      ? r.fulfill({ status: 200, contentType: "application/json", body: '{"ok":true,"undo":"tok-1"}' }) : r.continue());
    await page.route("**/api/undo/tok-1", (r) => { undone++; r.fulfill({ status: 200, contentType: "application/json", body: '{"ok":true}' }); });
    page.on("dialog", (d) => d.accept());
    await page.reload({ waitUntil: "networkidle" });
    await page.evaluate(() => setSheet(true));
    await page.evaluate(() => [...document.querySelectorAll("#trip-list .trip-row")].find((r) => r.textContent.includes("F2 Undo me"))
      .querySelector("button[title='Edit this trip']").click());
    await page.click("form.trip-edit button.danger");
    await page.waitForSelector("#toast .toast-act");
    assert.ok((await page.textContent("#toast")).startsWith("Trip deleted"));
    await page.click("#toast .toast-act");
    await page.waitForFunction(() => document.getElementById("toast").textContent.includes("is back"));
    assert.equal(undone, 1);
    await ctx.close();
  }],
  ["F2: Undo after the 30 s says it's too late; a delete without a token has no Undo", async ({ browser, base }) => {
    const { ctx, page } = await signedIn(browser, base);
    await page.route("**/api/undo/old", (r) => r.fulfill({ status: 410, contentType: "application/json", body: '{"error":"too late to undo"}' }));
    await page.evaluate(() => deletedToast("Pin", { ok: true, undo: "old" }));
    await page.click("#toast .toast-act");
    await page.waitForFunction(() => document.getElementById("toast").textContent === "Too late to undo");
    await page.evaluate(() => deletedToast("Pin", { ok: true }));
    assert.equal(await page.$("#toast .toast-act"), null);
    assert.equal(await page.textContent("#toast"), "Pin deleted");
    await ctx.close();
  }],
  ["F2: an ended trip has Reopen trip; it posts reopen, and a live trip blocks it", async ({ browser, base }) => {
    const { ctx, page } = await signedIn(browser, base);
    const t = await trips(page, [{ name: "F2 Reopen", start_at: "2022-09-01", end_at: "2022-09-04" }]);
    const id = t["F2 Reopen"].id;
    // earlier tests may have left a trip live: end it, so only one is live at a time
    for (const h of (await api(page, "GET", "/api/holidays")).body.filter((x) => x.active)) {
      await api(page, "POST", `/api/holidays/${h.id}/end`);
    }
    let posted = 0;
    await page.route(`**/api/holidays/${id}/reopen`, (r) => { posted++; r.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ ...t["F2 Reopen"], end_at: null, active: true }) }); });
    page.on("dialog", (d) => d.accept());
    await page.reload({ waitUntil: "networkidle" });
    // opens the trip's edit form, or leaves it open: the edit button toggles it
    const open = async (name) => {
      await page.evaluate(() => setSheet(true));
      await page.evaluate((n) => {
        const row = [...document.querySelectorAll("#trip-list .trip-row")].find((r) => r.textContent.includes(n));
        const f = row.nextElementSibling;
        if (!(f && f.matches("form.trip-edit"))) row.querySelector("button[title='Edit this trip']").click();
      }, name);
      await page.waitForSelector("form.trip-edit");
    };
    await open("F2 Reopen");
    const reopen = page.locator("form.trip-edit button", { hasText: "Reopen trip" });
    await reopen.click();
    await page.waitForFunction(() => document.getElementById("toast").textContent.includes("is live again"));
    assert.equal(posted, 1);
    // with another trip live, it says so instead of asking the server
    // (set and clicked in one go: a background reload would reset state.holidays)
    await open("F2 Reopen");
    await page.evaluate(() => {
      state.holidays.find((h) => h.name !== "F2 Reopen").active = true;
      [...document.querySelectorAll("form.trip-edit button")].find((b) => b.textContent === "Reopen trip").click();
    });
    await page.waitForFunction(() => document.getElementById("toast").textContent.includes("first"));
    assert.equal(posted, 1);
    // a planned or live trip has no Reopen
    const r = await api(page, "POST", "/api/holidays", { name: "F2 Planned", color: "#1d6fb8", start_at: "2031-01-01" });
    assert.ok(r.status < 300, JSON.stringify(r));
    await page.reload({ waitUntil: "networkidle" });
    await open("F2 Planned");
    assert.equal(await page.locator("form.trip-edit button", { hasText: "Reopen trip" }).count(), 0);
    await ctx.close();
  }],
  ["F2 end to end: the real server's undo brings a deleted trip and its pin back", async ({ browser, base }) => {
    const { ctx, page } = await signedIn(browser, base);
    const t = await trips(page, [{ name: "F2 Real undo", start_at: "2021-03-01", end_at: "2021-03-02" }]);
    const id = t["F2 Real undo"].id;
    await api(page, "POST", "/api/pins", { holiday_id: id, lat: 48.85, lng: 2.35, title: "F2 real pin", note: "" });
    page.on("dialog", (d) => d.accept());
    await page.reload({ waitUntil: "networkidle" });
    await page.evaluate(() => setSheet(true));
    await page.evaluate(() => [...document.querySelectorAll("#trip-list .trip-row")].find((r) => r.textContent.includes("F2 Real undo"))
      .querySelector("button[title='Edit this trip']").click());
    await page.click("form.trip-edit button.danger");
    await page.waitForSelector("#toast .toast-act");
    assert.equal((await api(page, "GET", "/api/holidays")).body.some((h) => h.id === id), false, "really deleted");
    await page.click("#toast .toast-act");
    await page.waitForFunction(() => document.getElementById("toast").textContent.includes("is back"));
    const back = (await api(page, "GET", "/api/map")).body;
    assert.ok(back.holidays.some((h) => h.id === id && h.name === "F2 Real undo"));
    assert.ok(back.pins.some((p) => p.holiday_id === id && p.title === "F2 real pin"));
    assert.ok(await page.evaluate((i) => state.holidays.some((h) => h.id === i), id), "the list shows it again");
    await ctx.close();
  }],
  ["day numbers: a trip started at 21:00 in New York is day 1 that evening and day 2 the next day", async ({ browser, base }) => {
    for (const [tz, start, at, want] of [
      ["America/New_York", "2026-08-01T01:00:00Z", "2026-08-01T02:00:00Z", 1], // 31 Jul 21:00 → 22:00
      ["America/New_York", "2026-08-01T01:00:00Z", "2026-08-01T16:00:00Z", 2], // 1 Aug noon
      ["Asia/Bangkok", "2026-07-31T22:00:00Z", "2026-08-01T05:00:00Z", 1],     // 1 Aug 05:00 → noon
      ["Europe/London", "2026-05-23", "2026-05-24T11:00:00Z", 2],              // a picked date
    ]) {
      const { ctx, page } = await signedIn(browser, base, { timezoneId: tz });
      const n = await page.evaluate(([s, a]) => tripDay({ start_at: s }, a).n, [start, at]);
      assert.equal(n, want, `${tz} ${start} at ${at}`);
      await ctx.close();
    }
  }],
  ["F6: the story says who started the trip, added a pin and wrote the journal; a share says none of it", async ({ browser, base }) => {
    const { ctx, page } = await signedIn(browser, base);
    const t = await trips(page, [{ name: "F6 Who", start_at: "2020-06-01", end_at: "2020-06-03" }]);
    const id = t["F6 Who"].id;
    if (!(await api(page, "GET", "/api/map")).body.pins.some((p) => p.holiday_id === id)) {
      await api(page, "POST", "/api/pins", { holiday_id: id, lat: 50.7, lng: -1.9, title: "F6 beach", note: "sandy" });
      await api(page, "POST", "/api/pins", { holiday_id: id, lat: 50.8, lng: -1.8, title: "F6 old pin", note: "" });
    }
    await api(page, "PATCH", `/api/holidays/${id}`, { journal: "A good week." });
    await page.reload({ waitUntil: "networkidle" });
    const byLines = () => page.$$eval("#story-scroll .story-by", (ns) => ns.map((n) => n.textContent));
    // an old pin ('' = unknown) shows nothing
    await page.evaluate((i) => {
      state.pins.find((p) => p.holiday_id === i && p.title === "F6 old pin").created_by = "";
      openStory(holidayById(i));
    }, id);
    assert.deepEqual(await byLines(), ["Trip started by tester", "Added by tester", "Written by tester"]);
    const beach = await page.$eval("#story-scroll .story-sec", (s) => s.textContent);
    assert.ok(beach.includes("F6 beach") && beach.includes("Added by tester"), beach);
    const url = (await api(page, "POST", `/api/holidays/${id}/share`, { days: 7 })).body.url;
    await page.goto(base + url, { waitUntil: "networkidle" });
    await page.evaluate(() => openStory(state.holidays[0]));
    await page.waitForSelector("#story:not([hidden])");
    assert.ok((await page.textContent("#story-scroll")).includes("F6 beach"));
    assert.deepEqual(await byLines(), []);
    await ctx.close();
  }],
  ["hooks: the story draws tracks (none on close or on a share), the passport opens stats, Family opens import", async ({ browser, base }) => {
    const { ctx, page } = await signedIn(browser, base);
    // stand-in modules, served where linus's real ones live
    const fake = {
      "tracks.js": "window.WSTracks = { draw(id) { (window.trackCalls ||= []).push(id); } };",
      "stats.js": "window.WSStats = { open() { window.statsOpened = (window.statsOpened || 0) + 1; } };",
      "import.js": "window.WSImport = { open() { window.importOpened = (window.importOpened || 0) + 1; } };",
    };
    for (const [f, body] of Object.entries(fake)) {
      await page.route(`**/${f}`, (r) => r.fulfill({ status: 200, contentType: "text/javascript", body }));
    }
    const t = await trips(page, [{ name: "Hooks trip", start_at: "2019-05-01", end_at: "2019-05-02" }]);
    const id = t["Hooks trip"].id;
    await page.reload({ waitUntil: "networkidle" });
    assert.equal(await page.$eval("link[href='/print.css']", (l) => l.media), "print");
    await page.evaluate((i) => openStory(holidayById(i)), id);
    await page.evaluate(() => closeStory());
    assert.deepEqual(await page.evaluate(() => window.trackCalls), [id, null]);
    await page.evaluate(() => document.getElementById("btn-passport").click());
    await page.waitForSelector(".pp-spread");
    await page.click(".pp-stats");
    assert.equal(await page.evaluate(() => window.statsOpened), 1);
    await page.evaluate(() => openFamily());
    await page.locator("button", { hasText: "Import a backup file" }).click();
    assert.equal(await page.evaluate(() => window.importOpened), 1);
    // a share loads the modules too, but never asks for tracks
    const url = (await api(page, "POST", `/api/holidays/${id}/share`, { days: 7 })).body.url;
    await page.goto(base + url, { waitUntil: "networkidle" });
    await page.evaluate(() => { openStory(state.holidays[0]); closeStory(); });
    assert.equal(await page.evaluate(() => window.trackCalls), undefined);
    await ctx.close();
  }],
  ["hooks: without the modules the passport and Family pages carry no dead buttons", async ({ browser, base }) => {
    const { ctx, page } = await signedIn(browser, base);
    for (const f of ["tracks.js", "stats.js", "import.js"]) await page.route(`**/${f}`, (r) => r.fulfill({ status: 404, body: "" }));
    await page.reload({ waitUntil: "networkidle" });
    await page.evaluate(() => document.getElementById("btn-passport").click());
    await page.waitForSelector(".pp-spread");
    assert.equal(await page.$(".pp-stats"), null);
    await page.evaluate(() => openFamily());
    await page.waitForSelector(".fm-list");
    assert.equal(await page.locator("button", { hasText: "Import a backup file" }).count(), 0);
    await ctx.close();
  }],
  ["F3: the story's All photos opens the whole roll under trip-day headings; a share has no gallery", async ({ browser, base }) => {
    const { ctx, page } = await signedIn(browser, base, { timezoneId: "Europe/London" });
    const t = await trips(page, [{ name: "F3 Roll", start_at: "2018-05-23", end_at: "2018-05-25" }]);
    const id = t["F3 Roll"].id;
    if (!(await api(page, "GET", "/api/map")).body.pins.some((p) => p.holiday_id === id)) {
      await api(page, "POST", "/api/pins", { holiday_id: id, lat: 50.75, lng: -1.7, title: "F3 pin", note: "" });
    }
    const roll = [
      { asset_id: "p1", taken_at: "2018-05-23T10:00:00Z", type: "image", duration: 0 },
      { asset_id: "p2", taken_at: "2018-05-24T02:30:00Z", type: "image", duration: 0 }, // before 4 am: still day 1
      { asset_id: "v1", taken_at: "2018-05-24T12:00:00Z", type: "video", duration: 65.4 },
    ];
    let asked = 0;
    await page.route(`**/api/holidays/${id}/timeline`, (r) => { asked++; r.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify(roll) }); });
    await page.reload({ waitUntil: "networkidle" });
    // no photos: no gallery section
    await page.evaluate((i) => openStory(holidayById(i)), id);
    assert.equal(await page.$(".story-gallery"), null);
    await page.evaluate((i) => { state.pins.find((p) => p.holiday_id === i).photo_count = 3; openStory(holidayById(i)); }, id);
    assert.equal(await page.textContent(".story-gallery button"), "See all 3 photos day by day");
    await page.click(".story-gallery button");
    await page.waitForSelector(".trip-gallery .gallery-grid");
    assert.equal(asked, 1);
    const heads = await page.$$eval(".trip-gallery .day-head", (hs) => hs.map((h) => h.textContent));
    assert.equal(heads.length, 2, heads.join("|"));
    assert.match(heads[0], /^Day 1 · Wednesday,? 23 May$/);
    assert.match(heads[1], /^Day 2 · Thursday,? 24 May$/);
    assert.equal(await page.$$eval(".trip-gallery .gallery-grid", (g) => g.map((x) => x.children.length).join(",")), "2,1");
    assert.equal(await page.textContent(".trip-gallery .vbadge"), "▶ 1:05");
    assert.equal(await page.getAttribute(".trip-gallery .vthumb img", "alt"), "Video, 1:05");
    await ctx.close();
  }],
  ["F7: a video plays in the lightbox with its poster; stepping on or closing stops it; a share streams from the share", async ({ browser, base }) => {
    const { ctx, page } = await signedIn(browser, base);
    const list = [
      { asset_id: "v1", taken_at: "2018-05-24T12:00:00Z", type: "video", duration: 9 },
      { asset_id: "p1", taken_at: "2018-05-24T13:00:00Z", type: "image", duration: 0 },
    ];
    const lbState = () => page.evaluate(() => {
      const v = document.getElementById("lb-video"), i = document.getElementById("lb-img");
      return { video: !v.hidden, src: v.getAttribute("src"), poster: v.getAttribute("poster"), img: !i.hidden,
        orig: document.getElementById("lb-orig").getAttribute("href"), label: document.getElementById("lb-orig").textContent };
    });
    await page.evaluate((l) => openLightbox(l, 0, {}), list);
    let s = await lbState();
    assert.deepEqual([s.video, s.img, s.src, s.poster, s.orig], [true, false, "/api/photo/v1/video", "/api/photo/v1/preview", "/api/photo/v1/original"]);
    assert.equal(await page.isVisible("#lb-video"), true);
    await page.click("#lb-next");
    s = await lbState();
    assert.deepEqual([s.video, s.img, s.src], [false, true, null]);
    await page.click("#lb-prev");
    await page.click("#lb-close");
    s = await lbState();
    assert.deepEqual([s.video, s.src], [false, null]);
    // a story chapter marks its video tile
    await page.evaluate((l) => {
      const g = document.createElement("div");
      g.id = "probe";
      g.appendChild(thumbCell(l[0], () => {}));
      g.appendChild(thumbCell(l[1], () => {}));
      document.body.appendChild(g);
    }, list);
    assert.deepEqual(await page.$$eval("#probe > *", (n) => n.map((x) => x.tagName + (x.className ? "." + x.className : ""))), ["SPAN.vthumb", "IMG"]);
    await ctx.close();
    // share: the stream and the open link stay on the share route
    const sctx = await browser.newContext({ serviceWorkers: "block" });
    const sp = await sctx.newPage();
    await sp.route("**/api/share/tok**", (r) => r.fulfill({ status: 410, contentType: "application/json", body: '{"error":"this share link has expired"}' }));
    await sp.goto(base + "/share/tok");
    await sp.waitForFunction(() => document.body.textContent.includes("has expired"), null, { timeout: 3000 });
    await sp.evaluate((l) => openLightbox(l, 0, {}), list);
    const ss = await sp.evaluate(() => [document.getElementById("lb-video").getAttribute("src"), document.getElementById("lb-orig").getAttribute("href"), document.getElementById("lb-orig").textContent]);
    assert.deepEqual(ss, ["/api/share/tok/photo/v1/video", "/api/share/tok/photo/v1/video", "Open video"]);
    await sctx.close();
  }],
  ["F5: long legs bend into great circles across the 180th meridian; short legs stay straight", async ({ browser, base }) => {
    const { ctx, page } = await signedIn(browser, base);
    const r = await page.evaluate(() => {
      const jumps = (pts) => pts.slice(1).some((p, i) => Math.abs(p[1] - pts[i][1]) > 180);
      const bad = (pts) => pts.some((p) => !Number.isFinite(p[0]) || !Number.isFinite(p[1]));
      const short = arcRoute([[50.72, -1.88], [50.75, -1.7], [51.5, -0.12]]);
      const atlantic = arcRoute([[51.47, -0.45], [40.64, -73.78]]);
      const pacific = arcRoute([[35.55, 139.78], [33.94, -118.4]]);
      const chain = arcRoute([[33.94, -118.4], [35.55, 139.78], [35.44, 139.64], [33.94, -118.4]]);
      const opposite = arcRoute([[10, 20], [-10, -160]]);
      return {
        short: [short.pts.length, short.planes.length],
        atlantic: { n: atlantic.pts.length, top: Math.max(...atlantic.pts.map((p) => p[0])), planes: atlantic.planes.length, deg: atlantic.planes[0].deg },
        pacific: { jumps: jumps(pacific.pts), bad: bad(pacific.pts), deg: pacific.planes[0].deg, end: pacific.pts[pacific.pts.length - 1] },
        chain: { jumps: jumps(chain.pts), planes: chain.planes.length, bad: bad(chain.pts) },
        opposite: { bad: bad(opposite.pts), n: opposite.pts.length },
      };
    });
    assert.deepEqual(r.short, [3, 0]);
    assert.ok(r.atlantic.n > 30 && r.atlantic.planes === 1, JSON.stringify(r.atlantic));
    assert.ok(r.atlantic.top > 52, "the arc bows north of both ends: " + r.atlantic.top);
    assert.ok(r.atlantic.deg < -60 && r.atlantic.deg > -120, "flying west: " + r.atlantic.deg);
    assert.equal(r.pacific.jumps, false);
    assert.equal(r.pacific.bad, false);
    assert.ok(r.pacific.deg > 60 && r.pacific.deg < 120, "flying east: " + r.pacific.deg);
    assert.ok(Math.abs(r.pacific.end[1] - (-118.4 + 360)) < 1e-6, "LA drawn east of Tokyo: " + r.pacific.end);
    assert.deepEqual([r.chain.jumps, r.chain.planes, r.chain.bad], [false, 2, false]);
    assert.deepEqual(r.opposite, { bad: false, n: 2 });
    await ctx.close();
  }],
  ["F5: a flown trip shows one plane per long leg on the map, dimmed with its trip", async ({ browser, base }) => {
    const { ctx, page } = await signedIn(browser, base);
    const t = await trips(page, [{ name: "F5 New York", start_at: "2017-04-01", end_at: "2017-04-09" }]);
    const id = t["F5 New York"].id;
    if (!(await api(page, "GET", "/api/map")).body.pins.some((p) => p.holiday_id === id)) {
      await api(page, "POST", "/api/pins", { holiday_id: id, lat: 51.47, lng: -0.45, title: "F5 Heathrow", note: "" });
      await api(page, "POST", "/api/pins", { holiday_id: id, lat: 40.64, lng: -73.78, title: "F5 JFK", note: "" });
      await api(page, "POST", "/api/pins", { holiday_id: id, lat: 40.75, lng: -73.99, title: "F5 Midtown", note: "" });
    }
    await page.reload({ waitUntil: "networkidle" });
    const planes = () => page.evaluate((i) => state.routes.filter((r) => r instanceof L.Marker && r.tripId === i).length, id);
    assert.equal(await planes(), 1);
    await page.evaluate(() => setFocus(-1));
    assert.equal(await page.evaluate((i) => state.routes.find((r) => r instanceof L.Marker && r.tripId === i).getElement().classList.contains("dimmed"), id), true);
    // a reload doesn't stack planes
    await page.evaluate(() => loadData());
    assert.equal(await planes(), 1);
    await ctx.close();
  }],
];
