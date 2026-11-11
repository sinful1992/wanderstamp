// WA-4 (audit 137, team task 142): a session that simply expired must not leave
// the last user's whole map in localStorage (or drawn behind the sign-in cover).
// A boot with no network still opens the saved map.
import assert from "node:assert/strict";
import { signedIn, api } from "./harness.mjs";

async function withPin(page) {
  let h = (await api(page, "GET", "/api/holidays")).body.find((x) => x.name === "WA4 trip");
  if (!h) h = (await api(page, "POST", "/api/holidays", { name: "WA4 trip", color: "#4f7d2e", start_at: "2025-06-01", end_at: "2025-06-03" })).body;
  await api(page, "POST", "/api/pins", { holiday_id: h.id, lat: 40, lng: 3, title: "wa4 " + Date.now(), note: "" });
  await page.reload({ waitUntil: "networkidle" });
  await page.waitForFunction(() => { const s = JSON.parse(localStorage.getItem(SNAP_KEY) || "null"); return s && s.pins.length > 0; });
}

const pinMarkers = (page) => page.evaluate(() => [...state.markers.keys()].filter((k) => k.startsWith("p") || k.startsWith("c")).length);

export const tests = [
  ["WA-4: an expired session forgets the saved map", async ({ browser, base }) => {
    const { ctx, page } = await signedIn(browser, base);
    await withPin(page);
    await ctx.clearCookies(); // the session ran out
    await page.reload({ waitUntil: "networkidle" });
    await page.waitForSelector("#login:not([hidden])");
    assert.equal(await page.evaluate(() => localStorage.getItem(SNAP_KEY)), null, "the last user's map is still saved");
    assert.equal(await pinMarkers(page), 0, "the last user's pins are still drawn");
    assert.equal(await page.evaluate(() => state.holidays.length), 0);
    await ctx.close();
  }],
  ["WA-4: a boot with no network still shows the saved map", async ({ browser, base }) => {
    const { ctx, page } = await signedIn(browser, base);
    await withPin(page);
    await page.route("**/api/map", (r) => r.abort("internetdisconnected"));
    await page.reload({ waitUntil: "networkidle" });
    assert.ok(await page.evaluate(() => localStorage.getItem(SNAP_KEY)), "snapshot removed on a network error");
    assert.ok((await pinMarkers(page)) > 0, "the saved map is not drawn offline");
    await ctx.close();
  }],
  ["WA-4: a server error at boot keeps the saved map", async ({ browser, base }) => {
    const { ctx, page } = await signedIn(browser, base);
    await withPin(page);
    await page.route("**/api/map", (r) => r.fulfill({ status: 502, contentType: "text/html", body: "<h1>Bad gateway</h1>" }));
    await page.reload({ waitUntil: "networkidle" });
    assert.ok(await page.evaluate(() => localStorage.getItem(SNAP_KEY)), "snapshot removed on a 502");
    await ctx.close();
  }],
];
