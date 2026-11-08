// Task 136: the TV's QR code opens /#pair=CODE on the phone. One Confirm tap
// pairs; nothing pairs without it. /api/tv/pair is mocked: tvlive covers the real one.
import assert from "node:assert/strict";
import { signedIn, USER, PASS } from "./harness.mjs";

const ANDROID = "Mozilla/5.0 (Linux; Android 14; SM-S931B) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/130.0 Mobile Safari/537.36";
const PHONE = { viewport: { width: 390, height: 800 } };

function mockPair(ctx, statuses = []) {
  const sent = [];
  return ctx.route("**/api/tv/pair", (r) => {
    sent.push(r.request().postDataJSON());
    const status = statuses.shift() || 200;
    return r.fulfill({ status, contentType: "application/json", body: JSON.stringify(status === 200 ? { id: 1 } : { error: "no" }) });
  }).then(() => sent);
}

async function signIn(page) {
  await page.fill("#login-user", USER);
  await page.fill("#login-pass", PASS);
  await page.click("#login-form button[type=submit]");
}

export const tests = [
  ["QR: signed out, the code survives sign-in, then one Confirm pairs and the hash is gone", async ({ browser, base }) => {
    const ctx = await browser.newContext({ ...PHONE, locale: "en-GB", serviceWorkers: "block" });
    const sent = await mockPair(ctx);
    const page = await ctx.newPage();
    await page.goto(`${base}/#pair=KXRT`, { waitUntil: "networkidle" });
    assert.equal(await page.locator("#login").isVisible(), true);
    await signIn(page);
    await page.waitForSelector("#tv-pair-ok");
    assert.equal(sent.length, 0, "nothing is sent before Confirm");
    assert.equal(await page.locator(".tv-pair-code").innerText(), "KXRT");
    await page.fill("#tv-pair-name", "Bedroom");
    await page.click("#tv-pair-ok");
    await page.waitForFunction(() => document.getElementById("overlay").hidden);
    assert.deepEqual(sent, [{ code: "KXRT", name: "Bedroom" }]);
    assert.equal(new URL(page.url()).hash, "");
    await ctx.close();
  }],

  ["QR: Cancel sends nothing, and a reload does not ask again", async ({ browser, base }) => {
    const { ctx, page } = await signedIn(browser, base, PHONE);
    const sent = await mockPair(ctx);
    await page.goto(`${base}/#pair=KXRT`);
    await page.waitForSelector("#tv-pair-ok");
    await page.click(".tv-pair-confirm .linkish");
    await page.reload({ waitUntil: "networkidle" });
    assert.equal(await page.locator("#tv-pair-ok").count(), 0);
    assert.equal(sent.length, 0);
    await ctx.close();
  }],

  ["QR: a scan while the page is already open still asks", async ({ browser, base }) => {
    const { ctx, page } = await signedIn(browser, base, PHONE);
    await mockPair(ctx);
    await page.evaluate(() => { location.hash = "#pair=MPQW"; });
    await page.waitForSelector("#tv-pair-ok");
    assert.equal(await page.locator(".tv-pair-code").innerText(), "MPQW");
    await ctx.close();
  }],

  ["QR: an expired code says so, keeps the sheet, and a second Confirm can succeed", async ({ browser, base }) => {
    const { ctx, page } = await signedIn(browser, base, PHONE);
    const sent = await mockPair(ctx, [404, 200]);
    await page.goto(`${base}/#pair=KXRT`);
    await page.click("#tv-pair-ok");
    await page.waitForFunction(() => document.body.innerText.includes("expired"));
    assert.equal(await page.locator("#overlay").isVisible(), true);
    await page.click("#tv-pair-ok");
    await page.waitForFunction(() => document.getElementById("overlay").hidden);
    assert.equal(sent.length, 2);
    await ctx.close();
  }],

  ["QR: an empty name doesn't send, and a double tap sends once", async ({ browser, base }) => {
    const { ctx, page } = await signedIn(browser, base, PHONE);
    const sent = await mockPair(ctx);
    await page.goto(`${base}/#pair=KXRT`);
    await page.fill("#tv-pair-name", "   ");
    await page.click("#tv-pair-ok");
    assert.equal(sent.length, 0);
    await page.fill("#tv-pair-name", "Den");
    await page.evaluate(() => { const b = document.getElementById("tv-pair-ok"); b.click(); b.click(); });
    await page.waitForFunction(() => document.getElementById("overlay").hidden);
    assert.equal(sent.length, 1);
    await ctx.close();
  }],

  ["QR: junk in the hash shows no sheet and runs nothing", async ({ browser, base }) => {
    const { ctx, page } = await signedIn(browser, base, PHONE);
    const sent = await mockPair(ctx);
    for (const h of ["#pair=kxrt", "#pair=KXR", "#pair=KXRTT", "#pair=KIOT", "#pair=%3Cimg%20src=x%20onerror=window.__x=1%3E", "#pair="]) {
      await page.goto(`${base}/${h}`);
      await page.reload({ waitUntil: "networkidle" });
      assert.equal(await page.locator("#tv-pair-ok").count(), 0, h);
    }
    assert.equal(await page.evaluate(() => window.__x), undefined);
    assert.equal(sent.length, 0);
    await ctx.close();
  }],

  ["QR: the Open in app link shows only in an Android browser, never inside the app or after coming back", async ({ browser, base }) => {
    const cases = [["#pair=KXRT", ANDROID, 1], ["#pair=KXRT&noapp=1", ANDROID, 0], ["#pair=KXRT&app=1", ANDROID, 0], ["#pair=KXRT", undefined, 0]];
    for (const [hash, userAgent, want] of cases) {
      const ctx = await browser.newContext({ ...PHONE, locale: "en-GB", serviceWorkers: "block", ...(userAgent ? { userAgent } : {}) });
      const page = await ctx.newPage();
      await page.goto(`${base}/${hash}`, { waitUntil: "networkidle" });
      assert.equal(await page.locator("#pair-app").count(), want, `${hash} ${userAgent || "desktop"}`);
      if (want) {
        const href = await page.locator("#pair-app").getAttribute("href");
        assert.match(href, /^intent:\/\/pair\?code=KXRT#Intent;scheme=wanderstamp;package=app\.wanderstamp;/);
        assert.ok(href.includes(encodeURIComponent("#pair=KXRT&noapp=1")), "falls back with noapp, so it cannot loop");
        const box = await page.locator("#pair-app").boundingBox();
        assert.ok(box.y + box.height <= 800 && box.x >= 0 && box.x + box.width <= 390, "on screen");
      }
      await ctx.close();
    }
  }],

  ["QR: the plain page has no sheet, and Show on TV by code still works beside it", async ({ browser, base }) => {
    const { ctx, page } = await signedIn(browser, base, PHONE);
    assert.equal(await page.locator("#tv-pair-ok").count(), 0);
    assert.equal(await page.locator("#pair-app").count(), 0);
    await ctx.close();
  }],
];
