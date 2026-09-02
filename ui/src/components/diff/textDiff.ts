type Operation = { kind: 'context' | 'add' | 'remove'; text: string; oldLine: number; newLine: number };

function lines(text: string): string[] {
  if (!text) return [];
  const result = text.split('\n');
  if (result.at(-1) === '') result.pop();
  return result;
}

// Myers' algorithm keeps this fast for whole-file ACP snapshots while still
// finding unchanged lines between multiple, unrelated edits.
function operations(before: string[], after: string[]): Operation[] {
  const trace: Map<number, number>[] = [];
  let frontier = new Map<number, number>([[1, 0]]);
  const limit = before.length + after.length;

  for (let distance = 0; distance <= limit; distance += 1) {
    trace.push(new Map(frontier));
    for (let diagonal = -distance; diagonal <= distance; diagonal += 2) {
      let x: number;
      if (diagonal === -distance || (diagonal !== distance && (frontier.get(diagonal - 1) ?? -1) < (frontier.get(diagonal + 1) ?? -1))) {
        x = frontier.get(diagonal + 1) ?? 0;
      } else {
        x = (frontier.get(diagonal - 1) ?? 0) + 1;
      }
      let y = x - diagonal;
      while (x < before.length && y < after.length && before[x] === after[y]) {
        x += 1;
        y += 1;
      }
      frontier.set(diagonal, x);
      if (x >= before.length && y >= after.length) {
        const reversed: Array<{ kind: Operation['kind']; text: string }> = [];
        let oldIndex = before.length;
        let newIndex = after.length;
        for (let d = trace.length - 1; d >= 0; d -= 1) {
          const previous = trace[d];
          const k = oldIndex - newIndex;
          const previousK = k === -d || (k !== d && (previous.get(k - 1) ?? -1) < (previous.get(k + 1) ?? -1)) ? k + 1 : k - 1;
          const previousX = previous.get(previousK) ?? 0;
          const previousY = previousX - previousK;
          while (oldIndex > previousX && newIndex > previousY) {
            reversed.push({ kind: 'context', text: before[oldIndex - 1] });
            oldIndex -= 1;
            newIndex -= 1;
          }
          if (d === 0) break;
          if (oldIndex === previousX) {
            reversed.push({ kind: 'add', text: after[newIndex - 1] });
            newIndex -= 1;
          } else {
            reversed.push({ kind: 'remove', text: before[oldIndex - 1] });
            oldIndex -= 1;
          }
        }
        let oldLine = 1;
        let newLine = 1;
        return reversed.reverse().map((operation) => {
          const annotated = { ...operation, oldLine, newLine };
          if (operation.kind !== 'add') oldLine += 1;
          if (operation.kind !== 'remove') newLine += 1;
          return annotated;
        });
      }
    }
  }
  return [];
}

function range(start: number, count: number): string {
  return count === 1 ? String(start) : `${start},${count}`;
}

export function createUnifiedPatch(path: string, oldText: string, newText: string): string {
  const ops = operations(lines(oldText), lines(newText));
  const changed = ops.flatMap((operation, index) => operation.kind === 'context' ? [] : [index]);
  const output = [`diff --git a/${path} b/${path}`, `--- a/${path}`, `+++ b/${path}`];
  if (!changed.length) return output.join('\n');

  const hunks: Array<[number, number]> = [];
  for (const index of changed) {
    const start = Math.max(0, index - 3);
    const end = Math.min(ops.length, index + 4);
    const previous = hunks.at(-1);
    if (previous && start <= previous[1]) previous[1] = Math.max(previous[1], end);
    else hunks.push([start, end]);
  }

  for (const [start, end] of hunks) {
    const hunk = ops.slice(start, end);
    const oldCount = hunk.filter((operation) => operation.kind !== 'add').length;
    const newCount = hunk.filter((operation) => operation.kind !== 'remove').length;
    output.push(`@@ -${range(hunk[0].oldLine, oldCount)} +${range(hunk[0].newLine, newCount)} @@`);
    output.push(...hunk.map((operation) => `${operation.kind === 'add' ? '+' : operation.kind === 'remove' ? '-' : ' '}${operation.text}`));
  }
  return output.join('\n');
}
