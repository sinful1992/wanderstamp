// B1: trip dates picked as calendar days must show as those days in any timezone.
import assert from "node:assert/strict";
import { signedIn, api } from "./harness.mjs";

// ICU builds differ: some put spaces round a range dash ("23 – 24 May").
const dash = (s) => s.replace(/\s*–\s*/g, "–").replace(/\s/g, " ");

async function tripMeta(page, name) {
  await page.reload({ waitUntil: "networkidle" });
  return page.evaluate((n) => {
    const h = state.holidays.find((x) => x.name === n);
    return { range: tripRange(h), days: tripDays(h) };
  }, name);
}

export const tests = [
  ["BST: a trip that ended 24 May shows 23–24 May, 2 days", async ({ browser, base }) => {
    const { ctx, page } = await signedIn(browser, base, { timezoneId: "Europe/London" });
    const r = await api(page, "POST", "/api/holidays", { name: "Bransgore B1", color: "#1d6fb8", start_at: "2026-05-23", end_at: "2026-05-24" });
    assert.ok(r.status < 300, JSON.stringify(r));
    const m = await tripMeta(page, "Bransgore B1");
    assert.equal(dash(m.range), "23–24 May 2026");
    assert.equal(m.days, 2);
    // the trips list shows the same line
    const shown = await page.evaluate(() => [...document.querySelectorAll(".trip-meta")].map((e) => e.textContent).find((t) => t.includes("May 2026")));
    assert.ok(shown && dash(shown).startsWith("23–24 May 2026"), shown);
    await ctx.close();
  }],
  ["west of UTC: the start day does not slip back a day", async ({ browser, base }) => {
    const { ctx, page } = await signedIn(browser, base, { timezoneId: "America/New_York" });
    await api(page, "POST", "/api/holidays", { name: "NY B1", color: "#c0392b", start_at: "2026-01-10", end_at: "2026-01-12" });
    const m = await tripMeta(page, "NY B1");
    assert.equal(dash(m.range), "10–12 Jan 2026");
    assert.equal(m.days, 3);
    await ctx.close();
  }],
  ["a one-day trip across the year end and a real end moment", async ({ browser, base }) => {
    const { page, ctx } = await signedIn(browser, base, { timezoneId: "Europe/London" });
    const out = await page.evaluate(() => ({
      one: fmtRange("2026-07-04T00:00:00Z", "2026-07-04T23:59:59Z"),
      oneDays: tripDays({ start_at: "2026-07-04T00:00:00Z", end_at: "2026-07-04T23:59:59Z" }),
      ny: fmtRange("2025-12-30T00:00:00Z", "2026-01-02T23:59:59Z"),
      // ended with the button at 23:30 BST = 22:30Z: that's still the 24th locally
      moment: fmtTripDay("2026-05-24T22:30:00Z"),
      bare: fmtTripDay("2026-05-24"),
      bad: fmtRange("nonsense", "2026-01-01"),
    }));
    assert.equal(out.one, "4 Jul 2026");
    assert.equal(out.oneDays, 1);
    assert.equal(dash(out.ny), "30 Dec 2025–2 Jan 2026");
    assert.equal(out.moment, "24 May 2026");
    assert.equal(out.bare, "24 May 2026");
    assert.equal(out.bad, "Invalid Date");
    await ctx.close();
  }],
];
