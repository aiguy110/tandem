import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import { test } from "node:test";
import { fileURLToPath } from "node:url";

test("runner loads a TypeScript importer and emits versioned session frames", () => {
  const runner = fileURLToPath(new URL("./runner.ts", import.meta.url));
  const fixture = fileURLToPath(new URL("./testdata/fixture.ts", import.meta.url));
  const request = {
    protocolVersion: 1,
    agent: "custom",
    checkpoints: [
      { importerId: "other", importerVersion: 1, sourceKey: "ignored", checkpoint: 1 },
      { importerId: "sdk-fixture", importerVersion: 2, sourceKey: "source", checkpoint: { offset: 7 } },
    ],
  };
  const result = spawnSync(
    process.execPath,
    ["--import", "tsx", runner, fixture, "--fixture"],
    { input: `${JSON.stringify(request)}\n`, encoding: "utf8" },
  );
  assert.equal(result.status, 0, result.stderr);
  const records = result.stdout.trim().split("\n").map((line) => JSON.parse(line));
  assert.deepEqual(records.map(({ type }) => type), [
    "hello", "begin_session", "entry", "end_session",
  ]);
  assert.deepEqual(records[0], {
    type: "hello",
    protocolVersion: 1,
    importer: { id: "sdk-fixture", version: 2 },
  });
  assert.equal(records[1].session.id, "session");
  assert.equal(records[2].entry.text, "hello history");
  assert.deepEqual(records[3].checkpoint, { offset: 8 });
});
