// B2: an offline pin waits in the queue through server errors, and only a real "no" drops it.
import assert from "node:assert/strict";
import { signedIn, api } from "./harness.mjs";

async function setup(browser, base) {
  const { ctx, page } = await signedIn(browser, base);
  let h = (await api(page, "GET", "/api/holidays")).body.find((x) => x.name === "Queue B2");
  if (!h) h = (await api(page, "POST", "/api/holidays", { name: "Queue B2", color: "#2e7d4f", start_at: "2025-03-01", end_at: "2025-03-03" })).body;
  await page.evaluate((id) => {
    localStorage.setItem(QUEUE_KEY, JSON.stringify([{ qid: "t1", holiday_id: id, lat: 51.5, lng: -0.1, title: "queued B2", note: "" }]));
  }, h.id);
  return { ctx, page, h };
}

const queued = (page) => page.evaluate(() => JSON.parse(localStorage.getItem(QUEUE_KEY) || "[]").length);

// WA-2 helpers. lostOnce: the first POST reaches the server, its answer never arrives.
async function lostOnce(page) {
  const bodies = [];
  await page.route("**/api/pins", async (r) => {
    if (r.request().method() !== "POST") return r.continue();
    bodies.push(r.request().postDataJSON());
    if (bodies.length === 1) { await r.fetch(); return r.abort("failed"); }
    return r.continue();
  });
  return bodies;
}
const retitle = (page, t) => page.evaluate((t) => {
  const q = JSON.parse(localStorage.getItem(QUEUE_KEY));
  q[0].title = t + " " + Date.now();
  q[0].qid = "k" + Date.now();
  localStorage.setItem(QUEUE_KEY, JSON.stringify(q));
  return q[0].title;
}, t);
const titled = async (page, t) => (await api(page, "GET", "/api/map")).body.pins.filter((p) => p.title === t).length;

export const tests = [
  ...[502, 503, 500, 429].map((code) => [`HTTP ${code} keeps the queued pin`, async ({ browser, base }) => {
    const { ctx, page } = await setup(browser, base);
    await page.route("**/api/pins", (r) => r.request().method() === "POST" ? r.fulfill({ status: code, contentType: "text/html", body: "<h1>Bad gateway</h1>" }) : r.continue());
    await page.evaluate(() => syncQueue());
    assert.equal(await queued(page), 1);
    // the server comes back: the pin goes through
    await page.unroute("**/api/pins");
    await page.evaluate(() => syncQueue());
    assert.equal(await queued(page), 0);
    const pins = (await api(page, "GET", "/api/map")).body.pins.filter((p) => p.title === "queued B2");
    assert.ok(pins.length >= 1);
    await ctx.close();
  }]),
  ["a network error keeps it", async ({ browser, base }) => {
    const { ctx, page } = await setup(browser, base);
    await page.route("**/api/pins", (r) => r.request().method() === "POST" ? r.abort("internetdisconnected") : r.continue());
    await page.evaluate(() => syncQueue());
    assert.equal(await queued(page), 1);
    await ctx.close();
  }],
  ["a 400 (trip gone) drops it", async ({ browser, base }) => {
    const { ctx, page } = await setup(browser, base);
    await page.route("**/api/pins", (r) => r.request().method() === "POST" ? r.fulfill({ status: 400, contentType: "application/json", body: '{"error":"no such trip"}' }) : r.continue());
    await page.evaluate(() => syncQueue());
    assert.equal(await queued(page), 0);
    await ctx.close();
  }],
  ["a pin that keeps getting a 5xx doesn't block the pins behind it", async ({ browser, base }) => {
    const { ctx, page } = await setup(browser, base);
    await page.evaluate(() => {
      const q = JSON.parse(localStorage.getItem(QUEUE_KEY));
      q.push({ ...q[0], qid: "t2", title: "behind B2" });
      q[0].title = "stuck B2";
      localStorage.setItem(QUEUE_KEY, JSON.stringify(q));
    });
    await page.route("**/api/pins", (r) => r.request().method() === "POST" && JSON.parse(r.request().postData()).title === "stuck B2"
      ? r.fulfill({ status: 500, contentType: "text/plain", body: "boom" }) : r.continue());
    await page.evaluate(() => syncQueue());
    const left = await page.evaluate(() => JSON.parse(localStorage.getItem(QUEUE_KEY)).map((p) => p.title));
    assert.deepEqual(left, ["stuck B2"]);
    const pins = (await api(page, "GET", "/api/map")).body.pins.filter((p) => p.title === "behind B2");
    assert.ok(pins.length >= 1);
    await ctx.close();
  }],
  ["a pin queued while a sync runs stays queued", async ({ browser, base }) => {
    const { ctx, page } = await setup(browser, base);
    let release;
    const gate = new Promise((r) => { release = r; });
    await page.route("**/api/pins", async (r) => { if (r.request().method() === "POST") await gate; r.continue(); });
    const run = page.evaluate(() => syncQueue());
    await page.waitForTimeout(200);
    await page.evaluate(() => {
      const q = JSON.parse(localStorage.getItem(QUEUE_KEY));
      q.push({ ...q[0], qid: "late", title: "late B2" });
      localStorage.setItem(QUEUE_KEY, JSON.stringify(q));
    });
    release();
    await run;
    const left = await page.evaluate(() => JSON.parse(localStorage.getItem(QUEUE_KEY)).map((p) => p.qid));
    assert.deepEqual(left, ["late"]);
    await ctx.close();
  }],
  ["a pin discarded while the sync is sending another one isn't sent", async ({ browser, base }) => {
    const { ctx, page } = await setup(browser, base);
    await page.evaluate(() => {
      const q = JSON.parse(localStorage.getItem(QUEUE_KEY));
      q.push({ ...q[0], qid: "gone", title: "discarded B2" });
      localStorage.setItem(QUEUE_KEY, JSON.stringify(q));
    });
    let release;
    const gate = new Promise((r) => { release = r; });
    const sent = [];
    await page.route("**/api/pins", async (r) => {
      if (r.request().method() === "POST") { sent.push(JSON.parse(r.request().postData()).title); await gate; }
      r.continue();
    });
    const run = page.evaluate(() => syncQueue());
    await page.waitForTimeout(200);
    await page.evaluate(() => localStorage.setItem(QUEUE_KEY, JSON.stringify(JSON.parse(localStorage.getItem(QUEUE_KEY)).filter((p) => p.qid !== "gone"))));
    release();
    await run;
    assert.deepEqual(sent, ["queued B2"]);
    await ctx.close();
  }],
  // ---- WA-2 (audit 137): the queue id is the server's client_id, so a pin sent twice is stored once ----
  ["WA-2: a queued pin whose answer was lost is stored once", async ({ browser, base }) => {
    const { ctx, page } = await setup(browser, base);
    const title = await retitle(page, "lost B2");
    await lostOnce(page);
    await page.evaluate(() => syncQueue());
    assert.equal(await queued(page), 1, "the answer was lost: the pin waits");
    await page.evaluate(() => syncQueue());
    assert.equal(await queued(page), 0);
    assert.equal(await titled(page, title), 1);
    await ctx.close();
  }],
  ["WA-2: a pin placed online whose answer was lost is queued under the id it was sent with, and stored once", async ({ browser, base }) => {
    const { ctx, page, h } = await setup(browser, base);
    await page.evaluate(() => localStorage.setItem(QUEUE_KEY, "[]"));
    const title = "online lost B2 " + Date.now();
    const bodies = await lostOnce(page);
    await page.evaluate(() => placePin(map.getCenter()));
    await page.selectOption("form.pop-form select", String(h.id));
    await page.fill("form.pop-form input[type=text]", title);
    await page.click("form.pop-form button[type=submit]");
    for (let i = 0; i < 100 && !(await queued(page)); i++) await page.waitForTimeout(50);
    const q = await page.evaluate(() => JSON.parse(localStorage.getItem(QUEUE_KEY)));
    assert.equal(q.length, 1);
    assert.ok(bodies[0].client_id, "the first post carries a client_id");
    assert.equal(q[0].qid, bodies[0].client_id);
    assert.equal(q[0].client_id, undefined);
    await page.evaluate(() => syncQueue());
    assert.equal(await queued(page), 0);
    assert.equal(bodies[1].client_id, bodies[0].client_id);
    assert.equal(await titled(page, title), 1);
    await ctx.close();
  }],
  ["WA-2: ids already waiting on a phone are sent as they are; one the server would refuse goes without a key", async ({ browser, base }) => {
    const { ctx, page } = await setup(browser, base);
    await page.evaluate(() => {
      const one = JSON.parse(localStorage.getItem(QUEUE_KEY))[0];
      const { qid, ...bare } = one;
      localStorage.setItem(QUEUE_KEY, JSON.stringify([
        { ...one, qid: "1794298543932:k3x9af", title: "old id" },
        { ...one, qid: "bad id!", title: "bad id" },
        { ...one, qid: "x".repeat(65), title: "long id" },
        { ...bare, title: "no id" },
        { ...one, qid: 12345, title: "number id" },
      ]));
    });
    const got = {};
    await page.route("**/api/pins", (r) => {
      if (r.request().method() !== "POST") return r.continue();
      const b = r.request().postDataJSON();
      got[b.title] = b;
      return r.fulfill({ status: 201, contentType: "application/json", body: "{}" });
    });
    await page.evaluate(() => syncQueue());
    assert.equal(await queued(page), 0);
    assert.equal(got["old id"].client_id, "1794298543932:k3x9af");
    assert.equal("client_id" in got["bad id"], false);
    assert.equal("client_id" in got["long id"], false);
    assert.equal("client_id" in got["no id"], false);
    assert.equal(got["number id"].client_id, "12345");
    for (const b of Object.values(got)) assert.equal("qid" in b, false);
    await ctx.close();
  }],
  ["WA-2: a 410 (saved earlier, deleted since) drops the queued pin without a word; a 400 still says so", async ({ browser, base }) => {
    const { ctx, page } = await setup(browser, base);
    await page.evaluate(() => { document.getElementById("toast").hidden = true; document.getElementById("toast").textContent = ""; });
    await page.route("**/api/pins", (r) => r.request().method() === "POST" ? r.fulfill({ status: 410, contentType: "application/json", body: '{"error":"pin was deleted"}' }) : r.continue());
    await page.evaluate(() => syncQueue());
    assert.equal(await queued(page), 0);
    assert.equal(await page.textContent("#toast"), "");
    await page.unroute("**/api/pins");
    await page.evaluate(() => localStorage.setItem(QUEUE_KEY, JSON.stringify([{ qid: "t9", holiday_id: 999999, lat: 1, lng: 1, title: "nowhere", note: "" }])));
    await page.route("**/api/pins", (r) => r.request().method() === "POST" ? r.fulfill({ status: 400, contentType: "application/json", body: '{"error":"no such trip"}' }) : r.continue());
    await page.evaluate(() => syncQueue());
    assert.equal(await page.textContent("#toast"), "1 rejected by the server");
    await ctx.close();
  }],
  ["WA-2: a second tap on Add pin after a server error sends the same client_id", async ({ browser, base }) => {
    const { ctx, page, h } = await setup(browser, base);
    await page.evaluate(() => localStorage.setItem(QUEUE_KEY, "[]"));
    const bodies = [];
    await page.route("**/api/pins", (r) => {
      if (r.request().method() !== "POST") return r.continue();
      bodies.push(r.request().postDataJSON());
      return r.fulfill({ status: 502, contentType: "text/html", body: "<h1>Bad gateway</h1>" });
    });
    await page.evaluate(() => placePin(map.getCenter()));
    await page.selectOption("form.pop-form select", String(h.id));
    await page.fill("form.pop-form input[type=text]", "twice B2");
    for (let n = 1; n <= 2; n++) {
      await page.click("form.pop-form button[type=submit]");
      for (let i = 0; i < 100 && bodies.length < n; i++) await page.waitForTimeout(50);
      await page.waitForSelector("form.pop-form button[type=submit]:not([disabled])");
    }
    assert.equal(bodies.length, 2);
    assert.match(bodies[0].client_id, /^[A-Za-z0-9_:.-]{1,64}$/);
    assert.equal(bodies[1].client_id, bodies[0].client_id);
    assert.equal(await queued(page), 0, "a 502 on the online path is an error, not a queued pin");
    // another form is another pin
    await page.evaluate(() => { map.closePopup(); placePin(map.getCenter()); });
    await page.fill("form.pop-form input[type=text]", "other B2");
    await page.click("form.pop-form button[type=submit]");
    for (let i = 0; i < 100 && bodies.length < 3; i++) await page.waitForTimeout(50);
    assert.notEqual(bodies[2].client_id, bodies[0].client_id);
    await ctx.close();
  }],
];
