// D2: controls in sentence case, stamps keep capitals. D3: 44 px targets, labelled pins and swatches.
import assert from "node:assert/strict";
import { signedIn, api } from "./harness.mjs";

export const tests = [
  ["D2: buttons are sentence case; the stamp type is untouched", async ({ browser, base }) => {
    const { ctx, page } = await signedIn(browser, base, { viewport: { width: 360, height: 760 }, hasTouch: true, isMobile: true });
    const t = await page.evaluate(() => ({
      start: getComputedStyle(document.getElementById("btn-new-trip")).textTransform,
      title: getComputedStyle(document.getElementById("pill-label").closest("#sheet-pill")).textTransform,
    }));
    assert.equal(t.start, "none");
    assert.equal(t.title, "uppercase");
    await ctx.close();
  }],
  ["D3: swatches, the trip row's icons and zoom buttons take a 44 px finger", async ({ browser, base }) => {
    const { ctx, page } = await signedIn(browser, base, { viewport: { width: 360, height: 760 }, hasTouch: true, isMobile: true });
    let h = (await api(page, "GET", "/api/holidays")).body.find((x) => x.name === "Design D3");
    if (!h) h = (await api(page, "POST", "/api/holidays", { name: "Design D3", color: "#1d6fb8", start_at: "2021-04-01", end_at: "2021-04-02" })).body;
    await api(page, "POST", "/api/pins", { holiday_id: h.id, lat: 48.85, lng: 2.35, title: "Louvre", note: "", created_at: "2021-04-01T10:00:00Z" });
    await page.reload({ waitUntil: "networkidle" });
    await page.evaluate(() => { setSheet(true); document.getElementById("btn-new-trip").click(); });
    const r = await page.evaluate(() => {
      // a tap 20 px right of a swatch's centre still lands on that swatch
      const sw = [...document.querySelectorAll("#swatches button")];
      const hits = sw.map((b) => {
        const q = b.getBoundingClientRect();
        const x = q.left + q.width / 2, y = q.top + q.height / 2;
        return [document.elementFromPoint(x + 20, y) === b || document.elementFromPoint(x - 20, y) === b, document.elementFromPoint(x, y + 20) === b];
      });
      const eye = document.querySelector(".trip-eye").getBoundingClientRect();
      const zoom = document.querySelector(".leaflet-control-zoom a").getBoundingClientRect();
      return {
        hits, eye: [eye.width, eye.height], zoom: [zoom.width, zoom.height],
        names: sw.map((b) => b.getAttribute("aria-label")),
        pressed: sw.map((b) => b.getAttribute("aria-pressed")),
        pin: [...document.querySelectorAll(".leaflet-marker-icon")].map((m) => m.getAttribute("aria-label")).find((l) => l && l.includes("Louvre")),
        overflow: document.documentElement.scrollWidth > window.innerWidth,
      };
    });
    assert.ok(r.hits.every(([a, b]) => a && b), JSON.stringify(r.hits));
    assert.deepEqual(r.eye, [44, 44]);
    assert.ok(r.zoom[0] >= 44 && r.zoom[1] >= 44, JSON.stringify(r.zoom));
    assert.equal(r.names[0], "Blue trip colour");
    assert.equal(r.pressed.filter((p) => p === "true").length, 1);
    assert.equal(r.pin, "Louvre · Design D3");
    assert.equal(r.overflow, false, "no sideways scroll at 360 px");
    await ctx.close();
  }],
];
