// Guards the two identities that must never drift apart: manifest.json's
// version and the Go sidecar's Metadata.Version (main.go). The DBX host
// refuses to load the plugin when they disagree.
import { readFileSync } from "node:fs";
import { join, dirname } from "node:path";
import { fileURLToPath } from "node:url";

const root = join(dirname(fileURLToPath(import.meta.url)), "..");
const manifest = JSON.parse(readFileSync(join(root, "manifest.json"), "utf8"));
const mainGo = readFileSync(join(root, "backend", "main.go"), "utf8");
const match = mainGo.match(/Version:\s*"([^"]+)"/);
if (!match) {
  console.error("check-version: Metadata.Version not found in backend/main.go");
  process.exit(1);
}
if (manifest.version !== match[1]) {
  console.error(
    `check-version: manifest.json version ${manifest.version} != backend/main.go version ${match[1]}\n` +
      "  bump both together (they must match or the host rejects the sidecar).",
  );
  process.exit(1);
}
console.log(`check-version: ${manifest.version} consistent in manifest.json and main.go`);
