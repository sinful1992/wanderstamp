// B3: the username box doesn't capitalise; B5: one tap = one request; B6: failures are said out loud.
import assert from "node:assert/strict";
import { signedIn, api } from "./harness.mjs";

const toastText = (page) => page.evaluate(() => { const t = document.getElementById("toast"); return t.hidden ? "" : t.textContent; });

export const tests = [
  ["B3: the username box has autocapitalize/autocorrect off", async ({ browser, base }) => {
    const ctx = await browser.newContext({ serviceWorkers: "block" });
    const page = await ctx.newPage();
    await page.goto(base);
    const a = await page.$eval("#login-user", (i) => [i.getAttribute("autocapitalize"), i.getAttribute("autocorrect"), i.spellcheck]);
    assert.deepEqual(a, ["off", "off", false]);
    await ctx.close();
  }],
  ["B5: a double tap on Start makes one trip", async ({ browser, base }) => {
    const { ctx, page } = await signedIn(browser, base, { viewport: { width: 390, height: 844 } });
    let posts = 0;
    await page.route("**/api/holidays", async (r) => {
      if (r.request().method() !== "POST") return r.continue();
      posts++;
      await new Promise((res) => setTimeout(res, 400)); // a slow phone network
      return r.continue();
    });
    await page.evaluate(() => { setSheet(true); document.getElementById("btn-new-trip").click(); });
    await page.fill("#trip-name", "Double tap B5");
    await page.fill("#trip-start", "2024-02-01");
    await page.fill("#trip-end", "2024-02-02");
    await page.evaluate(() => {
      const b = document.querySelector("#new-trip-form [type=submit]");
      b.click(); b.click(); b.click();
    });
    await page.waitForTimeout(1200);
    assert.equal(posts, 1);
    const n = (await api(page, "GET", "/api/holidays")).body.filter((h) => h.name === "Double tap B5").length;
    assert.equal(n, 1);
    await ctx.close();
  }],
  ["B6: End holiday failing says so, and the button comes back", async ({ browser, base }) => {
    const { ctx, page } = await signedIn(browser, base);
    const hs = (await api(page, "GET", "/api/holidays")).body;
    if (!hs.some((h) => h.active)) {
      const r = await api(page, "POST", "/api/holidays", { name: "Live B6", color: "#2e7d4f" });
      assert.ok(r.status < 300, JSON.stringify(r));
    }
    await page.reload({ waitUntil: "networkidle" });
    await page.route("**/api/holidays/*/end", (r) => r.fulfill({ status: 500, contentType: "application/json", body: '{"error":"database is locked"}' }));
    page.on("dialog", (d) => d.accept());
    const errors = [];
    page.on("pageerror", (e) => errors.push(e.message));
    await page.click("#btn-end");
    await page.waitForFunction(() => document.getElementById("toast").textContent.includes("database is locked"), null, { timeout: 3000 });
    assert.equal(await page.$eval("#btn-end", (b) => b.disabled), false);
    assert.equal(await page.$eval("#btn-end", (b) => b.textContent), "End");
    assert.deepEqual(errors, []);
    await ctx.close();
  }],
  ["B6: a failed pin delete toasts instead of failing silently", async ({ browser, base }) => {
    const { ctx, page } = await signedIn(browser, base);
    await page.route("**/api/pins/*", (r) => r.request().method() === "DELETE" ? r.fulfill({ status: 503, body: "" }) : r.continue());
    // the same wrapper every delete/revoke/admin action goes through
    await page.evaluate(() => busy(null, () => api("DELETE", "/api/pins/999999")));
    assert.match(await toastText(page), /Service Unavailable|Something went wrong/);
    await ctx.close();
  }],
];
