// Task 133: the TV page against the real door (TV_LISTEN_ADDR, started by the
// harness on its own port). Pairs a TV from a signed-in session, browses,
// plays, and unpairs, all through the real server.
import assert from "node:assert/strict";
import { signedIn, api } from "./harness.mjs";

async function doorUp(tvBase) {
  try { await fetch(tvBase + "/tv"); return true; } catch { return false; }
}

async function tvContext(browser) {
  const ctx = await browser.newContext({ viewport: { width: 1920, height: 1080 }, locale: "en-GB", timezoneId: "Europe/London", serviceWorkers: "block" });
  await ctx.route(/arcgisonline\.com/, (r) => r.fulfill({ status: 204, body: "" }));
  return ctx;
}

const waitScreen = (page, id, timeout = 10000) => page.waitForFunction((s) => window.WSTV && window.WSTV.screen === s, id, { timeout });

// Shows the code on a fresh TV page and types it on the phone side; returns the paired TV page.
async function pairTV(browser, base, tvBase, phone, name = "Living room") {
  const ctx = await tvContext(browser);
  const tv = await ctx.newPage();
  const errors = [];
  tv.on("pageerror", (e) => errors.push(String(e)));
  await tv.goto(tvBase + "/tv");
  await waitScreen(tv, "pair");
  await tv.waitForFunction(() => document.querySelectorAll("#pair-code span").length === 4);
  const code = (await tv.locator("#pair-code").innerText()).replace(/\s/g, "");
  assert.match(code, /^[A-HJ-NP-Z]{4}$/, `code ${code}`);
  const r = await api(phone, "POST", "/api/tv/pair", { code: code.toLowerCase(), name });
  assert.equal(r.status, 201, JSON.stringify(r));
  await waitScreen(tv, "shelf", 40000);
  return { ctx, tv, errors, device: r.body, code };
}

let seeded = null;
async function seed(phone) {
  if (seeded) return seeded;
  const trip = (await api(phone, "POST", "/api/holidays", { name: "TV Lakes <b>bold</b>", color: "#2a7f62", start_at: "2025-08-02", end_at: "2025-08-04" })).body;
  await api(phone, "POST", "/api/pins", { holiday_id: trip.id, lat: 54.6013, lng: -3.1347, title: "Keswick", note: "Pencil museum", created_at: "2025-08-02T10:00:00Z" });
  await api(phone, "POST", "/api/pins", { holiday_id: trip.id, lat: 54.5413, lng: -3.2766, title: "Buttermere", note: "", created_at: "2025-08-03T11:00:00Z" });
  const planned = (await api(phone, "POST", "/api/holidays", { name: "TV Future trip", color: "#b3372a", start_at: "2030-05-01" })).body;
  seeded = { trip, planned };
  return seeded;
}

export const tests = [
  ["door serves only the TV page, its assets and the TV API", async ({ tvBase }) => {
    assert.ok(await doorUp(tvBase), "door is listening");
    for (const p of ["/tv", "/tv/", "/tv/tv.js", "/tv/tv.css", "/leaflet/leaflet.js", "/staatliches-latin.woff2"]) {
      const r = await fetch(tvBase + p);
      assert.equal(r.status, 200, p);
    }
    assert.match(await (await fetch(tvBase + "/tv")).text(), /Wanderstamp TV/);
    for (const p of ["/", "/index.html", "/app.js", "/api/map", "/api/holidays", "/api/me", "/share/abc", "/api/share/abc", "/api/tv/devices", "/api/export", "/sw.js"]) {
      const r = await fetch(tvBase + p);
      assert.equal(r.status, 404, `GET ${p} on the door`);
    }
    for (const p of ["/api/login", "/api/tv/pair", "/api/pins"]) {
      const r = await fetch(tvBase + p, { method: "POST", headers: { "Content-Type": "application/json" }, body: "{}" });
      assert.ok(r.status === 404 || r.status === 405, `POST ${p} on the door: ${r.status}`);
    }
    const me = await fetch(tvBase + "/api/tv/me");
    assert.equal(me.status, 401, "no token, not paired");
    const csp = (await fetch(tvBase + "/tv")).headers.get("content-security-policy") || "";
    assert.match(csp, /server\.arcgisonline\.com/, "tiles allowed by the CSP");
  }],

  ["pair from the phone → shelf with the trips (no planned ones) → journey → unpair → code again", async ({ browser, base, tvBase }) => {
    const { ctx: pctx, page: phone } = await signedIn(browser, base);
    const { trip, planned } = await seed(phone);
    assert.equal(planned.planned, true, "the future trip is planned");
    const { ctx, tv, errors, device } = await pairTV(browser, base, tvBase, phone);
    await tv.waitForFunction(() => document.querySelectorAll(".tripstamp").length >= 1);
    const names = await tv.locator(".tripstamp .band").allInnerTexts();
    assert.ok(names.some((n) => /TV Lakes <b>bold<\/b>/i.test(n)), `trip name shown as text: ${names}`);
    assert.ok(!names.some((n) => /Future/i.test(n)), "a planned trip is not on the TV");
    // the pairing cookie is HttpOnly: the page itself can't read it
    assert.ok(!(await tv.evaluate(() => document.cookie)).includes("ws_tv"));
    // play the seeded trip
    const idx = await tv.evaluate((id) => [...document.querySelectorAll(".tripstamp")].findIndex((s) => s.dataset.id === String(id)), trip.id);
    for (let i = 0; i < idx; i++) await tv.keyboard.press("ArrowRight");
    await tv.keyboard.press("Enter");
    await waitScreen(tv, "journey");
    assert.match((await tv.locator("#j-stop").innerText()).toLowerCase(), /keswick/);
    await tv.keyboard.press("ArrowDown");
    assert.match((await tv.locator("#j-stop").innerText()).toLowerCase(), /buttermere/);
    await tv.keyboard.press("Escape");
    await waitScreen(tv, "shelf");
    // the phone lists the TV, then unpairs it
    const list = await api(phone, "GET", "/api/tv/devices");
    assert.ok(list.body.some((d) => d.id === device.id && d.name === "Living room" && d.last_seen), JSON.stringify(list.body));
    assert.equal((await api(phone, "DELETE", `/api/tv/devices/${device.id}`)).status, 204);
    assert.equal(await tv.evaluate(async () => (await fetch("/api/tv/me")).status), 401, "token dead at once");
    await tv.reload();
    await waitScreen(tv, "pair");
    assert.deepEqual(errors, []);
    await ctx.close();
    await pctx.close();
  }],

  ["a paired TV reads previews only; a bearer token works without the cookie", async ({ browser, base, tvBase }) => {
    const { ctx: pctx, page: phone } = await signedIn(browser, base);
    await seed(phone);
    const { ctx, tv, device } = await pairTV(browser, base, tvBase, phone, "Bedroom");
    const st = await tv.evaluate(async () => ({
      orig: (await fetch("/api/tv/photo/00000000-0000-0000-0000-000000000000/original")).status,
      video: (await fetch("/api/tv/photo/00000000-0000-0000-0000-000000000000/video")).status,
      unknown: (await fetch("/api/tv/photo/00000000-0000-0000-0000-000000000000/preview")).status,
      bad: (await fetch("/api/tv/photo/..%2f..%2fetc/preview")).status,
      trip404: (await fetch("/api/tv/trips/999999")).status,
    }));
    assert.equal(st.orig, 403);
    assert.equal(st.video, 403);
    assert.equal(st.unknown, 404, "an asset on no shown trip");
    assert.ok(st.bad === 400 || st.bad === 404, `bad asset id: ${st.bad}`);
    assert.equal(st.trip404, 404);
    await api(phone, "DELETE", `/api/tv/devices/${device.id}`);
    await ctx.close();
    await pctx.close();
  }],

  ["a wrong or reused code pairs nothing", async ({ browser, base, tvBase }) => {
    const { ctx: pctx, page: phone } = await signedIn(browser, base);
    const bad = await api(phone, "POST", "/api/tv/pair", { code: "ZZZZ", name: "Nope" });
    assert.ok(bad.status === 404 || bad.status === 429, `wrong code: ${bad.status}`);
    const { ctx, device, code } = await pairTV(browser, base, tvBase, phone, "Kitchen");
    // the same code a second time: gone once used
    const again = await api(phone, "POST", "/api/tv/pair", { code, name: "Kitchen 2" });
    assert.notEqual(again.status, 201, "a used code can't pair a second TV");
    // a TV page showing a code, and a blank name for it
    const ctx2 = await tvContext(browser);
    const tv2 = await ctx2.newPage();
    await tv2.goto(tvBase + "/tv");
    await tv2.waitForFunction(() => document.querySelectorAll("#pair-code span").length === 4);
    const code2 = (await tv2.locator("#pair-code").innerText()).replace(/\s/g, "");
    const blank = await api(phone, "POST", "/api/tv/pair", { code: code2, name: "   " });
    assert.equal(blank.status, 400, "blank name");
    const long = await api(phone, "POST", "/api/tv/pair", { code: code2, name: "x".repeat(41) });
    assert.equal(long.status, 400, "41-char name");
    await ctx2.close();
    await api(phone, "DELETE", `/api/tv/devices/${device.id}`);
    await ctx.close();
    await pctx.close();
  }],

  ["Show on TV in the account page pairs a real TV page", async ({ browser, base, tvBase }) => {
    const { ctx: pctx, page: phone } = await signedIn(browser, base);
    const ctx = await tvContext(browser);
    const tv = await ctx.newPage();
    await tv.goto(tvBase + "/tv");
    await tv.waitForFunction(() => document.querySelectorAll("#pair-code span").length === 4);
    const code = (await tv.locator("#pair-code").innerText()).replace(/\s/g, "");
    await phone.click("#btn-account");
    await phone.fill("#tv-code", code.toLowerCase().split("").join(" "));
    await phone.fill("#tv-name", "Den");
    await phone.click("#tv-pair button[type=submit]");
    await phone.waitForSelector("#tv-list .tv-item");
    assert.match(await phone.locator("#tv-list").innerText(), /Den/);
    await waitScreen(tv, "shelf", 40000);
    phone.on("dialog", (d) => d.accept());
    await phone.locator("#tv-list .tv-item", { hasText: "Den" }).locator("button").click();
    await phone.waitForFunction(() => !/Den/.test(document.querySelector("#tv-list").innerText));
    assert.equal(await tv.evaluate(async () => (await fetch("/api/tv/me")).status), 401);
    await ctx.close();
    await pctx.close();
  }],
];
