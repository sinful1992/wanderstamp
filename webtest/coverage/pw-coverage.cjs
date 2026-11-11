// Preload for the browser tests (rank 14, task 142): node -r ./tools/coverage/pw-coverage.cjs <test.cjs>
// Every page a test opens collects V8 JS coverage (Playwright page.coverage, Chromium only). When the
// page, its context or the browser is closed, the covered lines of each game script are written to
// $COV_DIR/<pid>-<n>.json. A test that dies before closing anything writes nothing (the runner says so).
// Env: PW_CORE (playwright-core path, the same one the tests require), COV_DIR (output folder),
//      COV_URL (regex of script urls to keep; default ^file:// = the games' own pages).
const fs = require('fs');
const path = require('path');
const crypto = require('crypto');
const { lineCoverage } = require('./lines.cjs');

const dir = process.env.COV_DIR;
if (!dir) throw new Error('pw-coverage: set COV_DIR');
fs.mkdirSync(dir, { recursive: true });
const KEEP = new RegExp(process.env.COV_URL || '^file://');
const pw = require(process.env.PW_CORE || 'playwright-core');

let n = 0;
const pending = new Set(); // pages still collecting

async function collect(page) {
  if (!pending.has(page)) return;
  pending.delete(page);
  let entries;
  try { entries = await page.coverage.stopJSCoverage(); } catch { return; } // page already gone
  const out = [];
  for (const e of entries) {
    if (!KEEP.test(e.url) || !e.source) continue; // only our own files (not CDN libraries)
    const { countable, covered } = lineCoverage(e.source, e.functions);
    // a test server's port changes from run to run: keep the path only, so one file merges as one
    out.push({ url: e.url.replace(/^https?:\/\/[^/]+/, ''), hash: crypto.createHash('sha1').update(e.source).digest('hex'), countable, covered });
  }
  fs.writeFileSync(path.join(dir, `${process.pid}-${++n}.json`), JSON.stringify(out));
}

const tracked = new WeakSet();
async function track(page) {
  if (tracked.has(page)) return page; // browser.newPage goes through context.newPage: count it once
  tracked.add(page);
  pending.add(page);
  await page.coverage.startJSCoverage({ resetOnNavigation: false, reportAnonymousScripts: false });
  const close = page.close.bind(page);
  page.close = async (...a) => { await collect(page); return close(...a); };
  return page;
}

function wrapContext(ctx) {
  const newPage = ctx.newPage.bind(ctx);
  ctx.newPage = async (...a) => track(await newPage(...a));
  const close = ctx.close.bind(ctx);
  ctx.close = async (...a) => { await Promise.all(ctx.pages().map(collect)); return close(...a); };
  return ctx;
}

const launch = pw.chromium.launch.bind(pw.chromium);
pw.chromium.launch = async (...a) => {
  const b = await launch(...a);
  const newContext = b.newContext.bind(b);
  b.newContext = async (...x) => wrapContext(await newContext(...x));
  const newPage = b.newPage.bind(b);
  b.newPage = async (...x) => track(await newPage(...x));
  const close = b.close.bind(b);
  b.close = async (...x) => { await Promise.all([...pending].map(collect)); return close(...x); };
  return b;
};
