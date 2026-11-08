// Frontend tests: the real app in headless Chromium against a throwaway local server.
//   node webtest/run.mjs                 (builds the server with `go build` into a temp dir)
//   WS_BIN=/path/to/holidaymap node webtest/run.mjs   (use a binary you already built)
// Needs playwright-core and a Chromium: set PW_CORE to the playwright-core folder
// and CHROME to a chromium/headless-shell executable if they aren't found on their own.
import { spawn, execFileSync } from "node:child_process";
import { mkdtempSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join, dirname } from "node:path";
import { createRequire } from "node:module";
import { fileURLToPath, pathToFileURL } from "node:url";
import net from "node:net";

const here = dirname(fileURLToPath(import.meta.url));
export const USER = "tester", PASS = "webtest-pass-1234";

async function loadPlaywright() {
  const req = createRequire(import.meta.url);
  const spec = process.env.PW_CORE || "playwright-core";
  let path;
  try { path = req.resolve(spec); } catch { path = req.resolve("playwright"); }
  const m = await import(pathToFileURL(path).href);
  return m.chromium || m.default.chromium;
}

function freePort() {
  return new Promise((res) => {
    const s = net.createServer();
    s.listen(0, "127.0.0.1", () => { const p = s.address().port; s.close(() => res(p)); });
  });
}

export async function startServer() {
  const dir = mkdtempSync(join(tmpdir(), "wswebtest-"));
  let bin = process.env.WS_BIN;
  if (!bin) {
    bin = join(dir, process.platform === "win32" ? "holidaymap.exe" : "holidaymap");
    execFileSync("go", ["build", "-o", bin, "."], { cwd: join(here, ".."), stdio: "inherit" });
  }
  const port = await freePort();
  const tvPort = await freePort(); // task 133: the TV door (TV_LISTEN_ADDR)
  const proc = spawn(bin, [], {
    env: {
      ...process.env,
      LISTEN_ADDR: `127.0.0.1:${port}`, TV_LISTEN_ADDR: `127.0.0.1:${tvPort}`, DB_PATH: join(dir, "t.db"), COOKIE_SECURE: "false",
      ADMIN_USERNAME: USER, ADMIN_PASSWORD: PASS, IMMICH_URL: "http://127.0.0.1:1", IMMICH_API_KEY: "",
    },
    stdio: ["ignore", "ignore", "pipe"],
  });
  let log = "";
  proc.stderr.on("data", (d) => { log += d; });
  const base = `http://127.0.0.1:${port}`;
  for (let i = 0; i < 100; i++) {
    try { if ((await fetch(base + "/healthz")).ok) break; } catch {}
    await new Promise((r) => setTimeout(r, 100));
    if (i === 99) throw new Error("server did not start:\n" + log);
  }
  return {
    base,
    tvBase: `http://127.0.0.1:${tvPort}`,
    async stop() {
      proc.kill();
      await new Promise((r) => proc.once("exit", r));
      try { rmSync(dir, { recursive: true, force: true }); } catch {}
    },
  };
}

export async function launch() {
  const chromium = await loadPlaywright();
  return chromium.launch(process.env.CHROME ? { executablePath: process.env.CHROME } : {});
}

// A signed-in page. opts go to newContext (timezoneId, locale, viewport…).
export async function signedIn(browser, base, opts = {}) {
  const ctx = await browser.newContext({ locale: "en-GB", serviceWorkers: "block", ...opts });
  const page = await ctx.newPage();
  await page.goto(base, { waitUntil: "networkidle" });
  await page.fill("#login-user", USER);
  await page.fill("#login-pass", PASS);
  await page.click("#login-form button[type=submit]");
  await page.waitForSelector("#login", { state: "hidden" });
  return { ctx, page };
}

export function api(page, method, url, body) {
  return page.evaluate(async ([m, u, b]) => {
    const r = await fetch(u, { method: m, headers: { "Content-Type": "application/json" }, body: b === undefined ? undefined : JSON.stringify(b) });
    const t = await r.text();
    let j; try { j = JSON.parse(t); } catch { j = t; }
    return { status: r.status, body: j };
  }, [method, url, body]);
}
