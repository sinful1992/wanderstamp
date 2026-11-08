// Task 136: a note belongs to one visit. /api/pins is mocked with visits, so
// these run against the fields the server contract promises.
import assert from "node:assert/strict";
import { signedIn, api } from "./harness.mjs";

const V = (day, note, n = 1) => ({ at: `${day}T10:00:00Z`, until: `${day}T11:00:00Z`, n, day, note });

async function setup(browser, base, visits, extra = {}) {
  const { ctx, page } = await signedIn(browser, base, { viewport: { width: 390, height: 800 } });
  let h = (await api(page, "GET", "/api/holidays")).body.find((x) => x.name === "Notes trip");
  if (!h) h = (await api(page, "POST", "/api/holidays", { name: "Notes trip", color: "#1d6fb8", start_at: "2026-09-25", end_at: "2026-09-27" })).body;
  const pin = { id: 900, holiday_id: h.id, kind: "manual", lat: 52.4, lng: -1.5, title: "Premier", note: "", photo_count: 0, cover_asset: "", created_at: "2026-09-25T10:00:00Z", visited_at: "2026-09-25T10:00:00Z", visits, ...extra };
  await page.route("**/api/pins", (r) => r.request().method() === "GET" ? r.fulfill({ json: [pin] }) : r.continue());
  const sent = [];
  await page.route("**/api/pins/900", (r) => { sent.push(r.request().postDataJSON()); return r.fulfill({ json: {} }); });
  await page.reload({ waitUntil: "networkidle" });
  await page.evaluate((id) => openStory(state.holidays.find((x) => x.id === id)), h.id);
  await page.waitForSelector(".story-actions button");
  return { ctx, page, sent, pin };
}

const notes = (page) => page.$$eval(".story-note", (n) => n.map((x) => x.textContent));
async function edit(page, nth = 0) {
  await page.locator(".story-actions button", { hasText: "Edit" }).nth(nth).click();
  await page.waitForSelector("form.pop-form textarea");
}
const save = (page) => page.click("form.pop-form button[type=submit]");
const waitSent = (page, sent) => page.waitForFunction(() => document.querySelector("#toast, .toast")).then(() => sent);

export const tests = [
  ["notes: each stop shows its own visit's note", async ({ browser, base }) => {
    const { ctx, page } = await setup(browser, base, [V("2026-09-25", ""), V("2026-09-26", "Food horrible")]);
    assert.deepEqual(await notes(page), ["Food horrible"]);
    await ctx.close();
  }],

  ["notes: editing a stop opens on that visit, and saves under its day", async ({ browser, base }) => {
    const { ctx, page, sent } = await setup(browser, base, [V("2026-09-25", "first"), V("2026-09-26", "second")]);
    await edit(page, 1);
    assert.equal(await page.inputValue("select.note-day"), "2026-09-26");
    assert.equal(await page.inputValue("form.pop-form textarea"), "second");
    await page.fill("form.pop-form textarea", "changed");
    await save(page);
    await waitSent(page, sent);
    assert.deepEqual(sent[0], { title: "Premier", note: "changed", note_day: "2026-09-26" });
    await ctx.close();
  }],

  ["notes: moving a note to an empty day is one call with from_day", async ({ browser, base }) => {
    const { ctx, page, sent } = await setup(browser, base, [V("2026-09-25", "Food horrible"), V("2026-09-26", "")]);
    await edit(page, 0);
    await page.selectOption("select.note-day", "2026-09-26");
    assert.equal(await page.inputValue("form.pop-form textarea"), "Food horrible");
    await save(page);
    await waitSent(page, sent);
    assert.equal(sent.length, 1);
    assert.deepEqual(sent[0], { title: "Premier", note: "Food horrible", note_day: "2026-09-26", from_day: "2026-09-25" });
    await ctx.close();
  }],

  ["notes: picking a day with a note shows it, and asks before dropping typed text", async ({ browser, base }) => {
    const { ctx, page } = await setup(browser, base, [V("2026-09-25", "first"), V("2026-09-26", "second")]);
    await edit(page, 0);
    await page.fill("form.pop-form textarea", "typed");
    page.once("dialog", (d) => d.dismiss());
    await page.selectOption("select.note-day", "2026-09-26");
    assert.equal(await page.inputValue("select.note-day"), "2026-09-25");
    assert.equal(await page.inputValue("form.pop-form textarea"), "typed");
    page.once("dialog", (d) => d.accept());
    await page.selectOption("select.note-day", "2026-09-26");
    assert.equal(await page.inputValue("form.pop-form textarea"), "second");
    await ctx.close();
  }],

  ["notes: one visit has no list and still saves under its day; an unchanged note isn't sent", async ({ browser, base }) => {
    const { ctx, page, sent } = await setup(browser, base, [V("2026-09-25", "only")]);
    await edit(page, 0);
    assert.equal(await page.locator("select.note-day").isVisible(), false);
    await save(page);
    await waitSent(page, sent);
    assert.deepEqual(sent[0], { title: "Premier" });
    await ctx.close();
  }],

  ["notes: two visits on one day are one choice; a note on a day with no visit can be edited", async ({ browser, base }) => {
    const twice = [V("2026-09-25", "same"), { ...V("2026-09-25", ""), at: "2026-09-25T15:00:00Z", until: "2026-09-25T16:00:00Z" }];
    const { ctx, page } = await setup(browser, base, twice, { other_notes: [{ day: "2026-09-20", note: "lost" }] });
    await edit(page, 0);
    const opts = await page.$$eval("select.note-day option", (o) => o.map((x) => x.value));
    assert.deepEqual(opts, ["2026-09-25", "2026-09-20"]);
    await page.selectOption("select.note-day", "2026-09-20");
    assert.equal(await page.inputValue("form.pop-form textarea"), "lost");
    await ctx.close();
  }],

  ["notes: a pin with no visits keeps its plain note", async ({ browser, base }) => {
    const { ctx, page, sent } = await setup(browser, base, [], { note: "plain" });
    assert.deepEqual(await notes(page), ["plain"]);
    await edit(page, 0);
    await page.fill("form.pop-form textarea", "plain 2");
    await save(page);
    await waitSent(page, sent);
    assert.deepEqual(sent[0], { title: "Premier", note: "plain 2" });
    await ctx.close();
  }],
];
