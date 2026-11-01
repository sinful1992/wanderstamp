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
];
