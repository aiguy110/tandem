// Copy ghostty-web's WASM engine into public/ so Vite serves it as a static
// asset (mirrors spike/terminal's predev step). Kept out of git via .gitignore.
import fs from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

const here = path.dirname(fileURLToPath(import.meta.url));
const src = path.join(here, '..', 'node_modules', 'ghostty-web', 'ghostty-vt.wasm');
const dest = path.join(here, '..', 'public', 'ghostty-vt.wasm');

if (!fs.existsSync(src)) {
  console.warn('[copy-wasm] ghostty-vt.wasm not found — run npm install first. Terminal will use the xterm.js fallback.');
  process.exit(0);
}
fs.mkdirSync(path.dirname(dest), { recursive: true });
fs.copyFileSync(src, dest);
console.log('[copy-wasm] ghostty-vt.wasm -> public/');
