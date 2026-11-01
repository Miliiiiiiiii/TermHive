// Asserts the built UI bundle actually contains the design system. Guards
// against the 0.2.x regression where a refactor dropped `import "./style.css"`
// and shipped a completely unstyled build that only surfaced in the real
// client. Run after `npm run build:ui`.
import { readFileSync, statSync } from "node:fs";
import { join, dirname } from "node:path";
import { fileURLToPath } from "node:url";

const root = join(dirname(fileURLToPath(import.meta.url)), "..");
const entry = join(root, "ui", "dist", "index.html");

let raw;
try {
  statSync(entry);
  raw = readFileSync(entry, "utf8");
} catch {
  console.error("check-build: ui/dist/index.html missing — run `npm run build:ui` first");
  process.exit(1);
}

// The bundle inlines one <style> (vite-plugin-singlefile); these selectors
// come from ui/src/style.css and must survive every refactor.
const required = [".icon-btn", ".files-wrap", ".fp-list", ".launcher-hero", "[hidden]{display:none"];
const missing = required.filter((marker) => !raw.includes(marker));
if (missing.length) {
  console.error("check-build: built UI is missing critical CSS markers:", missing.join(", "));
  console.error("  is `import \"./style.css\"` still the first import in ui/src/main.ts?");
  process.exit(1);
}
console.log(`check-build: design system present in ui/dist/index.html (${required.length} markers)`);
