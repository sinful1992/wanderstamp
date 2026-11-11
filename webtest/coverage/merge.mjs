// Merge per-page line coverage (rank 14, task 142). The same script (url + source hash) seen by many
// pages counts once: a line is covered when any page ran it. Different sources (an old and a new build
// under one url) stay apart.
export function merge(entries) {
  const byKey = new Map();
  for (const e of entries) {
    const key = e.url + '#' + e.hash;
    let m = byKey.get(key);
    if (!m) byKey.set(key, m = { url: e.url, hash: e.hash, countable: new Set(e.countable), covered: new Set() });
    for (const l of e.covered) m.covered.add(l);
  }
  return [...byKey.values()];
}

export function summary(merged, keep = () => true) {
  let countable = 0, covered = 0, scripts = 0;
  for (const m of merged) {
    if (!keep(m.url)) continue;
    scripts++;
    countable += m.countable.size;
    for (const l of m.covered) if (m.countable.has(l)) covered++;
  }
  return { scripts, countable, covered, pct: countable ? (100 * covered) / countable : 0 };
}
