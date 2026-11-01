// The ONLY sanctioned way to bump the plugin version: writes manifest.json
// and backend/main.go consistently and requires the CHANGELOG entry to
// already exist. Usage: node scripts/release.mjs patch|minor
import { readFileSync, writeFileSync } from "node:fs";
import { join, dirname } from "node:path";
import { fileURLToPath } from "node:url";

const root = join(dirname(fileURLToPath(import.meta.url)), "..");
const kind = process.argv[2];
if (kind !== "patch" && kind !== "minor") {
  console.error("usage: node scripts/release.mjs patch|minor");
  process.exit(1);
}

const manifestPath = join(root, "manifest.json");
const mainPath = join(root, "backend", "main.go");
const manifest = JSON.parse(readFileSync(manifestPath, "utf8"));
const current = String(manifest.version || "");
const match = current.match(/^(\d+)\.(\d+)\.(\d+)$/);
if (!match) {
  console.error(`release: current version "${current}" is not a plain x.y.z (settle prerelease tags first)`);
  process.exit(1);
}
let [major, minor, patch] = match.slice(1).map(Number);
if (kind === "minor") {
  minor += 1;
  patch = 0;
} else {
  patch += 1;
}
const next = `${major}.${minor}.${patch}`;

const changelog = readFileSync(join(root, "CHANGELOG.md"), "utf8");
if (!changelog.includes(`## [${next}]`)) {
  console.error(
    `release: CHANGELOG.md has no "## [${next}]" entry.\n` +
      "  write the changelog entry FIRST, then run the release script (docs/VERSIONING.md).",
  );
  process.exit(1);
}

writeFileSync(manifestPath, readFileSync(manifestPath, "utf8").replace(`"version": "${current}"`, `"version": "${next}"`));
const mainGo = readFileSync(mainPath, "utf8");
if (!mainGo.includes(`Version:      "${current}"`)) {
  // revert the manifest bump so both files stay in sync
  writeFileSync(manifestPath, readFileSync(manifestPath, "utf8").replace(`"version": "${next}"`, `"version": "${current}"`));
  console.error(`release: backend/main.go does not carry Version: "${current}" — fix manually and retry`);
  process.exit(1);
}
writeFileSync(mainPath, mainGo.replace(`Version:      "${current}"`, `Version:      "${next}"`));

console.log(`release: ${current} → ${next} (manifest.json + backend/main.go)`);
console.log("next: npm test && npm run package");
