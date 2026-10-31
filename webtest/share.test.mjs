// S5 client: a share link has a lifetime, and replacing one asks first.
// S2 client: a share page never links the original file.
import assert from "node:assert/strict";
import { signedIn, api } from "./harness.mjs";

async function openEdit(page, name) {
  await page.reload({ waitUntil: "networkidle" });
  await page.evaluate((n) => {
    setSheet(true);
    const row = [...document.querySelectorAll("#trip-list .trip-row")].find((r) => r.textContent.includes(n));
    row.querySelector("button[title='Edit this trip']").click();
  }, name);
  await page.waitForSelector("form.trip-edit");
}

export const tests = [
  ["S5: the new link sends the chosen lifetime; replacing one asks first", async ({ browser, base }) => {
    const { ctx, page } = await signedIn(browser, base);
    let h = (await api(page, "GET", "/api/holidays")).body.find((x) => x.name === "Share S5");
    if (!h) h = (await api(page, "POST", "/api/holidays", { name: "Share S5", color: "#1d6fb8", start_at: "2022-06-01", end_at: "2022-06-04" })).body;
    await openEdit(page, "Share S5");
    const sent = [];
    await page.route(`**/api/holidays/${h.id}/share`, async (r) => {
      if (r.request().method() !== "POST") return r.continue();
      sent.push(r.request().postDataJSON());
      return r.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ url: "/share/abc", expires_at: "2030-01-31T12:00:00Z" }) });
    });
    const dialogs = [];
    page.on("dialog", (d) => { dialogs.push(d.type() + ":" + d.message()); d.type() === "prompt" ? d.dismiss() : d.accept(); });
    assert.equal(await page.$eval("form.trip-edit select.share-life", (s) => s.value), "30"); // default: a month
    await page.selectOption("form.trip-edit select.share-life", "7");
    await page.click("form.trip-edit .share-make button");
    // headless has no clipboard, so the link comes up in a prompt to copy
    for (let i = 0; i < 30 && !dialogs.some((d) => d.startsWith("prompt")); i++) await page.waitForTimeout(100);
    assert.ok(dialogs.some((d) => d.startsWith("prompt")), JSON.stringify(dialogs));
    assert.deepEqual(sent, [{ days: 7 }]);
    assert.ok(!dialogs.some((d) => d.startsWith("confirm")), "no confirm for a first link");
    await ctx.close();
  }],
  ["S5: a shared trip says when its link ends, and New share link confirms", async ({ browser, base }) => {
    const { ctx, page } = await signedIn(browser, base);
    let h = (await api(page, "GET", "/api/holidays")).body.find((x) => x.name === "Shared S5b");
    if (!h) h = (await api(page, "POST", "/api/holidays", { name: "Shared S5b", color: "#c0392b", start_at: "2022-07-01", end_at: "2022-07-04" })).body;
    await api(page, "POST", `/api/holidays/${h.id}/share`, { days: 0 });
    await openEdit(page, "Shared S5b");
    const state = await page.$eval("form.trip-edit .share-state", (p) => p.textContent);
    assert.match(state, /^Shared · the link (never expires|works until .+)$/);
    let posts = 0;
    await page.route(`**/api/holidays/${h.id}/share`, (r) => { if (r.request().method() === "POST") posts++; return r.continue(); });
    const dialogs = [];
    page.on("dialog", (d) => { dialogs.push(d.type()); d.dismiss(); }); // say No
    await page.click("form.trip-edit .share-make button");
    await page.waitForTimeout(300);
    assert.deepEqual(dialogs, ["confirm"]);
    assert.equal(posts, 0, "saying No keeps the old link");
    await ctx.close();
  }],
  ["S2: the share lightbox offers the preview, not the original", async ({ browser, base }) => {
    const ctx = await browser.newContext({ serviceWorkers: "block" });
    const page = await ctx.newPage();
    await page.route("**/api/share/tok/**", (r) => r.fulfill({ status: 404, body: "{}" }));
    await page.route("**/api/share/tok", (r) => r.fulfill({ status: 410, contentType: "application/json", body: '{"error":"this share link has expired"}' }));
    await page.goto(base + "/share/tok");
    await page.waitForFunction(() => document.querySelector("#overlay-title, .overlay h2, h2") && document.body.textContent.includes("has expired"), null, { timeout: 3000 });
    const href = await page.evaluate(() => {
      openLightbox([{ asset_id: "a1", taken_at: "2022-07-02T10:00:00Z" }], 0, {});
      return [document.getElementById("lb-orig").getAttribute("href"), document.getElementById("lb-orig").textContent];
    });
    assert.deepEqual(href, ["/api/share/tok/photo/a1/preview", "Open photo"]);
    await ctx.close();
  }],
];
