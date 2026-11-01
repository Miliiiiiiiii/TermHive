// Gate: every released version must have a CHANGELOG entry. Runs in
// `npm run check` and CI — a version bump without a changelog entry fails
// the build (docs/VERSIONING.md).
import { readFileSync } from "node:fs";
import { join, dirname } from "node:path";
import { fileURLToPath } from "node:url";

const root = join(dirname(fileURLToPath(import.meta.url)), "..");
const manifest = JSON.parse(readFileSync(join(root, "manifest.json"), "utf8"));
let changelog;
try {
  changelog = readFileSync(join(root, "CHANGELOG.md"), "utf8");
} catch {
  console.error("check-changelog: CHANGELOG.md is missing");
  process.exit(1);
}
const version = String(manifest.version);
// prerelease builds (x.y.z-rc.n) inherit the entry of their base version
const base = version.replace(/-rc\.\d+$/, "");
if (!changelog.includes(`## [${base}]`)) {
  console.error(`check-changelog: no "## [${base}]" entry in CHANGELOG.md for current version ${version}`);
  console.error("  add the entry, or bump via `npm run release:patch|minor` (docs/VERSIONING.md)");
  process.exit(1);
}
console.log(`check-changelog: CHANGELOG covers ${version}`);
