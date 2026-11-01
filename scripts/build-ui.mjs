// UI build entry used by both humans (`npm run build:ui`) and the DBX dev
// host (`ui_build = ["node", "scripts/build-ui.mjs"]`). Running vite through
// its Node API avoids nested npm invocations, which break when the dev host
// passes a \\?\ long-path cwd that cmd.exe cannot use on Windows.
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
import { build } from "vite";

const projectRoot = join(dirname(fileURLToPath(import.meta.url)), "..");
// The dev host spawns this script with a \\?\ long-path cwd; rollup's watcher
// cannot lstat that form, so normalize to the plain drive path first.
process.chdir(projectRoot);
const uiRoot = join(projectRoot, "ui");
const watch = process.argv.includes("--watch") || process.env.DBX_UI_WATCH === "1";

await build({
  root: uiRoot,
  configFile: join(uiRoot, "vite.config.ts"),
  mode: "production",
  build: watch ? { watch: {} } : undefined,
  base: "./",
});
