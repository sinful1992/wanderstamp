// WS-7 (audit 137, team task 142): at low zoom the world repeats; a click on a
// second copy gives a longitude past ±180, which the server refuses. The pin
// must be saved at the same place on the one world (-180..180).
import assert from "node:assert/strict";
import { signedIn, api } from "./harness.mjs";

async function trip(page) {
  let h = (await api(page, "GET", "/api/holidays")).body.find((x) => x.name === "WS7 trip");
  if (!h) h = (await api(page, "POST", "/api/holidays", { name: "WS7 trip", color: "#2e4f7d", start_at: "2025-05-01", end_at: "2025-05-03" })).body;
  await page.evaluate(() => loadData()); // the page learns of a trip made behind its back
  return h;
}

async function place(page, h, lat, lng, title) {
  // the popup opens where the tap was, which on a far world copy may be off
  // screen in a headless window: fill and send the form directly
  const shown = await page.evaluate(([a, b, id, t]) => {
    map.closePopup();
    placePin(L.latLng(a, b));
    const f = document.querySelector("form.pop-form");
    f.querySelector("select").value = String(id);
    f.querySelector("input[type=text]").value = t;
    const text = f.querySelector(".pop-coords").textContent;
    f.querySelector("button[type=submit]").click();
    return text;
  }, [lat, lng, h.id, title]);
  let pin;
  for (let i = 0; i < 100 && !pin; i++) {
    pin = (await api(page, "GET", "/api/map")).body.pins.find((p) => p.title === title);
    if (!pin) await page.waitForTimeout(50);
  }
  return { pin, shown };
}

const near = (a, b) => Math.abs(a - b) < 1e-6;

export const tests = [
  ...[[359.9, -0.1], [-190, 170], [540.5, -179.5], [12.5, 12.5]].map(([raw, want]) => [`WS-7: a click at lng ${raw} saves the pin at ${want}`, async ({ browser, base }) => {
    const { ctx, page } = await signedIn(browser, base);
    const h = await trip(page);
    const title = `ws7 ${raw} ${Date.now()}`;
    const errs = [];
    page.on("response", (r) => { if (r.url().endsWith("/api/pins") && r.status() >= 400) errs.push(r.status()); });
    const { pin, shown } = await place(page, h, 51.5, raw, title);
    assert.deepEqual(errs, [], "the server refused the pin");
    assert.ok(pin, "pin not stored");
    assert.ok(near(pin.lng, want), `stored lng ${pin.lng}, want ${want}`);
    assert.ok(near(pin.lat, 51.5));
    assert.equal(shown, `51.50000, ${want.toFixed(5)}`, "the coordinates shown are not the ones saved");
    await ctx.close();
  }]),
  ["WS-7: an offline pin from a second world copy is queued on the one world", async ({ browser, base }) => {
    const { ctx, page } = await signedIn(browser, base);
    const h = await trip(page);
    const title = "ws7 offline " + Date.now();
    await page.route("**/api/pins", (r) => r.request().method() === "POST" ? r.abort("internetdisconnected") : r.continue());
    await place(page, h, 10, 370, title);
    let q = [];
    for (let i = 0; i < 100 && !q.length; i++) {
      q = await page.evaluate((t) => JSON.parse(localStorage.getItem(QUEUE_KEY) || "[]").filter((x) => x.title === t), title);
      if (!q.length) await page.waitForTimeout(50);
    }
    assert.equal(q.length, 1);
    assert.ok(near(q[0].lng, 10), `queued lng ${q[0].lng}`);
    await page.unroute("**/api/pins");
    await page.evaluate(() => syncQueue());
    const pin = (await api(page, "GET", "/api/map")).body.pins.find((p) => p.title === title);
    assert.ok(pin && near(pin.lng, 10), "the synced pin is not at lng 10");
    await ctx.close();
  }],
];
