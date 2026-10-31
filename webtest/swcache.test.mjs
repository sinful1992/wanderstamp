// P5: the service worker's cache name is a build hash of static/, so any changed
// file (Leaflet, a font, an icon) gets a fresh cache. Runs the Dockerfile's own
// RUN line on a temp copy of static/; needs sh, find, sort, xargs, sha1sum, sed.
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import { cpSync, mkdtempSync, readFileSync, rmSync, appendFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";

const root = join(dirname(fileURLToPath(import.meta.url)), "..");

function runLine() {
  const df = readFileSync(join(root, "Dockerfile"), "utf8");
  const line = df.split(/\r?\n/).find((l) => l.startsWith("RUN ") && l.includes("__BUILD__"));
  assert.ok(line, "the Dockerfile has no RUN line that fills __BUILD__");
  return line.slice(4);
}

function build(dir) {
  execFileSync("sh", ["-c", runLine()], { cwd: dir, stdio: "pipe" });
  const m = readFileSync(join(dir, "static", "sw.js"), "utf8").match(/const CACHE = "shell-([^"]+)"/);
  assert.ok(m, "no CACHE line in sw.js");
  return m[1];
}

export const tests = [
  ["P5: sw.js has the placeholder, and a Leaflet change gives a new cache name", async () => {
    assert.match(readFileSync(join(root, "static", "sw.js"), "utf8"), /const CACHE = "shell-__BUILD__";/);
    const a = mkdtempSync(join(tmpdir(), "swp5-")), b = mkdtempSync(join(tmpdir(), "swp5-"));
    try {
      cpSync(join(root, "static"), join(a, "static"), { recursive: true });
      cpSync(join(root, "static"), join(b, "static"), { recursive: true });
      const h1 = build(a);
      assert.match(h1, /^[0-9a-f]{8}$/);
      appendFileSync(join(b, "static", "leaflet", "leaflet.css"), "\n/* 1.9.5 */\n");
      const h2 = build(b);
      assert.notEqual(h1, h2);
    } finally {
      rmSync(a, { recursive: true, force: true });
      rmSync(b, { recursive: true, force: true });
    }
  }],
];
