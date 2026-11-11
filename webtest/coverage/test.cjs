// Checks for the coverage tool (rank 14, task 142): node tools/coverage/test.cjs   env: PW_CORE, CHROME
// A fixture page with an uncalled function, an untaken if, a never-run arrow and a second page that calls
// the uncalled function: exact covered/countable lines per page and after the merge. Also: comment and
// closing-bracket lines are not counted, and browser.newPage (which goes through newContext) counts once.
const fs = require('fs');
const os = require('os');
const path = require('path');
const assert = require('assert/strict');

const covDir = fs.mkdtempSync(path.join(os.tmpdir(), 'covtest-'));
process.env.COV_DIR = covDir;
require('./pw-coverage.cjs');
const { chromium } = require(process.env.PW_CORE || 'playwright-core');
const { lineCoverage } = require('./lines.cjs');

let fails = 0;
const check = (name, fn) => { try { fn(); console.log('ok  ', name); } catch (e) { fails++; console.log('FAIL', name, '\n     ', e.message.replace(/\s+/g, ' ').slice(0, 400)); } };

// lines of the inline script, 0-based (line 0 is the empty line right after <script>)
const SCRIPT = `
// a comment line: never counted
function used(x) {
  if (x > 5) {
    window.big = true;
  }
  return x + 1;
}
/* a block
   comment */
function unused() {
  window.u = 1;
  return 2;
}
const arrow = () => {
  window.a = 1;
};
window.r = used(1);
if (location.hash === '#two') unused();
`;
// countable: 2 function used, 3 if, 4 window.big, 6 return, 10 function unused, 11 window.u, 12 return 2,
//            14 const arrow, 15 window.a, 17 window.r, 18 if location   (5,7,16 are only brackets)
const COUNTABLE = [2, 3, 4, 6, 10, 11, 12, 14, 15, 17, 18];
const PAGE1 = [2, 3, 6, 14, 17, 18];         // V8 counts an uncalled function from its `function` keyword: line 10 too
const PAGE2 = [2, 3, 6, 10, 11, 12, 14, 17, 18];

const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'covfix-'));
const html = path.join(dir, 'fixture.html');
fs.writeFileSync(html, `<!doctype html><html><body><script>${SCRIPT}</script></body></html>`);
const url = 'file:///' + html.replace(/\\/g, '/').replace(/^\//, '');

(async () => {
  const { merge, summary } = await import('./merge.mjs');
  const b = await chromium.launch({ executablePath: process.env.CHROME });
  const ctx = await b.newContext();
  const p1 = await ctx.newPage();
  await p1.goto(url);
  await p1.close();
  const p2 = await b.newPage(); // the other way to make a page
  await p2.goto(url + '#two');
  await b.close(); // p2 is collected by the browser's close

  const files = fs.readdirSync(covDir).sort().map(f => JSON.parse(fs.readFileSync(path.join(covDir, f), 'utf8')));
  const mine = files.map(f => f.filter(e => e.url.endsWith('fixture.html')));
  check('two pages, two files, one fixture script each', () => {
    assert.equal(files.length, 2);
    assert.deepEqual(mine.map(m => m.length), [1, 1]);
  });
  check('countable lines skip blanks, comments and closing brackets', () => assert.deepEqual(mine[0][0].countable, COUNTABLE));
  check('page 1: uncalled function body, untaken if and never-run arrow are not covered', () => assert.deepEqual(mine[0][0].covered, PAGE1));
  check('page 2: the function page 1 never called is covered', () => assert.deepEqual(mine[1][0].covered, PAGE2));
  check('merge: one script, union of both pages', () => {
    const m = merge(files.flat().filter(e => e.url.endsWith('fixture.html')));
    assert.equal(m.length, 1);
    const s = summary(m);
    assert.deepEqual([s.scripts, s.covered, s.countable], [1, PAGE2.length, COUNTABLE.length]);
  });
  check('merge keeps two different sources under one url apart', () => {
    const e = mine[0][0];
    const s = summary(merge([e, { ...e, hash: 'other', covered: [] }]));
    assert.deepEqual([s.scripts, s.countable], [2, 2 * COUNTABLE.length]);
  });
  check('lineCoverage: innermost range decides, a line counts by its first code character', () => {
    const src = 'a();\n  b();\n';
    const fns = [{ ranges: [{ startOffset: 0, endOffset: src.length, count: 1 }, { startOffset: 5, endOffset: 12, count: 0 }] }];
    assert.deepEqual(lineCoverage(src, fns), { countable: [0, 1], covered: [0] });
  });
  check('lineCoverage: no ranges at all means nothing covered', () => assert.deepEqual(lineCoverage('x();\n', []), { countable: [0], covered: [] }));
  check('lineCoverage: empty source', () => assert.deepEqual(lineCoverage('', []), { countable: [], covered: [] }));
  fs.rmSync(covDir, { recursive: true, force: true }); fs.rmSync(dir, { recursive: true, force: true });
  console.log(fails ? `${fails} FAILED` : 'all passed');
  process.exit(fails ? 1 : 0);
})().catch(e => { console.error(e); process.exit(1); });
