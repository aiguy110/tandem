// A tiny fuzzy subsequence matcher — enough for the dir-first spawn palette and
// the command palette (no dependency needed). Returns a score (higher = better)
// or -Infinity when the query isn't a subsequence of the target.
export function fuzzyScore(query: string, target: string): number {
  if (!query) return 0;
  const q = query.toLowerCase();
  const t = target.toLowerCase();
  let qi = 0;
  let score = 0;
  let streak = 0;
  let prevIdx = -1;
  for (let ti = 0; ti < t.length && qi < q.length; ti++) {
    if (t[ti] === q[qi]) {
      streak++;
      score += 1 + streak; // reward consecutive matches
      if (ti === 0 || /[\s/_\-.]/.test(t[ti - 1])) score += 3; // word-boundary bonus
      if (prevIdx >= 0 && ti === prevIdx + 1) score += 1;
      prevIdx = ti;
      qi++;
    } else {
      streak = 0;
    }
  }
  if (qi < q.length) return -Infinity;
  score -= t.length * 0.01; // gentle preference for shorter targets
  return score;
}

export function fuzzyFilter<T>(query: string, items: T[], key: (t: T) => string): T[] {
  if (!query) return items;
  return items
    .map((it) => ({ it, s: fuzzyScore(query, key(it)) }))
    .filter((x) => x.s > -Infinity)
    .sort((a, b) => b.s - a.s)
    .map((x) => x.it);
}

// A weighted field: the text to match against and how identifying it is. A
// repo's name is more identifying than its path, so a name hit should outrank a
// path hit even when both match equally well (every repo under ~/Projects has
// "/home/josiah" in its path, so a query like "home" matches all of them).
export type FuzzyField = [text: string, weight: number];

// Score a query against several weighted fields, keeping the best hit. Weights
// scale positive scores only — a negative score means a match so thin the
// length penalty swamped it, and scaling that up would rank it higher.
export function fuzzyFieldsScore(query: string, fields: FuzzyField[]): number {
  if (!query) return 0;
  let best = -Infinity;
  for (const [text, weight] of fields) {
    const s = fuzzyScore(query, text);
    if (s === -Infinity) continue;
    best = Math.max(best, s > 0 ? s * weight : s);
  }
  return best;
}

export function fuzzyFilterFields<T>(query: string, items: T[], fields: (t: T) => FuzzyField[]): T[] {
  if (!query) return items;
  return items
    .map((it) => ({ it, s: fuzzyFieldsScore(query, fields(it)) }))
    .filter((x) => x.s > -Infinity)
    .sort((a, b) => b.s - a.s)
    .map((x) => x.it);
}
