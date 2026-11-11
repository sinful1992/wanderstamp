// Line coverage of static/*.js from the browser tests (audit 137 rank 14, team task 142):
//   WS_BIN=<built server> PW_CORE=... CHROME=... node webtest/coverage/run.mjs
// Runs webtest/run.mjs once with pw-coverage.cjs preloaded (every page collects V8 coverage), then prints
// the covered lines per file and one line: "coverage: <X>% lines (page.coverage)". Leaflet is not ours and
// is left out. A failed test run still reports what its pages collected, and says the run failed.
import { readdirSync, readFileSync, mkdtempSync, rmSync } from 'node:fs';
import { join, dirname, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';
import { tmpdir } from 'node:os';
import { spawnSync } from 'node:child_process';
import { merge, summary } from './merge.mjs';

const here = dirname(fileURLToPath(import.meta.url));
const root = resolve(here, '..', '..');
const covDir = mkdtempSync(join(tmpdir(), 'ws-cov-'));
const r = spawnSync(process.execPath, ['-r', join(here, 'pw-coverage.cjs'), join(root, 'webtest', 'run.mjs'), ...process.argv.slice(2)], {
  cwd: root, stdio: ['ignore', 'pipe', 'inherit'], encoding: 'utf8',
  env: { ...process.env, COV_DIR: covDir, COV_URL: '^https?://(127\\.0\\.0\\.1|localhost)(:\\d+)?/(?!leaflet/)' },
});
const last = (r.stdout || '').trim().split('\n').at(-1) || '';
const merged = merge(readdirSync(covDir).flatMap(f => JSON.parse(readFileSync(join(covDir, f), 'utf8'))));
rmSync(covDir, { recursive: true, force: true });
for (const m of merged.sort((a, b) => a.url.localeCompare(b.url))) {
  const s = summary([m]);
  console.log(`  ${m.url.padEnd(24)} ${s.covered}/${s.countable} lines ${s.pct.toFixed(1)}%`);
}
const all = summary(merged);
console.log(`webtest: ${last}${r.status ? ` (exit ${r.status}: coverage may be missing)` : ''}`);
console.log(`coverage: ${all.pct.toFixed(2)}% lines (page.coverage)`);
process.exit(r.status ?? 1);
