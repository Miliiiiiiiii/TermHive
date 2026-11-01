import { defineConfig } from "vite";
import { viteSingleFile } from "vite-plugin-singlefile";

// The DBX sandbox serves the UI from a single document (the dev host inlines
// every script/style and CSP blocks network fetches), so the build must be
// one self-contained index.html.
export default defineConfig({
  base: "./",
  plugins: [viteSingleFile()],
  build: {
    target: "es2022",
    assetsInlineLimit: 100_000_000,
    cssCodeSplit: false,
    outDir: "dist",
    emptyOutDir: true,
  },
});
