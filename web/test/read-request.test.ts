import { afterEach, expect, it, vi } from "vitest";
const workspace = vi.hoisted(() => ({ id: "a", generation: 1 }));
vi.mock("../src/api/workspace", async (original) => ({
  ...(await original<typeof import("../src/api/workspace")>()),
  workspaceContext: () => workspace,
}));
import { createReadRequest } from "../src/shared/read-request";

afterEach(() => {
  workspace.id = "a";
  workspace.generation = 1;
});

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

it("rejects an old workspace generation even after switching back to the same id", () => {
  const reads = createReadRequest();
  const request = reads.begin();
  const context = { ...workspace };
  workspace.generation += 2;
  expect(request.current(context)).toBe(false);
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
