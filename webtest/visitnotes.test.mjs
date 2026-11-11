// Task 136: a note belongs to one visit. /api/map is mocked with visits, so
// these run against the fields the server contract promises.
import assert from "node:assert/strict";
import { signedIn, api } from "./harness.mjs";

const V = (day, note, n = 1) => ({ at: `${day}T10:00:00Z`, until: `${day}T11:00:00Z`, n, day, note });

async function setup(browser, base, visits, extra = {}) {
  const { ctx, page } = await signedIn(browser, base, { viewport: { width: 390, height: 800 } });
  let h = (await api(page, "GET", "/api/holidays")).body.find((x) => x.name === "Notes trip");
  if (!h) h = (await api(page, "POST", "/api/holidays", { name: "Notes trip", color: "#1d6fb8", start_at: "2026-09-25", end_at: "2026-09-27" })).body;
  const pin = { id: 900, holiday_id: h.id, kind: "manual", lat: 52.4, lng: -1.5, title: "Premier", note: "", photo_count: 0, cover_asset: "", created_at: "2026-09-25T10:00:00Z", visited_at: "2026-09-25T10:00:00Z", visits, ...extra };
  await page.route("**/api/map*", async (r) => {
    // a map reload still in flight when the test closes its context: nothing left to answer (linus #10321)
    try {
      const res = await r.fetch();
      await r.fulfill({ response: res, json: { ...(await res.json()), pins: [pin] } });
    } catch (e) {
      if (!page.isClosed() && !/closed/i.test(String(e && e.message))) throw e;
    }
  });
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
async function waitSent(page, sent) {
  for (let i = 0; i < 100 && !sent.length; i++) await page.waitForTimeout(50);
  assert.ok(sent.length, "the PATCH was sent");
}

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

  ["notes: new words on another day are a new note and leave the first one (linus #9744)", async ({ browser, base }) => {
    const { ctx, page, sent } = await setup(browser, base, [V("2026-09-25", "X"), V("2026-09-26", "")]);
    await edit(page, 0);
    await page.selectOption("select.note-day", "2026-09-26");
    assert.equal(await page.locator(".note-move").innerText(), "Moves the note from Fri 25 Sept");
    await page.fill("form.pop-form textarea", "Breakfast ok");
    assert.equal(await page.locator(".note-move").isVisible(), false);
    await save(page);
    await waitSent(page, sent);
    assert.deepEqual(sent[0], { title: "Premier", note: "Breakfast ok", note_day: "2026-09-26" });
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

  // USER #9792: the two branches V8 showed never ran (linus #9797).
  ["notes: visits with no note field show the pin's note once, at the first stop", async ({ browser, base }) => {
    const { ctx, page } = await setup(browser, base, [V("2026-09-25"), V("2026-09-26")], { note: "old" });
    assert.deepEqual(await notes(page), ["old"]);
    await ctx.close();
  }],

  ["notes: a pin with no visits field edits as a plain note", async ({ browser, base }) => {
    const { ctx, page, sent } = await setup(browser, base, undefined, { note: "plain" });
    await edit(page, 0);
    assert.equal(await page.locator("select.note-day").count(), 0);
    await page.fill("form.pop-form textarea", "plain 2");
    await save(page);
    await waitSent(page, sent);
    assert.deepEqual(sent[0], { title: "Premier", note: "plain 2" });
    await ctx.close();
  }],
  // WA-1 (audit 137): Cancel on the question goes back to the day the words were typed for.
  ["WA-1: Cancel returns the picker to the day picked before, and Save leaves the first day's note alone", async ({ browser, base }) => {
    const { ctx, page, sent } = await setup(browser, base, [V("2026-09-25", "first"), V("2026-09-26", ""), V("2026-09-27", "third")]);
    await edit(page, 0);
    await page.selectOption("select.note-day", "2026-09-26");
    await page.fill("form.pop-form textarea", "for the 26th");
    page.once("dialog", (d) => d.dismiss());
    await page.selectOption("select.note-day", "2026-09-27");
    assert.equal(await page.inputValue("select.note-day"), "2026-09-26");
    assert.equal(await page.inputValue("form.pop-form textarea"), "for the 26th");
    await save(page);
    await waitSent(page, sent);
    assert.deepEqual(sent[0], { title: "Premier", note: "for the 26th", note_day: "2026-09-26" });
    await ctx.close();
  }],

  ["WA-1: Cancel twice, then OK: the picker follows every step", async ({ browser, base }) => {
    const { ctx, page, sent } = await setup(browser, base, [V("2026-09-25", "first"), V("2026-09-26", ""), V("2026-09-27", "third")]);
    await edit(page, 0);
    await page.fill("form.pop-form textarea", "typed");
    page.once("dialog", (d) => d.dismiss());
    await page.selectOption("select.note-day", "2026-09-27");
    assert.equal(await page.inputValue("select.note-day"), "2026-09-25", "nothing else was picked: back to the first day");
    await page.selectOption("select.note-day", "2026-09-26");
    page.once("dialog", (d) => d.dismiss());
    await page.selectOption("select.note-day", "2026-09-27");
    assert.equal(await page.inputValue("select.note-day"), "2026-09-26");
    page.once("dialog", (d) => d.accept());
    await page.selectOption("select.note-day", "2026-09-27");
    assert.equal(await page.inputValue("form.pop-form textarea"), "third");
    // moving the shown note to the empty day is still a move from the 27th
    await page.selectOption("select.note-day", "2026-09-26");
    await save(page);
    await waitSent(page, sent);
    assert.deepEqual(sent[0], { title: "Premier", note: "third", note_day: "2026-09-26", from_day: "2026-09-27" });
    await ctx.close();
  }],
  ["a pin deleted on another phone: saving it answers 404, the map reloads and the pin goes, what was typed stays", async ({ browser, base }) => {
    const { ctx, page } = await setup(browser, base, [V("2026-09-25", "first")]);
    let maps = 0, gone = false;
    await page.unroute("**/api/map*");
    await page.route("**/api/map*", async (r) => {
      maps++;
      try {
        const res = await r.fetch();
        const j = await res.json();
        await r.fulfill({ response: res, json: { ...j, pins: gone ? j.pins : [...j.pins, { id: 900, holiday_id: j.holidays[0].id, kind: "manual", lat: 52.4, lng: -1.5, title: "Premier", note: "", photo_count: 0, cover_asset: "" }] } });
      } catch (e) {
        if (!page.isClosed() && !/closed/i.test(String(e && e.message))) throw e;
      }
    });
    await page.unroute("**/api/pins/900");
    await page.route("**/api/pins/900", (r) => { gone = true; return r.fulfill({ status: 404, json: { error: "pin not found" } }); });
    await edit(page, 0);
    await page.fill("form.pop-form textarea", "typed on a pin that is gone");
    const before = maps;
    await save(page);
    for (let i = 0; i < 100 && maps === before; i++) await page.waitForTimeout(50);
    assert.ok(maps > before, "the map was asked for again");
    await page.waitForFunction(() => !state.pins.some((p) => p.id === 900));
    assert.equal(await page.inputValue("form.pop-form textarea"), "typed on a pin that is gone", "the words are not wiped");
    await ctx.close();
  }],
  ["a save that fails for another reason (500) does not reload the map", async ({ browser, base }) => {
    const { ctx, page } = await setup(browser, base, [V("2026-09-25", "first")]);
    let hit = false, maps = 0;
    await page.unroute("**/api/pins/900");
    await page.route("**/api/pins/900", (r) => { hit = true; return r.fulfill({ status: 500, json: { error: "database error" } }); });
    page.on("request", (q) => { if (/\/api\/map/.test(q.url())) maps++; });
    await edit(page, 0);
    await page.fill("form.pop-form textarea", "typed");
    await save(page);
    for (let i = 0; i < 100 && !hit; i++) await page.waitForTimeout(50);
    await page.waitForTimeout(400);
    assert.ok(hit, "the PATCH was sent");
    assert.equal(maps, 0, "no reload on a 500");
    await ctx.close();
  }],
];
