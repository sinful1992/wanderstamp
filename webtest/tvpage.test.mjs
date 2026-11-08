// Task 133: the /tv page with the door's API mocked in the browser, so every
// screen, key and failure can be driven exactly. tvlive.test.mjs runs the same
// page against the real door.
import assert from "node:assert/strict";
import { signedIn } from "./harness.mjs";

// a 1x1 JPEG for every photo and tile
const JPEG = Buffer.from(
  "/9j/4AAQSkZJRgABAQEASABIAAD/2wBDAP//////////////////////////////////////////////////////////////////////////////////////wgALCAABAAEBAREA/8QAFBABAAAAAAAAAAAAAAAAAAAAAP/aAAgBAQABPxA=", "base64");

const H = (id, name, extra = {}) => ({
  id, name, color: "#2a7f62", start_at: "2026-08-02T00:00:00Z", end_at: "2026-08-05T23:59:59Z", active: false, planned: false,
  journal: "", cover_asset: `c${id}`, pin_count: 0, photo_count: 0, dest_name: "", dest_lat: 0, dest_lng: 0, ...extra,
});
const P = (id, hid, title, lat, lng, visits, extra = {}) => ({
  id, holiday_id: hid, kind: "photo", lat, lng, title, note: "", created_at: visits[0].at, visited_at: visits[0].at,
  photo_count: 0, cover_asset: "", visits, created_by: "", ...extra,
});
const V = (at, until) => ({ at, until, n: 0 });
const PH = (asset, pin, taken_at, type = "image") => ({ asset_id: asset, pin_id: pin, taken_at, type, duration: 0 });

// Lake District: 3 stops, Keswick visited twice (day 1 and day 3)
function fixture() {
  const lake = H(1, "Lake District", { pin_count: 3, photo_count: 6 });
  const pins = [
    P(10, 1, "Keswick", 54.6013, -3.1347, [V("2026-08-02T10:00:00Z", "2026-08-02T12:00:00Z"), V("2026-08-04T09:00:00Z", "2026-08-04T10:00:00Z")], { note: "Pencil museum, then rain." }),
    P(11, 1, "Buttermere", 54.5413, -3.2766, [V("2026-08-03T11:00:00Z", "2026-08-03T15:00:00Z")]),
    P(12, 1, "Castlerigg", 54.6027, -3.0985, [V("2026-08-02T16:00:00Z", "2026-08-02T16:00:00Z")], { kind: "manual" }),
  ];
  const photos = [
    PH("a1", 10, "2026-08-02T10:05:00Z"), PH("a2", 10, "2026-08-02T11:00:00Z"),
    PH("b1", 11, "2026-08-03T11:30:00Z"), PH("b2", 11, "2026-08-03T12:00:00Z", "video"), PH("b3", 11, "2026-08-03T14:00:00Z"),
    PH("a3", 10, "2026-08-04T09:30:00Z"),
  ];
  const paris = H(2, "Paris <img src=x onerror=window.__xss=1>", { start_at: "2025-04-10T00:00:00Z", pin_count: 0, photo_count: 0, cover_asset: "" });
  const empty = H(3, "Norway", { start_at: "2024-07-01T00:00:00Z" });
  return { trips: [lake, paris, empty], details: { 1: { holiday: lake, pins, photos }, 2: { holiday: paris, pins: [], photos: [] }, 3: { holiday: empty, pins: [], photos: [] } } };
}

// Opens /tv with the door mocked. st is live: tests flip st.paired / st.pairNow / st.offline.
async function tvPage(browser, base, opts = {}) {
  const st = { paired: true, pairNow: false, offline: false, codes: 0, polls: 0, me: 0, gone: false, data: fixture(), photoReqs: [], ...opts.state };
  const ctx = await browser.newContext({ viewport: opts.viewport || { width: 1920, height: 1080 }, ...(opts.mobile ? { isMobile: true, deviceScaleFactor: 2 } : {}), locale: "en-GB", timezoneId: "Europe/London", serviceWorkers: "block" });
  const page = await ctx.newPage();
  const errors = [];
  page.on("pageerror", (e) => errors.push(String(e)));
  if (opts.clock) await page.clock.install({ time: new Date("2026-11-08T20:00:00Z") });
  await ctx.route(/arcgisonline\.com/, (r) => r.fulfill({ status: 200, contentType: "image/jpeg", body: JPEG }));
  const json = (r, status, body) => r.fulfill({ status, contentType: "application/json", body: JSON.stringify(body) });
  await ctx.route(/\/api\/tv\/|\/tv\/pair/, async (r) => {
    if (st.offline) return r.abort("internetdisconnected");
    const u = new URL(r.request().url());
    const m = r.request().method();
    const p = u.pathname;
    if (m === "POST" && p === "/tv/pair") {
      st.codes++;
      return json(r, 200, { code: st.codes === 1 ? "KXRT" : "MPQW", poll: `poll${st.codes}`, expires_in: 300 });
    }
    if (p.startsWith("/tv/pair/")) {
      st.polls++;
      if (st.gone) { st.gone = false; return json(r, 410, { error: "code expired" }); }
      if (st.pairNow) { st.paired = true; st.pairNow = false; return json(r, 200, { name: "Living room" }); }
      await new Promise((res) => setTimeout(res, 50));
      return json(r, 202, { pending: true });
    }
    if (!st.paired) return json(r, 401, { error: "not paired" });
    if (p === "/api/tv/me") { st.me++; return json(r, 200, { name: "Living room" }); }
    if (p === "/api/tv/trips") {
      st.tripCalls = (st.tripCalls || 0) + 1;
      if (st.tripsFail > 0) { st.tripsFail--; return json(r, 500, { error: "database error" }); }
      return json(r, 200, st.data.trips);
    }
    const t = p.match(/^\/api\/tv\/trips\/(\d+)$/);
    if (t) return st.data.details[t[1]] ? json(r, 200, st.data.details[t[1]]) : json(r, 404, { error: "unknown trip" });
    const ph = p.match(/^\/api\/tv\/photo\/([^/]+)\/([^/]+)$/);
    if (ph) { st.photoReqs.push(ph[2]); return r.fulfill({ status: 200, contentType: "image/jpeg", body: JPEG }); }
    return json(r, 404, { error: "not found" });
  });
  await page.goto(base + "/tv/index.html"); // the door serves it at /tv; the main listener only by file name
  return { ctx, page, st, errors };
}

const screen = (page) => page.evaluate(() => window.WSTV && window.WSTV.screen);
const waitScreen = (page, id) => page.waitForFunction((s) => window.WSTV && window.WSTV.screen === s, id, { timeout: 5000 });
const key = async (page, k, n = 1) => { for (let i = 0; i < n; i++) await page.keyboard.press(k); };
const J = (page) => page.evaluate(() => window.WSTV.journey);
const text = (page, sel) => page.locator(sel).innerText();

export const tests = [
  ["an empty trip after a full one shows nothing of the one before (linus #9577 bug 1)", async ({ browser, base }) => {
    const { ctx, page } = await tvPage(browser, base);
    await page.waitForSelector(".tripstamp.focus");
    await key(page, "Enter");
    await waitScreen(page, "journey");
    await page.waitForFunction(() => document.querySelectorAll("#j-route .s").length > 0);
    await key(page, "Escape");
    await waitScreen(page, "shelf");
    await page.waitForSelector(".tripstamp.focus");
    await key(page, "ArrowRight", 2);
    await key(page, "Enter");
    await waitScreen(page, "journey");
    assert.equal(await page.locator("#j-stop").innerText(), "");
    assert.equal(await page.locator("#j-note").innerText(), "");
    assert.equal(await page.locator("#j-count").innerText(), "");
    assert.equal(await page.locator("#j-route .s").count(), 0);
    assert.equal(await page.locator("#j-card").evaluate((n) => n.classList.contains("nophoto")), true);
    assert.equal(await page.locator("#j-img").getAttribute("src"), null);
    await ctx.close();
  }],

  ["trips failing right after pairing: offline screen, then the shelf, never a dead blank shelf (linus #9577 bug 2)", async ({ browser, base }) => {
    const { ctx, page, st } = await tvPage(browser, base, { clock: true, state: { paired: false, tripsFail: 1 } });
    await waitScreen(page, "pair");
    st.pairNow = true;
    await waitScreen(page, "offline");
    await page.clock.runFor(5100);
    await waitScreen(page, "shelf");
    await page.waitForFunction(() => document.querySelectorAll(".tripstamp").length === 3);
    await ctx.close();
  }],

  ["a TV left on the shelf picks up a new trip within a minute and keeps its focus (linus #9577 item 3)", async ({ browser, base }) => {
    const { ctx, page, st } = await tvPage(browser, base, { clock: true });
    await waitScreen(page, "shelf");
    await page.waitForFunction(() => document.querySelectorAll(".tripstamp").length === 3);
    await key(page, "ArrowRight");
    const calls = st.tripCalls;
    await page.clock.runFor(61000);
    for (let i = 0; i < 50 && st.tripCalls <= calls; i++) await new Promise((r) => setTimeout(r, 50));
    assert.ok(st.tripCalls > calls, "trips asked again");
    assert.equal(await page.locator(".tripstamp").count(), 3, "unchanged: no redraw needed");
    st.data.trips = [{ ...st.data.trips[0], id: 9, name: "Brand new" }, ...st.data.trips];
    st.data.details[9] = { holiday: st.data.trips[0], pins: [], photos: [] };
    await page.clock.runFor(61000);
    await page.waitForFunction(() => document.querySelectorAll(".tripstamp").length === 4);
    assert.equal(await page.locator(".tripstamp.focus").getAttribute("data-id"), "2", "focus stays on the trip it was on");
    await ctx.close();
  }],

  ["Back from a journey reloads the trips (new photos on a live trip show up)", async ({ browser, base }) => {
    const { ctx, page, st } = await tvPage(browser, base);
    await page.waitForSelector(".tripstamp.focus");
    await key(page, "Enter");
    await waitScreen(page, "journey");
    const d = st.data.details[1];
    st.data.details[1] = { ...d, photos: [...d.photos, { asset_id: "a9", pin_id: 10, taken_at: "2026-08-04T09:40:00Z", type: "image", duration: 0 }] };
    await key(page, "Escape");
    await waitScreen(page, "shelf");
    await page.waitForSelector(".tripstamp.focus");
    await key(page, "Enter");
    await waitScreen(page, "journey");
    assert.equal((await J(page)).slides, 8, "the new photo is in the journey");
    await ctx.close();
  }],

  ["the day stamp's date keeps its numbers in the body face", async ({ browser, base }) => {
    const { ctx, page } = await tvPage(browser, base);
    await page.waitForSelector(".tripstamp.focus");
    await key(page, "Enter");
    await waitScreen(page, "journey");
    assert.ok(await page.locator("#j-date .num").count() >= 1);
    assert.match(await page.locator("#j-date .num").first().innerText(), /^\d+$/);
    await ctx.close();
  }],

  ["unpaired TV shows a 4-letter code and opens the shelf once the phone pairs it", async ({ browser, base }) => {
    const { ctx, page, st, errors } = await tvPage(browser, base, { state: { paired: false } });
    await waitScreen(page, "pair");
    await page.waitForFunction(() => document.querySelectorAll("#pair-code span").length === 4);
    assert.equal((await page.locator("#pair-code").innerText()).replace(/\s/g, ""), "KXRT");
    assert.ok(st.polls >= 1, "polls while waiting");
    st.pairNow = true;
    await waitScreen(page, "shelf");
    await page.waitForFunction(() => document.querySelectorAll(".tripstamp").length === 3);
    assert.match(await text(page, "#shelf-tally"), /3 trips, 6 photos/);
    assert.deepEqual(errors, []);
    await ctx.close();
  }],

  ["an expired code (410) is replaced by a new one", async ({ browser, base }) => {
    const { ctx, page, st } = await tvPage(browser, base, { state: { paired: false } });
    await page.waitForFunction(() => document.querySelector("#pair-code").textContent === "KXRT");
    st.gone = true;
    await page.waitForFunction(() => document.querySelector("#pair-code").textContent === "MPQW");
    assert.equal(st.codes, 2);
    await ctx.close();
  }],

  ["shelf: left/right stop at the ends, down reaches the actions, Back walks up then leaves", async ({ browser, base }) => {
    const { ctx, page } = await tvPage(browser, base);
    await waitScreen(page, "shelf");
    await page.waitForSelector(".tripstamp.focus");
    await key(page, "ArrowLeft");
    assert.equal((await page.evaluate(() => WSTV.shelf)).focus, 0);
    await key(page, "ArrowRight", 5);
    assert.equal((await page.evaluate(() => WSTV.shelf)).focus, 2);
    assert.equal(await page.locator(".tripstamp.focus").getAttribute("data-id"), "3");
    await key(page, "ArrowDown");
    assert.equal((await page.evaluate(() => WSTV.shelf)).row, 1);
    await key(page, "ArrowRight", 3);
    assert.match((await text(page, ".btn.focus")).toLowerCase(), /all photos/, "right stops at the last action");
    assert.equal((await page.evaluate(() => WSTV.shelf)).act, 1);
    assert.equal(await page.evaluate(() => WSTV.back()), true, "Back from the actions goes up to the trips");
    assert.equal((await page.evaluate(() => WSTV.shelf)).row, 0);
    assert.equal(await page.evaluate(() => WSTV.back()), false, "Back on the trips lets the app close");
    await ctx.close();
  }],

  ["trip names are text, never HTML", async ({ browser, base }) => {
    const { ctx, page } = await tvPage(browser, base);
    await page.waitForFunction(() => document.querySelectorAll(".tripstamp").length === 3);
    assert.match(await page.locator('.tripstamp[data-id="2"] .band').innerText(), /<img src=x/);
    assert.equal(await page.evaluate(() => window.__xss), undefined);
    await ctx.close();
  }],

  ["journey: stops in visit order, photos with their own visit, keys move by photo and by stop", async ({ browser, base }) => {
    const { ctx, page, st, errors } = await tvPage(browser, base);
    await page.waitForSelector(".tripstamp.focus");
    await key(page, "Enter");
    await waitScreen(page, "journey");
    // Keswick (a1,a2) → Castlerigg (card) → Buttermere (b1,b2,b3) → Keswick again (a3) = 7 slides, 4 stops
    let j = await J(page);
    assert.equal(j.slides, 7);
    assert.match((await text(page, "#j-stop")).toLowerCase(), /keswick/);
    assert.match(await text(page, "#j-note"), /Pencil museum/);
    assert.match(await text(page, "#j-count"), /1 of 2 photos here/);
    assert.match((await text(page, "#j-day")).toLowerCase(), /day 1/);
    await key(page, "ArrowRight");
    assert.match(await text(page, "#j-count"), /2 of 2/);
    await key(page, "ArrowRight");
    assert.match((await text(page, "#j-stop")).toLowerCase(), /castlerigg/);
    assert.equal(await page.locator("#j-card").evaluate((n) => n.classList.contains("nophoto")), true, "a stop without photos has no postcard");
    await key(page, "ArrowDown");
    assert.match((await text(page, "#j-stop")).toLowerCase(), /buttermere/);
    assert.match((await text(page, "#j-day")).toLowerCase(), /day 2/);
    await key(page, "ArrowRight");
    assert.equal(await page.locator("#j-vmark").isVisible(), true, "a video shows as its preview, marked");
    await key(page, "ArrowUp");
    j = await J(page);
    assert.equal(j.i, 3, "Up goes to the start of this stop first");
    await key(page, "ArrowUp");
    assert.equal((await J(page)).i, 2, "then to the stop before");
    await key(page, "ArrowDown", 2);
    assert.match((await text(page, "#j-stop")).toLowerCase(), /keswick/);
    assert.equal(await text(page, "#j-note"), "", "the note is said once, at the first visit");
    assert.match((await text(page, "#j-day")).toLowerCase(), /day 3/);
    assert.ok(!st.photoReqs.includes("original") && !st.photoReqs.includes("video"), "only thumb/preview are asked for");
    assert.ok(st.photoReqs.includes("preview"));
    await key(page, "ArrowRight");
    assert.equal((await J(page)).ended, true);
    assert.equal(await page.locator("#j-end").isVisible(), true);
    assert.match(await text(page, "#j-end-sum"), /4 stops, 6 photos/);
    await key(page, "Enter");
    assert.equal((await J(page)).i, 0, "OK at the end plays again");
    assert.equal(await page.evaluate(() => WSTV.back()), true);
    await waitScreen(page, "shelf");
    assert.deepEqual(errors, []);
    await ctx.close();
  }],

  ["journey plays by itself, OK pauses and resumes", async ({ browser, base }) => {
    const { ctx, page } = await tvPage(browser, base, { clock: true });
    await page.waitForSelector(".tripstamp.focus");
    await key(page, "Enter");
    await waitScreen(page, "journey");
    await page.clock.runFor(6100);
    assert.equal((await J(page)).i, 1, "next photo after 6 s");
    await key(page, "Enter");
    assert.equal((await J(page)).paused, true);
    assert.equal(await page.locator("#j-paused").isVisible(), true);
    await page.clock.runFor(30000);
    assert.equal((await J(page)).i, 1, "paused stays put");
    // a synthetic event: older playwright-core has no "MediaPlayPause" key name
    await page.evaluate(() => document.dispatchEvent(new KeyboardEvent("keydown", { key: "MediaPlayPause", bubbles: true })));
    assert.equal((await J(page)).paused, false);
    await page.clock.runFor(6100);
    assert.equal((await J(page)).i, 2);
    await page.clock.runFor(5100); // Castlerigg card is 5 s
    assert.equal((await J(page)).i, 3);
    await page.clock.runFor(4 * 6100);
    assert.equal((await J(page)).ended, true, "ends after the last photo");
    await ctx.close();
  }],

  ["a trip with no stops ends at once and says so; Back returns", async ({ browser, base }) => {
    const { ctx, page, errors } = await tvPage(browser, base);
    await page.waitForSelector(".tripstamp.focus");
    await key(page, "ArrowRight");
    await key(page, "Enter");
    await waitScreen(page, "journey");
    assert.equal((await J(page)).ended, true);
    assert.match(await text(page, "#j-end-sum"), /no stops yet/);
    await key(page, "Enter"); // nothing to replay: no crash
    await key(page, "Escape");
    await waitScreen(page, "shelf");
    assert.equal((await page.evaluate(() => WSTV.shelf)).focus, 1, "the shelf keeps its place");
    assert.deepEqual(errors, []);
    await ctx.close();
  }],

  ["all photos: grid moves by rows, OK opens the photo, Back closes it then leaves", async ({ browser, base }) => {
    const { ctx, page } = await tvPage(browser, base);
    await page.waitForSelector(".tripstamp.focus");
    await key(page, "ArrowDown");
    await key(page, "ArrowRight");
    await key(page, "Enter");
    await waitScreen(page, "photos");
    assert.equal(await page.locator("#p-grid .cell").count(), 6);
    assert.match(await text(page, "#p-title"), /6 photos/);
    await key(page, "ArrowDown");
    assert.equal((await page.evaluate(() => WSTV.photos)).i, 5, "down past the last row lands on the last photo");
    await key(page, "ArrowUp");
    assert.equal((await page.evaluate(() => WSTV.photos)).i, 5, "up from the first row stays put");
    await key(page, "ArrowLeft", 2);
    await key(page, "Enter");
    assert.equal((await page.evaluate(() => WSTV.photos)).viewer, true);
    assert.match(await text(page, "#p-cap"), /Buttermere, Day 2.*4 of 6/);
    await key(page, "ArrowRight");
    assert.match(await text(page, "#p-cap"), /5 of 6/);
    assert.equal(await page.evaluate(() => WSTV.back()), true);
    assert.equal((await page.evaluate(() => WSTV.photos)).viewer, false);
    await key(page, "Backspace");
    await waitScreen(page, "shelf");
    await ctx.close();
  }],

  ["an unpaired TV goes back to the code within a minute, even mid-journey", async ({ browser, base }) => {
    const { ctx, page, st } = await tvPage(browser, base, { clock: true });
    await page.waitForSelector(".tripstamp.focus");
    await key(page, "Enter");
    await waitScreen(page, "journey");
    await key(page, "Enter"); // paused: no timers would notice
    st.paired = false;
    await page.clock.runFor(61000);
    await waitScreen(page, "pair");
    await ctx.close();
  }],

  ["no server: the offline screen, then the shelf once it is back", async ({ browser, base }) => {
    const { ctx, page, st } = await tvPage(browser, base, { clock: true, state: { offline: true } });
    await waitScreen(page, "offline");
    st.offline = false;
    await page.clock.runFor(5100);
    await waitScreen(page, "shelf");
    await ctx.close();
  }],

  ["no trips: the shelf says so and the keys do nothing harmful", async ({ browser, base }) => {
    const { ctx, page, errors } = await tvPage(browser, base, { state: { data: { trips: [], details: {} } } });
    await waitScreen(page, "shelf");
    assert.equal(await page.locator("#shelf-empty").isVisible(), true);
    for (const k of ["ArrowRight", "ArrowDown", "Enter", "ArrowUp"]) await key(page, k);
    assert.equal(await screen(page), "shelf");
    assert.deepEqual(errors, []);
    await ctx.close();
  }],

  ["the stage fits a 960x540 WebView and a 4K screen with no scrolling", async ({ browser, base }) => {
    for (const viewport of [{ width: 960, height: 540 }, { width: 3840, height: 2160 }, { width: 1280, height: 720 }]) {
      const { ctx, page } = await tvPage(browser, base, { viewport });
      await waitScreen(page, "shelf");
      const r = await page.evaluate(() => {
        const b = document.getElementById("stage").getBoundingClientRect();
        return { w: b.width, h: b.height, l: b.left, t: b.top, sw: document.documentElement.scrollWidth, sh: document.documentElement.scrollHeight };
      });
      assert.ok(Math.abs(r.w - viewport.width) < 2 && Math.abs(r.h - viewport.height) < 2, `stage ${r.w}x${r.h} on ${viewport.width}x${viewport.height}`);
      assert.ok(r.sw <= viewport.width && r.sh <= viewport.height, "no scrollbars");
      await ctx.close();
    }
  }],

  // The Shield WebView (dpr 2, a real mobile viewport) grows innerWidth to fit the unscaled stage: 1200 on a 960 screen.
  ["the stage fits a mobile-viewport WebView like the Shield's, whose innerWidth is wider than the screen", async ({ browser, base }) => {
    const { ctx, page } = await tvPage(browser, base, { viewport: { width: 960, height: 540 }, mobile: true });
    await waitScreen(page, "shelf");
    const r = await page.evaluate(() => {
      const b = document.getElementById("stage").getBoundingClientRect(), h = document.documentElement;
      return { w: b.width, h: b.height, l: b.left, t: b.top, iw: innerWidth, cw: h.clientWidth, ch: h.clientHeight };
    });
    assert.ok(Math.abs(r.w - r.cw) < 2 && Math.abs(r.h - r.ch) < 2 && Math.abs(r.l) < 2 && Math.abs(r.t) < 2, `stage ${JSON.stringify(r)}`);
    await ctx.close();
  }],

  ["the route bar says its numbers are stops, and the stamp has a solid background (USER #9626)", async ({ browser, base }) => {
    const { ctx, page } = await tvPage(browser, base);
    await page.waitForSelector(".tripstamp.focus");
    await key(page, "Enter");
    await waitScreen(page, "journey");
    await page.waitForFunction(() => document.querySelectorAll("#j-route .s").length > 0);
    const r = await page.evaluate(() => ({
      labels: [...document.querySelectorAll("#j-route .s")].map((s) => s.textContent),
      bg: getComputedStyle(document.querySelector(".inkstamp")).backgroundColor,
      blend: getComputedStyle(document.querySelector(".inkstamp")).mixBlendMode,
    }));
    assert.deepEqual(r.labels, ["1", "2", "3", "4"]);
    assert.equal(await page.locator("#j-route .rlabel").innerText(), "Stops");
    assert.ok(parseFloat(r.bg.split(",").pop()) >= 0.9, r.bg);
    assert.equal(r.blend, "normal");
    await ctx.close();
  }],

  ["the route bar stays clear of the remote hints at every stop of a long trip (linus #9632)", async ({ browser, base }) => {
    const trip = H(1, "Long trip", { pin_count: 20, photo_count: 0 });
    const pins = Array.from({ length: 20 }, (_, i) => P(100 + i, 1, "Stop " + i, 54.5 + i * 0.005, -3.1, [V("2026-08-02T" + String(i + 1).padStart(2, "0") + ":00:00Z", "2026-08-02T" + String(i + 1).padStart(2, "0") + ":30:00Z")]));
    const { ctx, page } = await tvPage(browser, base, { state: { data: { trips: [trip], details: { 1: { holiday: trip, pins, photos: [] } } } } });
    await page.waitForSelector(".tripstamp.focus");
    await key(page, "Enter");
    await waitScreen(page, "journey");
    await page.waitForFunction(() => document.querySelectorAll("#j-route .s").length > 0);
    for (let i = 0; i < 20; i++) {
      const r = await page.evaluate(() => ({ bar: document.querySelector("#j-route").getBoundingClientRect().right, hints: document.querySelector("#journey .remote").getBoundingClientRect().left }));
      assert.ok(r.bar < r.hints, "stop " + i + ": " + JSON.stringify(r));
      await key(page, "ArrowDown");
      await page.waitForTimeout(30);
    }
    await ctx.close();
  }],

  ["a short hop slides the map instead of zooming out and back (USER #9626)", async ({ browser, base }) => {
    const { ctx, page } = await tvPage(browser, base);
    await page.waitForSelector(".tripstamp.focus");
    await page.evaluate(() => {
      window.__moves = [];
      for (const k of ["flyTo", "panTo"]) { const o = L.Map.prototype[k]; L.Map.prototype[k] = function (...a) { window.__moves.push(k); return o.apply(this, a); }; }
    });
    await key(page, "Enter");
    await waitScreen(page, "journey");
    await page.waitForFunction(() => document.querySelectorAll("#j-route .s").length > 0);
    await page.waitForTimeout(3000); // the opening move has landed
    await page.evaluate(() => { window.__moves.length = 0; });
    await key(page, "ArrowDown");
    await page.waitForFunction(() => window.__moves.length > 0);
    assert.deepEqual(await page.evaluate(() => window.__moves), ["panTo"]);
    await ctx.close();
  }],

  ["Show on TV: the phone pairs with the typed code and lists, then unpairs, the TV", async ({ browser, base }) => {
    const { ctx, page } = await signedIn(browser, base);
    const st = { devices: [], posts: [], deletes: 0 };
    await ctx.route(/\/api\/tv\/(pair|devices)/, async (r) => {
      const m = r.request().method(), p = new URL(r.request().url()).pathname;
      const json = (s, b) => r.fulfill({ status: s, contentType: "application/json", body: b === undefined ? "" : JSON.stringify(b) });
      if (m === "POST" && p === "/api/tv/pair") {
        const b = r.request().postDataJSON();
        st.posts.push(b);
        if (b.code !== "KXRT") return json(404, { error: "unknown code" });
        st.devices.push({ id: 7, name: b.name, created_by: "tester", created_at: "2026-11-08T20:00:00Z", last_seen: "" });
        return json(201, { id: 7, name: b.name });
      }
      if (m === "GET" && p === "/api/tv/devices") return json(200, st.devices);
      if (m === "DELETE" && p === "/api/tv/devices/7") { st.deletes++; st.devices = []; return r.fulfill({ status: 204, body: "" }); }
      return json(404, { error: "x" });
    });
    page.on("dialog", (d) => d.accept());
    await page.click("#btn-account");
    await page.waitForSelector("#tv-pair");
    await page.fill("#tv-code", " zzzz ");
    await page.click("#tv-pair button[type=submit]");
    await page.waitForFunction(() => /isn't on any TV/.test(document.body.innerText));
    assert.equal(st.posts[0].code, "ZZZZ", "code trimmed and upper-cased");
    await page.fill("#tv-code", "kx rt");
    await page.fill("#tv-name", "  Bedroom ");
    await page.click("#tv-pair button[type=submit]");
    await page.waitForSelector("#tv-list .tv-item");
    assert.deepEqual(st.posts[1], { code: "KXRT", name: "Bedroom" });
    assert.match(await page.locator("#tv-list").innerText(), /Bedroom.*paired by tester, not used yet/s);
    assert.equal(await page.inputValue("#tv-code"), "", "the code box empties after pairing");
    await page.click("#tv-list button");
    await page.waitForFunction(() => !document.querySelector("#tv-list .tv-item"));
    assert.equal(st.deletes, 1);
    await ctx.close();
  }],
];
