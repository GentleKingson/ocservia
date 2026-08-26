import { describe, expect, it } from "vitest";

import {
  completeOperationDetail,
  failOperationDetail,
  startOperationDetail,
} from "../src/shared/operation-detail";

describe("operation detail state", () => {
  it("does not retain stale detail across a failed then successful switch", () => {
    const first = { id: "operation-a" };
    const second = { id: "operation-b" };

    expect(completeOperationDetail(first)).toEqual({
      selected: first,
      error: "",
    });

    let state = failOperationDetail("Operation details are unavailable");
    expect(state).toEqual({
      error: "Operation details are unavailable",
    });

    expect(startOperationDetail()).toEqual({ error: "" });
    state = completeOperationDetail(second);

    expect(state).toEqual({
      selected: second,
      error: "",
    });
  });
});
