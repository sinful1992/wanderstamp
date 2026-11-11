// Audit 137 WS-8 (team task 141): a version tag must not publish the server
// image unless the tests passed. Reads the workflow files as text (no YAML
// dependency): jobs are the 2-space keys under `jobs:`.
// Run: node --test .github/tests/*.test.mjs
import { test } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";

const wf = join(dirname(fileURLToPath(import.meta.url)), "..", "workflows");
const read = (f) => readFileSync(join(wf, f), "utf8");

// A YAML comment: a whole-line `#`, or ` #` after content (task 142: checks no longer match text
// that only sits in a comment). A `#` inside quotes is rare in these files and stays.
export const stripComment = (line) => (/^\s*#/.test(line) ? "" : line.replace(/\s+#.*$/, ""));
const uncommented = (text) => text.split("\n").map(stripComment).join("\n");

// { name: [lines of that job] } for the top-level `jobs:` block
export function jobs(text) {
  const out = {};
  let inJobs = false, cur = null;
  for (const raw of text.split("\n")) {
    const line = stripComment(raw);
    if (/^jobs:\s*$/.test(line)) { inJobs = true; continue; }
    if (inJobs && /^\S/.test(line)) { inJobs = false; cur = null; continue; }
    if (!inJobs) continue;
    const m = /^  ([A-Za-z0-9_-]+):\s*$/.exec(line);
    if (m) { cur = m[1]; out[cur] = []; continue; }
    if (cur && line.trim()) out[cur].push(line);
  }
  return out;
}

const needsOf = (lines) => {
  const l = lines.find((x) => /^    needs:/.test(x));
  if (!l) return [];
  const v = l.replace(/^    needs:\s*/, "").trim();
  return v.startsWith("[") ? v.slice(1, -1).split(",").map((s) => s.trim()) : [v];
};

// every job that pushes an image or uploads a release (task 142: any build-push-action job counts,
// whatever its push: says, and buildx --push / registry outputs too)
const publishes = (lines) => lines.some((l) =>
  /docker\/build-push-action|push:\s*(true|\$\{\{)|docker push|buildx\b.*--push|type=registry|gh release|softprops\/action-gh-release/.test(l));
// the only accepted guard: exactly this, optionally inside ${{ }}
const TAG_IF = /^    if:\s*(\$\{\{\s*)?startsWith\(github\.ref,\s*'refs\/tags\/v'\)(\s*\}\})?\s*$/;

export function gateProblems(dockerYml, testYml) {
  const p = [];
  const j = jobs(dockerYml);
  testYml = uncommented(testYml);
  const testJob = Object.entries(j).find(([, ls]) => ls.some((l) => /^    uses:\s*["']?\.\/\.github\/workflows\/test\.yml["']?\s*$/.test(l)));
  if (!testJob) p.push("docker.yml has no job that calls ./.github/workflows/test.yml");
  for (const [name, ls] of Object.entries(j)) {
    if (!publishes(ls)) continue;
    if (!testJob || !needsOf(ls).includes(testJob[0])) p.push(`job ${name} publishes without needs: ${testJob ? testJob[0] : "test"}`);
    if (!ls.some((l) => TAG_IF.test(l))) p.push(`job ${name} publishes from any ref (its if: must be exactly startsWith(github.ref, 'refs/tags/v'))`);
    // task 142 (ada #10126 3): two tags at once must not race for :latest
    if (!ls.some((l) => /^    concurrency:/.test(l))) p.push(`job ${name} publishes with no concurrency: group`);
  }
  if (!/workflow_call:/.test(testYml)) p.push("test.yml cannot be called (no workflow_call)");
  for (const [what, re] of [["go vet", /run:\s*go vet \.\/\.\.\./], ["go test", /run:\s*go test\b.*\.\/\.\.\./], ["webtest", /node webtest\/run\.mjs/]]) {
    if (!re.test(testYml)) p.push(`test.yml does not run ${what}`);
  }
  if (/continue-on-error:\s*(true|\$\{\{)/.test(testYml)) p.push("test.yml lets a step fail (continue-on-error)");
  if (/run:.*\|\|\s*(true|:|exit 0)\b/.test(testYml)) p.push("test.yml swallows a failing command (|| true)");
  // a job or step condition can skip a check on tags (ada #10126 a12/a13): test.yml has none
  if (/^\s+(-\s+)?if:/m.test(testYml)) p.push("test.yml has an if: that can skip a check");
  return p;
}

test("the real workflows: a tag publishes only after the tests pass", () => {
  assert.deepEqual(gateProblems(read("docker.yml"), read("test.yml")), []);
});

const GOOD_TEST = "on:\n  workflow_call:\njobs:\n  test:\n    steps:\n      - run: go vet ./...\n      - run: go test -count=1 ./...\n      - run: node webtest/run.mjs\n";
const dock = (publishExtra) => `on:\n  push:\n    tags: ["v*"]\njobs:\n  test:\n    uses: ./.github/workflows/test.yml\n\n  publish:\n${publishExtra}    concurrency: publish\n    runs-on: ubuntu-latest\n    steps:\n      - uses: docker/build-push-action@v7\n        with:\n          push: true\n`;

test("the pre-fix shape (publish with no test job) is caught", () => {
  const before = "on:\n  push:\n    tags: [\"v*\"]\njobs:\n  publish:\n    runs-on: ubuntu-latest\n    steps:\n      - uses: docker/build-push-action@v7\n        with:\n          push: true\n";
  const p = gateProblems(before, "");
  assert.ok(p.some((x) => /no job that calls/.test(x)), p.join("; "));
  assert.ok(p.some((x) => /publishes without needs/.test(x)), p.join("; "));
});

test("needs present, list form, and a missing if", () => {
  assert.deepEqual(gateProblems(dock("    needs: test\n    if: startsWith(github.ref, 'refs/tags/v')\n"), GOOD_TEST), []);
  assert.deepEqual(gateProblems(dock("    needs: [lint, test]\n    if: startsWith(github.ref, 'refs/tags/v')\n"), GOOD_TEST), []);
  assert.match(gateProblems(dock("    needs: test\n"), GOOD_TEST).join(), /from any ref/);
  assert.match(gateProblems(dock("    needs: lint\n    if: startsWith(github.ref, 'refs/tags/v')\n"), GOOD_TEST).join(), /without needs/);
});

test("a test.yml that skips a step or swallows failures is caught", () => {
  const ok = dock("    needs: test\n    if: startsWith(github.ref, 'refs/tags/v')\n");
  assert.match(gateProblems(ok, GOOD_TEST.replace("      - run: go vet ./...\n", "")).join(), /go vet/);
  assert.match(gateProblems(ok, GOOD_TEST.replace("node webtest/run.mjs", "echo skip")).join(), /webtest/);
  assert.match(gateProblems(ok, GOOD_TEST.replace("on:\n  workflow_call:\n", "on:\n  push:\n")).join(), /cannot be called/);
  assert.match(gateProblems(ok, GOOD_TEST + "        continue-on-error: true\n").join(), /continue-on-error/);
});

test("a commented-out needs does not count", () => {
  assert.match(gateProblems(dock("    # needs: test\n    if: startsWith(github.ref, 'refs/tags/v')\n"), GOOD_TEST).join(), /without needs/);
});

// task 142: the 19 shapes a fresh review subagent showed the task 141 gate accepted (ada #10126)
const OK_IF = "    needs: test\n    if: startsWith(github.ref, 'refs/tags/v')\n";
test("an if: that can be true without a tag is refused", () => {
  for (const cond of [
    "always() && startsWith(github.ref, 'refs/tags/v')",
    "!cancelled() && startsWith(github.ref, 'refs/tags/v')",
    "failure() || startsWith(github.ref, 'refs/tags/v')",
    "startsWith(github.ref, 'refs/tags/v') || github.event_name == 'workflow_dispatch'",
    "!startsWith(github.ref, 'refs/tags/v')",
    "contains(github.ref, 'refs/tags/v')",
  ]) {
    assert.match(gateProblems(dock(`    needs: test\n    if: ${cond}\n`), GOOD_TEST).join(), /from any ref/, cond);
  }
  assert.deepEqual(gateProblems(dock("    needs: test\n    if: ${{ startsWith(github.ref, 'refs/tags/v') }}\n"), GOOD_TEST), []);
});

test("other ways to publish count as publishing", () => {
  const job = (step) => `on:\n  push:\njobs:\n  test:\n    uses: ./.github/workflows/test.yml\n  ship:\n    runs-on: ubuntu-latest\n    steps:\n${step}`;
  for (const step of [
    "      - uses: docker/build-push-action@v7\n        with:\n          push: ${{ github.event_name != 'pull_request' }}\n",
    "      - uses: docker/build-push-action@v7\n",
    "      - run: docker buildx build --push -t x .\n",
    "      - uses: docker/build-push-action@v7\n        with:\n          outputs: type=registry\n",
    "      - run: docker push ghcr.io/x\n",
  ]) {
    assert.match(gateProblems(job(step), GOOD_TEST).join(), /ship publishes without needs/, step);
  }
});

test("test.yml tricks are refused", () => {
  const ok = dock(OK_IF);
  assert.match(gateProblems(ok, GOOD_TEST.replace("go test -count=1 ./...", "go test -count=1 ./... || true")).join(), /swallows/);
  assert.match(gateProblems(ok, GOOD_TEST.replace("      - run: go vet ./...\n", "      # - run: go vet ./...\n")).join(), /does not run go vet/);
  assert.match(gateProblems(ok, GOOD_TEST + "      - run: x\n        continue-on-error: ${{ true }}\n").join(), /continue-on-error/);
  assert.match(gateProblems(ok, GOOD_TEST.replace("node webtest/run.mjs", "echo skip  # node webtest/run.mjs")).join(), /does not run webtest/);
});

test("the test job must call exactly test.yml", () => {
  const other = dock(OK_IF).replace("uses: ./.github/workflows/test.yml", "uses: ./.github/workflows/test.yml.noop.yml");
  assert.match(gateProblems(other, GOOD_TEST).join(), /no job that calls/);
  const commented = dock(OK_IF).replace("    uses: ./.github/workflows/test.yml", "    # uses: ./.github/workflows/test.yml\n    runs-on: x");
  assert.match(gateProblems(commented, GOOD_TEST).join(), /no job that calls/);
});

test("a publish job with no concurrency group is refused (two tags race for :latest)", () => {
  const noConc = dock(OK_IF).replace("    concurrency: publish\n", "");
  assert.match(gateProblems(noConc, GOOD_TEST).join(), /no concurrency/);
});

test("an if: anywhere in test.yml is refused (a step or the job could be skipped on tags)", () => {
  const ok = dock(OK_IF);
  assert.match(gateProblems(ok, GOOD_TEST.replace("      - run: node webtest/run.mjs\n", "      - if: github.event_name == 'pull_request'\n        run: node webtest/run.mjs\n")).join(), /if: that can skip/);
  assert.match(gateProblems(ok, GOOD_TEST.replace("  test:\n", "  test:\n    if: \"!startsWith(github.ref, 'refs/tags/')\"\n")).join(), /if: that can skip/);
});
