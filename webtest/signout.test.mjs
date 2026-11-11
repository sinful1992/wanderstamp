// WA-3 (audit 137, team task 142): pins queued offline belong to the person who
// placed them. Signing out must not hand them to whoever signs in next on the
// same device (they were sent as that person, into a trip they may not see,
// and dropped for good).
import assert from "node:assert/strict";
import { signedIn, api, USER, PASS } from "./harness.mjs";

let n = 0;
// A second user that has already chosen their own password (no 428 cover).
async function otherUser(page) {
  const name = `wa3o${Date.now()}${n++}`;
  const tmp = "wa3-temp-pass-1234", pass = "wa3-own-pass-5678";
  assert.equal((await api(page, "POST", "/api/users", { username: name, password: tmp })).status, 201);
  return { name, tmp, pass };
}

// Switch the signed-in session without the app's own sign-out.
async function become(page, user, pass, newPass) {
  await api(page, "POST", "/api/logout", {});
  const r = await api(page, "POST", "/api/login", { username: user, password: pass });
  assert.equal(r.status, 200, `login ${user}: ${JSON.stringify(r.body)}`);
  if (newPass) assert.equal((await api(page, "POST", "/api/password", { current_password: pass, new_password: newPass })).status, 200);
}

async function trip(page, name) {
  let h = (await api(page, "GET", "/api/holidays")).body.find((x) => x.name === name);
  if (!h) h = (await api(page, "POST", "/api/holidays", { name, color: "#7d2e4f", start_at: "2025-04-01", end_at: "2025-04-03" })).body;
  return h;
}

const queue = (page) => page.evaluate(() => JSON.parse(localStorage.getItem(QUEUE_KEY) || "[]"));
const queueOne = (page, holiday_id, title) =>
  page.evaluate(([id, t]) => queuePin({ holiday_id: id, lat: 48.85, lng: 2.35, title: t, note: "", created_at: new Date().toISOString() }), [holiday_id, title]);

// Sign out through the app with the network down for pins, so nothing can flush.
async function signOutOffline(page) {
  await page.route("**/api/pins", (r) => r.request().method() === "POST" ? r.abort("internetdisconnected") : r.continue());
  await Promise.all([page.waitForEvent("load"), page.evaluate(() => signOut())]);
  await page.waitForSelector("#login:not([hidden])");
  await page.unroute("**/api/pins");
}

function watchPinPosts(page) {
  const titles = [];
  page.on("request", (r) => {
    if (r.method() === "POST" && r.url().endsWith("/api/pins")) titles.push((r.postDataJSON() || {}).title);
  });
  return titles;
}

export const tests = [
  ["WA-3: a queued pin is not sent as the next user after sign-out", async ({ browser, base }) => {
    const { ctx, page } = await signedIn(browser, base);
    const h = await trip(page, "WA3 trip");
    const other = await otherUser(page);
    const title = "wa3 mine " + Date.now();
    await queueOne(page, h.id, title);
    await signOutOffline(page);
    const posts = watchPinPosts(page);
    await become(page, other.name, other.tmp, other.pass);
    await page.reload({ waitUntil: "networkidle" });
    await page.waitForFunction(() => state.me && state.me.username);
    await page.evaluate(() => syncQueue());
    await page.waitForLoadState("networkidle");
    assert.deepEqual(posts.filter((t) => t === title), [], "the pin was sent as the other user");
    assert.equal((await queue(page)).filter((q) => q.title === title).length, 1, "the pin was dropped from the queue");
    // and it is not drawn on the other user's map
    const ghosts = await page.evaluate(() => [...state.markers.keys()].filter((k) => k.startsWith("q")).length);
    assert.equal(ghosts, 0, "the other user sees the previous user's waiting pin");
    await ctx.close();
  }],
  ["WA-3: the owner signing in again sends it", async ({ browser, base }) => {
    const { ctx, page } = await signedIn(browser, base);
    const h = await trip(page, "WA3 trip");
    const other = await otherUser(page);
    const title = "wa3 back " + Date.now();
    await queueOne(page, h.id, title);
    await signOutOffline(page);
    await become(page, other.name, other.tmp, other.pass);
    await page.reload({ waitUntil: "networkidle" });
    await page.waitForFunction(() => state.me && state.me.username);
    await become(page, USER, PASS);
    await page.reload({ waitUntil: "networkidle" });
    await page.waitForFunction(() => state.me && state.me.username === "tester");
    await page.waitForFunction((t) => !JSON.parse(localStorage.getItem(QUEUE_KEY) || "[]").some((q) => q.title === t), title);
    const pins = (await api(page, "GET", "/api/map")).body.pins.filter((p) => p.title === title);
    assert.equal(pins.length, 1);
    await ctx.close();
  }],
  ["WA-3: sign-out with signal sends waiting pins first", async ({ browser, base }) => {
    const { ctx, page } = await signedIn(browser, base);
    const h = await trip(page, "WA3 trip");
    const title = "wa3 flush " + Date.now();
    await queueOne(page, h.id, title);
    await Promise.all([page.waitForEvent("load"), page.evaluate(() => signOut())]);
    await page.waitForSelector("#login:not([hidden])");
    assert.equal((await queue(page)).filter((q) => q.title === title).length, 0, "still queued after an online sign-out");
    await become(page, USER, PASS);
    assert.equal((await api(page, "GET", "/api/map")).body.pins.filter((p) => p.title === title).length, 1);
    await ctx.close();
  }],
  ["WA-3: a pin queued before this fix (no owner) still goes", async ({ browser, base }) => {
    const { ctx, page } = await signedIn(browser, base);
    const h = await trip(page, "WA3 trip");
    const title = "wa3 legacy " + Date.now();
    await page.evaluate(([id, t]) => localStorage.setItem(QUEUE_KEY, JSON.stringify([{ qid: "legacy" + Date.now(), holiday_id: id, lat: 1, lng: 1, title: t, note: "" }])), [h.id, title]);
    await page.evaluate(() => syncQueue());
    assert.equal((await queue(page)).length, 0);
    assert.equal((await api(page, "GET", "/api/map")).body.pins.filter((p) => p.title === title).length, 1);
    await ctx.close();
  }],
  ["WA-3: a pin queued on an offline boot belongs to the snapshot's user", async ({ browser, base }) => {
    const { ctx, page } = await signedIn(browser, base);
    const h = await trip(page, "WA3 trip");
    await page.waitForFunction(() => localStorage.getItem(SNAP_KEY));
    // an offline boot: the snapshot is drawn, /api/map never answered
    await page.evaluate(() => { state.me = null; restoreSnapshot(); });
    const title = "wa3 offboot " + Date.now();
    await queueOne(page, h.id, title);
    const q = (await queue(page)).find((x) => x.title === title);
    assert.equal(q.by, "tester", "queued pin carries no owner");
    await ctx.close();
  }],
  ["WA-3: a map answer landing after sign-out began does not save the map again (linus #10249)", async ({ browser, base }) => {
    const { ctx, page } = await signedIn(browser, base);
    const h = await trip(page, "WA3 trip");
    await queueOne(page, h.id, "wa3 race " + Date.now());
    let held = null;
    // a map load already in flight when sign-out starts (the refetch after a flush, or the
    // reload after a photo sync) waits here; sign-out itself never finishes
    await page.route("**/api/map", (r) => { if (!held) held = r; else r.continue(); });
    await page.route("**/api/logout", () => {});
    await page.evaluate(() => { loadData().catch(() => {}); });
    for (let i = 0; i < 200 && !held; i++) await page.waitForTimeout(25);
    assert.ok(held, "the map load did not start");
    await page.evaluate(() => { signOut(); });
    await page.waitForTimeout(300);
    // sign-out has already forgotten the map by the time the answer lands
    await page.evaluate(() => localStorage.removeItem(SNAP_KEY));
    await held.fulfill({ response: await held.fetch() });
    await page.waitForTimeout(500);
    assert.equal(await page.evaluate(() => localStorage.getItem(SNAP_KEY)), null, "the signed-out user's map was saved again");
    await ctx.close();
  }],
  ["WA-3: sign-out with an already expired session still forgets the map (linus #10262)", async ({ browser, base }) => {
    const { ctx, page } = await signedIn(browser, base);
    await page.waitForFunction(() => localStorage.getItem(SNAP_KEY));
    await page.route("**/api/logout", (r) => r.fulfill({ status: 401, contentType: "application/json", body: '{"error":"not logged in"}' }));
    await Promise.all([page.waitForEvent("load"), page.evaluate(() => signOut().catch(() => {}))]);
    assert.equal(await page.evaluate(() => localStorage.getItem(SNAP_KEY)), null, "the map stayed after signing out of an expired session");
    await ctx.close();
  }],
];
