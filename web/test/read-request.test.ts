import { expect, it } from "vitest";
import { createReadRequest } from "../src/shared/read-request";

it("fences late results and cleanup when a transport ignores cancellation", async () => {
  const reads = createReadRequest();
  let resolve!: (value: string) => void;
  let displayed = "";
  const old = reads.begin();
  const delayed = new Promise<string>((done) => {
    resolve = done;
  }).then((value) => {
    if (old.current()) displayed = value;
    return old.finish();
  });
  const latest = reads.begin();
  expect(old.signal.aborted).toBe(true);
  resolve("old");
  expect(await delayed).toBe(false);
  expect(reads.pending).toBe(true);
  expect(displayed).toBe("");
  if (latest.current()) displayed = "new";
  expect(latest.finish()).toBe(true);
  expect(reads.pending).toBe(false);
  expect(displayed).toBe("new");
});

it("keeps independent reads independent and invalidates settled read tokens on disposal", () => {
  const list = createReadRequest();
  const detail = createReadRequest();
  const a = list.begin();
  const b = detail.begin();
  expect(a.finish()).toBe(true);
  list.cancel();
  expect(a.current()).toBe(false);
  expect(b.current()).toBe(true);
  detail.cancel();
  expect(b.signal.aborted).toBe(true);
  expect(b.finish()).toBe(false);
});
