import { mountSetup } from "./renderer";
import { ResponseError } from "@/api/transport";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { flushTicks } from "./helpers";

const mocks = vi.hoisted(() => ({
  createAgentRollout: vi.fn(),
  onStarted: vi.fn(),
}));
vi.mock("../src/api/agents", () => ({
  createAgentRollout: mocks.createAgentRollout,
}));
vi.mock("vue-i18n", () => ({
  useI18n: () => ({ t: (key: string) => key }),
}));
import NodeRolloutDialog from "../src/components/nodes/NodeRolloutDialog.vue";

interface View {
  rolloutDialog: boolean;
  rolloutReason: string;
  rolloutStarting: boolean;
  rolloutError: string;
  openRolloutDialog(): void;
  submitRollout(): Promise<void>;
}

let unmount: (() => void) | undefined;
async function mount(): Promise<View> {
  const mounted = mountSetup(NodeRolloutDialog, {
    selectedNodes: [
      {
        id: "node-a",
        name: "Node A",
        agentUpgradeEligible: true,
        recommendedAgentVersion: "0.2.0",
      },
    ],
    onStarted: mocks.onStarted,
  });
  unmount = mounted.unmount;
  await flushTicks();
  const view = mounted.view as View;
  view.rolloutDialog = true;
  view.rolloutReason = "first";
  return view;
}
function created() {
  let resolve!: (value: { id: string }) => void;
  mocks.createAgentRollout.mockReturnValue(
    new Promise((done) => {
      resolve = done;
    }),
  );
  return (id: string) => {
    resolve({ id });
  };
}

beforeEach(() => {
  vi.resetAllMocks();
});
afterEach(() => unmount?.());

it("navigates to an accepted rollout from the page that submitted it", async () => {
  mocks.createAgentRollout.mockResolvedValue({ id: "rollout-a" });
  const view = await mount();
  await view.submitRollout();
  expect(mocks.onStarted).toHaveBeenCalledExactlyOnceWith("rollout-a");
  expect(view.rolloutReason).toBe("");
});

it("keeps a late create from navigating after the page unmounts", async () => {
  const resolve = created();
  const view = await mount();
  const submit = view.submitRollout();
  unmount?.();
  resolve("rollout-a");
  await submit;
  expect(mocks.createAgentRollout).toHaveBeenCalledTimes(1);
  expect(mocks.onStarted).not.toHaveBeenCalled();
});

it("reports a lost create response as unconfirmed without resending", async () => {
  mocks.createAgentRollout.mockRejectedValue(new TypeError("network"));
  const view = await mount();
  await view.submitRollout();
  expect(view.rolloutError).toBe("rolloutStartUnconfirmed");
  expect(view.rolloutDialog).toBe(true);
  expect(mocks.createAgentRollout).toHaveBeenCalledTimes(1);
});

function rejection(body: string, status = 409) {
  return new ResponseError(
    new Response(body, {
      status,
      headers: { "Content-Type": "application/problem+json" },
    }),
    "Response returned an error code",
  );
}

it("shows a definite server rejection by its Problem type", async () => {
  mocks.createAgentRollout.mockRejectedValueOnce(
    rejection(
      JSON.stringify({
        type: "https://ocservia.dev/problems/forbidden",
        title: "Approval required",
        status: 409,
      }),
    ),
  );
  mocks.createAgentRollout.mockRejectedValueOnce(
    rejection(
      JSON.stringify({
        type: "https://ocservia.dev/problems/no-eligible-nodes",
        title: "No eligible nodes",
        status: 409,
      }),
    ),
  );
  mocks.createAgentRollout.mockRejectedValueOnce(
    rejection(
      JSON.stringify({
        type: "https://ocservia.dev/problems/invalid-request",
        title: "Request is invalid",
        status: 400,
      }),
      400,
    ),
  );
  const view = await mount();
  await view.submitRollout();
  expect(view.rolloutError).toBe("rolloutForbidden");
  await view.submitRollout();
  expect(view.rolloutError).toBe("rolloutNoEligibleNodes");
  await view.submitRollout();
  expect(view.rolloutError).toBe("rolloutRequestInvalid");
});

it("never shows server text for an unknown or unreadable Problem", async () => {
  mocks.createAgentRollout.mockRejectedValueOnce(
    rejection(
      JSON.stringify({
        type: "https://ocservia.dev/problems/service-unavailable",
        title: "Service is unavailable",
        detail: "internal detail",
        status: 503,
      }),
      503,
    ),
  );
  mocks.createAgentRollout.mockRejectedValueOnce(rejection("not json"));
  mocks.createAgentRollout.mockRejectedValueOnce(
    rejection(JSON.stringify({ type: "toString", title: "x", status: 409 })),
  );
  const view = await mount();
  for (let attempt = 0; attempt < 3; attempt += 1) {
    await view.submitRollout();
    expect(view.rolloutError).toBe("rolloutStartRejected");
  }
});

it("drops a rejection whose Problem arrives after the view unmounts", async () => {
  let body!: (value: unknown) => void;
  const response = {
    status: 409,
    json: () =>
      new Promise((done) => {
        body = done;
      }),
  } as unknown as Response;
  mocks.createAgentRollout.mockRejectedValue(
    new ResponseError(response, "Response returned an error code"),
  );
  const view = await mount();
  const submit = view.submitRollout();
  await vi.waitFor(() => {
    expect(body).toBeDefined();
  });
  unmount?.();
  body({ type: "https://ocservia.dev/problems/forbidden" });
  await submit;
  expect(view.rolloutError).toBe("");
});

it("gives up navigation when the dialog closes, keeping the request", async () => {
  const resolve = created();
  const view = await mount();
  const submit = view.submitRollout();
  view.rolloutDialog = false;
  resolve("rollout-a");
  await submit;
  expect(mocks.createAgentRollout).toHaveBeenCalledTimes(1);
  expect(mocks.onStarted).not.toHaveBeenCalled();
  expect(view.rolloutReason).toBe("first");
  expect(view.rolloutStarting).toBe(false);
});

it("keeps a reopened dialog's input when the earlier create lands", async () => {
  const resolve = created();
  const view = await mount();
  const submit = view.submitRollout();
  view.rolloutDialog = false;
  view.openRolloutDialog();
  expect(view.rolloutStarting).toBe(true);
  view.rolloutReason = "second";
  resolve("rollout-a");
  await submit;
  expect(mocks.onStarted).not.toHaveBeenCalled();
  expect(view.rolloutDialog).toBe(true);
  expect(view.rolloutReason).toBe("second");
});

it("shows an unconfirmed start from a closed dialog when it reopens", async () => {
  let reject!: (cause: unknown) => void;
  mocks.createAgentRollout.mockReturnValue(
    new Promise((_, fail) => {
      reject = fail;
    }),
  );
  const view = await mount();
  const submit = view.submitRollout();
  view.rolloutDialog = false;
  reject(new TypeError("network"));
  await submit;
  view.openRolloutDialog();
  expect(view.rolloutError).toBe("rolloutStartUnconfirmed");
  expect(mocks.createAgentRollout).toHaveBeenCalledTimes(1);
});

it("clears a definite failure when the dialog reopens", async () => {
  mocks.createAgentRollout.mockRejectedValue(
    new ResponseError(new Response("{}", { status: 409 }), "conflict"),
  );
  const view = await mount();
  await view.submitRollout();
  view.rolloutDialog = false;
  view.openRolloutDialog();
  expect(view.rolloutError).toBe("");
});

it("does not report a navigation failure as an unconfirmed create", async () => {
  mocks.createAgentRollout.mockResolvedValue({ id: "rollout-a" });
  mocks.onStarted.mockRejectedValue(new Error("navigation"));
  const view = await mount();
  await expect(view.submitRollout()).rejects.toThrow("navigation");
  expect(view.rolloutError).toBe("");
  expect(view.rolloutStarting).toBe(false);
  expect(view.rolloutDialog).toBe(false);
  expect(mocks.createAgentRollout).toHaveBeenCalledTimes(1);
});

// The first POST may have created the rollout, so a
// definite rejection of an explicit retry does not settle the earlier start.
it("keeps an unconfirmed start visible when an explicit retry is rejected", async () => {
  mocks.createAgentRollout.mockRejectedValueOnce(new TypeError("network"));
  mocks.createAgentRollout.mockRejectedValueOnce(
    new ResponseError(
      new Response("{}", { status: 409 }),
      "Response returned an error code",
    ),
  );
  const view = await mount();
  await view.submitRollout();
  await view.submitRollout();
  expect(mocks.createAgentRollout).toHaveBeenCalledTimes(2);
  expect(view.rolloutError).toBe("rolloutStartUnconfirmed");
  view.rolloutDialog = false;
  view.openRolloutDialog();
  expect(view.rolloutError).toBe("rolloutStartUnconfirmed");
});

it("keeps an unconfirmed start visible while an explicit retry is pending", async () => {
  mocks.createAgentRollout.mockRejectedValueOnce(new TypeError("network"));
  const view = await mount();
  await view.submitRollout();
  const resolve = created();
  const retry = view.submitRollout();
  expect(view.rolloutError).toBe("rolloutStartUnconfirmed");
  view.rolloutDialog = false;
  view.openRolloutDialog();
  expect(view.rolloutError).toBe("rolloutStartUnconfirmed");
  resolve("rollout-b");
  await retry;
  expect(mocks.createAgentRollout).toHaveBeenCalledTimes(2);
  view.openRolloutDialog();
  expect(view.rolloutError).toBe("");
});

it("settles an unconfirmed start once a later create is confirmed", async () => {
  mocks.createAgentRollout.mockRejectedValueOnce(new TypeError("network"));
  mocks.createAgentRollout.mockResolvedValueOnce({ id: "rollout-b" });
  const view = await mount();
  await view.submitRollout();
  await view.submitRollout();
  expect(mocks.onStarted).toHaveBeenCalledWith("rollout-b");
  view.openRolloutDialog();
  expect(view.rolloutError).toBe("");
});

it("uses pre-parsed Problem type without reading the response body twice", async () => {
  const json = vi.fn();
  mocks.createAgentRollout.mockRejectedValueOnce(
    new ResponseError(
      { status: 403, json } as unknown as Response,
      "Forbidden",
      "https://ocservia.dev/problems/forbidden",
    ),
  );
  const view = await mount();
  await view.submitRollout();
  expect(view.rolloutError).toBe("rolloutForbidden");
  expect(json).not.toHaveBeenCalled();
});
