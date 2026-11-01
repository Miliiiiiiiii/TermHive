// The ONLY sanctioned way to bump the plugin version: writes manifest.json
// and backend/main.go consistently and requires the CHANGELOG entry to
// already exist. Usage: node scripts/release.mjs patch|minor|rc|settle
// - patch/minor: bump to the next released x.y.z
// - rc: stamp the next prerelease build of the CURRENT x.y.z
//       (0.1.0 -> 0.1.0-rc.1 -> 0.1.0-rc.2 ...), appending a one-line build
//       record under the base version's CHANGELOG entry
// - settle: drop the rc tag when the x.y.z actually ships (0.1.0-rc.3 -> 0.1.0)
import { readFileSync, writeFileSync } from "node:fs";
import { join, dirname } from "node:path";
import { fileURLToPath } from "node:url";

const root = join(dirname(fileURLToPath(import.meta.url)), "..");
const kind = process.argv[2];
if (kind !== "patch" && kind !== "minor" && kind !== "rc" && kind !== "settle") {
  console.error("usage: node scripts/release.mjs patch|minor|rc|settle");
  process.exit(1);
}

const manifestPath = join(root, "manifest.json");
const mainPath = join(root, "backend", "main.go");
const changelogPath = join(root, "CHANGELOG.md");
const manifest = JSON.parse(readFileSync(manifestPath, "utf8"));
const current = String(manifest.version || "");
let next; // version to write
let base; // x.y.z whose CHANGELOG entry must exist

if (kind === "rc" || kind === "settle") {
  const match = current.match(/^(\d+\.\d+\.\d+)(?:-rc\.(\d+))?$/);
  if (!match) {
    console.error(`release: current version "${current}" is not x.y.z or x.y.z-rc.N`);
    process.exit(1);
  }
  base = match[1];
  if (kind === "rc") {
    next = `${base}-rc.${Number(match[2] || 0) + 1}`;
  } else {
    if (!match[2]) {
      console.error(`release: nothing to settle — current version "${current}" has no rc tag`);
      process.exit(1);
    }
    next = base;
  }
} else {
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
  next = `${major}.${minor}.${patch}`;
  base = next;
}

let changelog = readFileSync(changelogPath, "utf8");
if (!changelog.includes(`## [${base}]`)) {
  console.error(
    `release: CHANGELOG.md has no "## [${base}]" entry.\n` +
      "  write the changelog entry FIRST, then run the release script (docs/VERSIONING.md).",
  );
  process.exit(1);
}

// rc builds get a one-line build record appended to their base version's entry
if (kind === "rc") {
  const date = new Date().toISOString().slice(0, 10);
  const anchor = `## [${base}]`;
  const start = changelog.indexOf(anchor);
  let end = changelog.indexOf("\n## ", start);
  if (end === -1) end = changelog.length;
  changelog =
    changelog.slice(0, end).replace(/\s*$/, "") +
    `\n\n> rc 验证构建：${next}（${date}）\n` +
    changelog.slice(end);
  writeFileSync(changelogPath, changelog);
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
