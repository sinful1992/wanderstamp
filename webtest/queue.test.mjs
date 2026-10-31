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
  return { ctx, page };
}

const queued = (page) => page.evaluate(() => JSON.parse(localStorage.getItem(QUEUE_KEY) || "[]").length);

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
];
