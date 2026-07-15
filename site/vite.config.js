import { defineConfig } from "vite";

// Static marketing site. `npm run build` emits fully static output to site/out,
// matching the deploy contract in the repo-root vercel.json.
export default defineConfig({
  base: "/",
  build: {
    outDir: "out",
    emptyOutDir: true,
    assetsInlineLimit: 2048,
    target: "es2020"
  }
});
