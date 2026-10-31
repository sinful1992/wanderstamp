// Runs every webtest/*.test.mjs. Each exports `tests`: [name, async ({ browser, base }) => {}].
import { readdirSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath, pathToFileURL } from "node:url";
import { startServer, launch } from "./harness.mjs";

const here = dirname(fileURLToPath(import.meta.url));
const only = process.argv[2];
const files = readdirSync(here).filter((f) => f.endsWith(".test.mjs") && (!only || f.includes(only)));
const server = await startServer();
const browser = await launch();
let pass = 0, fail = 0;
try {
  for (const f of files) {
    const { tests } = await import(pathToFileURL(join(here, f)).href);
    for (const [name, fn] of tests) {
      try {
        await fn({ browser, base: server.base });
        pass++; console.log(`ok    ${f}: ${name}`);
      } catch (e) {
        fail++; console.log(`FAIL  ${f}: ${name}\n      ${String(e && e.stack || e).split("\n").slice(0, 4).join("\n      ")}`);
      }
    }
  }
} finally {
  await browser.close();
  await server.stop();
}
console.log(`\n${pass} passed, ${fail} failed`);
process.exit(fail ? 1 : 0);
