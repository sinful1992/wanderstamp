// v1.14.0 features, checked in the real app.
import assert from "node:assert/strict";
import { signedIn, api } from "./harness.mjs";

async function trips(page, list) {
  const have = (await api(page, "GET", "/api/holidays")).body;
  const out = {};
  for (const t of list) {
    let h = have.find((x) => x.name === t.name);
    if (!h) {
      const r = await api(page, "POST", "/api/holidays", { color: "#1d6fb8", ...t });
      assert.ok(r.status < 300, JSON.stringify(r));
      h = r.body;
    }
    out[t.name] = h;
  }
  return out;
}

const F1 = [
  { name: "F1 Côte d'Azur", start_at: "2023-07-01", end_at: "2023-07-09" },
  { name: "F1 Lisbon", start_at: "2024-04-02", end_at: "2024-04-06" },
  { name: "F1 New Year Rome", start_at: "2024-12-30", end_at: "2025-01-02" },
  { name: "F1 Bath", start_at: "2025-06-10", end_at: "2025-06-12" },
  { name: "F1 Skye", start_at: "2025-08-01", end_at: "2025-08-08" },
];

const rowNames = (page) => page.$$eval("#trip-list .trip-row .trip-name", (ns) => ns.map((n) => n.textContent.trim()));

export const tests = [
  ["F1: the search finds a trip without its accents, and 'No trips match' clears", async ({ browser, base }) => {
    const { ctx, page } = await signedIn(browser, base);
    await trips(page, F1);
    await page.reload({ waitUntil: "networkidle" });
    await page.evaluate(() => setSheet(true));
    assert.equal(await page.isVisible("#trip-search"), true);
    await page.fill("#trip-search", "cote");
    assert.deepEqual(await rowNames(page), ["F1 Côte d'Azur"]);
    await page.fill("#trip-search", "rome new");
    assert.deepEqual(await rowNames(page), ["F1 New Year Rome"]);
    await page.fill("#trip-search", "zzzz");
    assert.deepEqual(await rowNames(page), []);
    assert.ok((await page.textContent("#trip-list")).includes("No trips match"));
    await page.click("#trip-list .sheet-none button");
    assert.equal(await page.inputValue("#trip-search"), "");
    assert.ok((await rowNames(page)).length >= 5);
    // a background reload keeps the search
    await page.fill("#trip-search", "lisbon");
    await page.evaluate(() => loadData());
    assert.deepEqual(await rowNames(page), ["F1 Lisbon"]);
    await ctx.close();
  }],
  ["F1: a year chip shows the trips that touch that year, a New Year trip in both", async ({ browser, base }) => {
    const { ctx, page } = await signedIn(browser, base);
    await trips(page, F1);
    await page.reload({ waitUntil: "networkidle" });
    await page.evaluate(() => setSheet(true));
    const chips = await page.$$eval("#year-chips .chip", (cs) => cs.map((c) => c.textContent));
    assert.equal(chips[0], "All");
    for (const y of ["2025", "2024", "2023"]) assert.ok(chips.includes(y), chips.join(","));
    const pick = async (y) => {
      await page.click(`#year-chips .chip:text-is("${y}")`);
      return (await rowNames(page)).filter((n) => n.startsWith("F1 "));
    };
    assert.deepEqual((await pick("2025")).sort(), ["F1 Bath", "F1 New Year Rome", "F1 Skye"]);
    assert.deepEqual((await pick("2024")).sort(), ["F1 Lisbon", "F1 New Year Rome"]);
    assert.equal(await page.getAttribute(`#year-chips .chip:text-is("2024")`, "aria-pressed"), "true");
    // year and search together
    await page.fill("#trip-search", "rome");
    assert.deepEqual(await rowNames(page), ["F1 New Year Rome"]);
    await page.fill("#trip-search", "");
    assert.equal((await pick("All")).length, 5);
    await ctx.close();
  }],
  ["F1: a hidden trip stays hidden after a reload, and only for that user", async ({ browser, base }) => {
    const { ctx, page } = await signedIn(browser, base);
    const t = await trips(page, F1);
    await api(page, "POST", "/api/pins", { holiday_id: t["F1 Bath"].id, lat: 51.38, lng: -2.36, title: "F1 Bath pin", note: "" });
    await page.reload({ waitUntil: "networkidle" });
    await page.evaluate(() => setSheet(true));
    const eye = (page) => page.evaluateHandle(() =>
      [...document.querySelectorAll("#trip-list .trip-row")].find((r) => r.textContent.includes("F1 Bath")).querySelector("button[title^='Show or hide']"));
    await (await eye(page)).click();
    const visible = () => page.evaluate(() => visiblePins().some((p) => p.title === "F1 Bath pin"));
    assert.equal(await visible(), false);
    await page.reload({ waitUntil: "networkidle" });
    assert.equal(await visible(), false, "hidden after reload");
    assert.equal(await page.evaluate((id) => state.hidden.has(id), t["F1 Bath"].id), true);
    // another user on the same browser starts with nothing hidden
    const other = await page.evaluate(() => localStorage.getItem("hm-hidden:someone-else"));
    assert.equal(other, null);
    // and showing it again is remembered too
    await page.evaluate(() => setSheet(true));
    await (await eye(page)).click();
    await page.reload({ waitUntil: "networkidle" });
    assert.equal(await visible(), true);
    await ctx.close();
  }],
  ["F1: a short list has no search box, and junk in storage doesn't break the map", async ({ browser, base }) => {
    const { ctx, page } = await signedIn(browser, base);
    await page.evaluate(() => {
      localStorage.setItem(`hm-hidden:${state.me.username}`, "{not json");
    });
    await page.reload({ waitUntil: "networkidle" });
    assert.equal(await page.evaluate(() => state.hidden.size), 0);
    await page.evaluate(() => setSheet(true));
    const n = await page.evaluate(() => state.holidays.length);
    assert.equal(await page.isHidden("#trip-find"), n < 5);
    await page.evaluate(() => { state.holidays = state.holidays.slice(0, 2); renderSheet(); });
    assert.equal(await page.isHidden("#trip-find"), true);
    await ctx.close();
  }],
];
