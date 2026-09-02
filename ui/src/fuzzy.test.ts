import { describe, it, expect } from 'vitest';
import { fuzzyFilter, fuzzyFilterFields } from './fuzzy';

type Repo = { name: string; path: string };
const repos: Repo[] = [
  { name: 'pi', path: '/home/josiah/Projects/pi' },
  { name: 'add', path: '/home/josiah/Projects/add' },
  { name: 'gcc', path: '/home/josiah/Projects/gcc' },
  { name: 'home-base', path: '/home/josiah/.tandem/home-base' },
];
const byName = (repo: Repo): [string, number][] => [[repo.name, 2], [repo.path, 1]];

describe('fuzzyFilterFields', () => {
  it('ranks a name hit above a path hit that scores the same', () => {
    // Every repo lives under /home/josiah, so "home" matches all of them; only
    // home-base matches on its name.
    expect(fuzzyFilterFields('home', repos, byName)[0].name).toBe('home-base');
    // The unweighted scorer buries it under the shorter paths.
    expect(fuzzyFilter('home', repos, (r) => r.name + ' ' + r.path)[0].name).not.toBe('home-base');
  });

  it('still matches on path when the name does not', () => {
    expect(fuzzyFilterFields('projects/gcc', repos, byName).map((r) => r.name)).toEqual(['gcc']);
  });

  it('drops items no field matches, and passes everything through for an empty query', () => {
    expect(fuzzyFilterFields('zzz', repos, byName)).toEqual([]);
    expect(fuzzyFilterFields('', repos, byName)).toEqual(repos);
  });
});
