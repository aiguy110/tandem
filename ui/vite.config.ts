import { defineConfig } from 'vite';
import react from '@vitejs/plugin-react';

// Built assets go to dist/ with relative base so the daemon can serve them from
// any mount point via TANDEM_UI_DIR. The dev server proxies WS/HTTP to a running
// daemon on :7717 so `npm run dev` works against the real backend.
export default defineConfig({
  base: './',
  plugins: [react()],
  server: {
    port: 5178,
    strictPort: false,
  },
  build: {
    target: 'es2022',
    outDir: 'dist',
  },
});
