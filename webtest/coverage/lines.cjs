// V8 block coverage → line coverage (rank 14, task 142).
// V8 gives, per function, ranges [{startOffset, endOffset, count}]: the first is the whole function,
// later ones are blocks inside it (a count-0 block is code that never ran). Applied outer to inner,
// the innermost range decides each character's count.
// A line counts when it has code: not blank, not only a comment, not only closing brackets/punctuation.
// It is covered when its first code character ran (count > 0).
function charCounts(source, functions) {
  const counts = new Int32Array(source.length).fill(0);
  const ranges = [];
  for (const f of functions) for (const r of f.ranges) ranges.push(r);
  // outer first: earlier start, then the longer range
  ranges.sort((a, b) => a.startOffset - b.startOffset || b.endOffset - a.endOffset);
  for (const r of ranges) counts.fill(r.count, Math.max(0, r.startOffset), Math.min(source.length, r.endOffset));
  return counts;
}

function lineCoverage(source, functions) {
  const counts = charCounts(source, functions);
  const countable = [], covered = [];
  let start = 0, line = 0, inBlockComment = false;
  while (start <= source.length) {
    let end = source.indexOf('\n', start);
    if (end < 0) end = source.length;
    const text = source.slice(start, end);
    // skip comments: whole-line //, and /* ... */ spans (enough for hand-written game code)
    let i = 0, code = -1;
    while (i < text.length) {
      if (inBlockComment) {
        const close = text.indexOf('*/', i);
        if (close < 0) { i = text.length; break; }
        inBlockComment = false; i = close + 2; continue;
      }
      const c = text[i];
      if (c === ' ' || c === '\t' || c === '\r') { i++; continue; }
      if (c === '/' && text[i + 1] === '/') break;
      if (c === '/' && text[i + 1] === '*') { inBlockComment = true; i += 2; continue; }
      if (/[\]\)\};,]/.test(c)) { i++; continue; } // closing only: belongs to the line that opened it
      code = i; break;
    }
    if (code >= 0) {
      countable.push(line);
      if (counts[start + code] > 0) covered.push(line);
    }
    if (end === source.length) break;
    start = end + 1; line++;
  }
  return { countable, covered };
}

module.exports = { lineCoverage, charCounts };
