// B4: a background reload doesn't wipe a form being typed in. B7/D4: plurals, no zero counts.
import assert from "node:assert/strict";
import { signedIn, api } from "./harness.mjs";

async function trip(page, name, extra = {}) {
  let h = (await api(page, "GET", "/api/holidays")).body.find((x) => x.name === name);
  if (!h) h = (await api(page, "POST", "/api/holidays", { name, color: "#1d6fb8", start_at: "2023-08-01", end_at: "2023-08-03", ...extra })).body;
  return h;
}

export const tests = [
  ["B4: typing in the trip form survives a reload, and saving still refreshes", async ({ browser, base }) => {
    const { ctx, page } = await signedIn(browser, base);
    const h = await trip(page, "Typing B4");
    await page.reload({ waitUntil: "networkidle" });
    await page.evaluate((id) => {
      setSheet(true);
      const row = [...document.querySelectorAll("#trip-list .trip-row")].find((r) => r.textContent.includes("Typing B4"));
      row.querySelector("button[title='Edit this trip']").click();
    }, h.id);
    await page.waitForSelector("form.trip-edit");
    await page.fill("form.trip-edit textarea", "half a sentence about the beach");
    await page.evaluate(() => loadData()); // what a photo sync or reconnect does
    assert.equal(await page.$eval("form.trip-edit textarea", (t) => t.value), "half a sentence about the beach");
    // save: the form goes and the list shows fresh data again
    await page.click("form.trip-edit button[type=submit]");
    await page.waitForFunction(() => !document.querySelector("form.trip-edit"), null, { timeout: 3000 });
    const j = (await api(page, "GET", "/api/holidays")).body.find((x) => x.id === h.id).journal;
    assert.equal(j, "half a sentence about the beach");
    await ctx.close();
  }],
  ["B4: a save that fails keeps the form dirty, so a reload doesn't wipe it", async ({ browser, base }) => {
    const { ctx, page } = await signedIn(browser, base);
    const h = await trip(page, "Failing B4");
    await page.reload({ waitUntil: "networkidle" });
    await page.evaluate(() => {
      setSheet(true);
      const row = [...document.querySelectorAll("#trip-list .trip-row")].find((r) => r.textContent.includes("Failing B4"));
      row.querySelector("button[title='Edit this trip']").click();
    });
    await page.waitForSelector("form.trip-edit");
    await page.fill("form.trip-edit textarea", "words that must not be lost");
    await page.route(`**/api/holidays/${h.id}`, (r) => ["PUT", "PATCH"].includes(r.request().method())
      ? r.fulfill({ status: 500, contentType: "text/plain", body: "boom" }) : r.continue());
    await page.click("form.trip-edit button[type=submit]");
    await page.waitForFunction(() => !document.querySelector("form.trip-edit button[type=submit]").disabled, null, { timeout: 3000 });
    await page.evaluate(() => loadData());
    assert.equal(await page.$eval("form.trip-edit textarea", (t) => t.value), "words that must not be lost");
    await ctx.close();
  }],
  ["B4: an untouched open form is still refreshed", async ({ browser, base }) => {
    const { ctx, page } = await signedIn(browser, base);
    await page.evaluate(() => { document.querySelectorAll("form[data-dirty]").forEach((f) => delete f.dataset.dirty); });
    const n = await page.evaluate(() => editing("trip-list"));
    assert.equal(n, false);
    await ctx.close();
  }],
  ["B7/D4: 1 pin, no '0 photos', and the pill says '3 trips'", async ({ browser, base }) => {
    const { ctx, page } = await signedIn(browser, base);
    const h = await trip(page, "Plural B7");
    await api(page, "POST", "/api/pins", { holiday_id: h.id, lat: 50.1, lng: -5.5, title: "one", note: "", created_at: "2023-08-02T10:00:00Z" });
    await page.reload({ waitUntil: "networkidle" });
    const meta = await page.evaluate(() => [...document.querySelectorAll(".trip-row")].find((r) => r.textContent.includes("Plural B7")).querySelector(".trip-meta").textContent);
    assert.match(meta, / · 1 pin$/);
    assert.doesNotMatch(meta, /0 photos|1 pins/);
    const pill = await page.$eval("#pill-label", (e) => e.textContent);
    const n = (await api(page, "GET", "/api/holidays")).body.length;
    assert.equal(pill, `${n} ${n === 1 ? "trip" : "trips"}`);
    assert.equal(await page.evaluate(() => [plural(1, "photo"), plural(0, "photo"), plural(2, "country", "countries")].join("|")), "1 photo|0 photos|2 countries");
    await ctx.close();
  }],
];
