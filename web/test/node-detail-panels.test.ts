import type { NodeObservedState } from "@ocservia/api-client";
import { createSSRApp, h } from "vue";
import { renderToString } from "vue/server-renderer";
import { expect, it, vi } from "vitest";

vi.mock("vue-i18n", () => ({
  useI18n: () => ({ t: (key: string) => key, locale: { value: "en" } }),
}));
import NodeObservedDetails from "../src/components/nodes/NodeObservedDetails.vue";
import NodeStatusSummary from "../src/components/nodes/NodeStatusSummary.vue";

// Only the fields the API always returns; every optional observation is absent.
const sparseNode = {
  id: "node-a",
  name: "Sparse node",
  version: 1,
  trustStatus: "active",
  connectionState: "offline",
  freshness: "stale",
} as NodeObservedState;

async function render(component: unknown, props: Record<string, unknown>) {
  const app = createSSRApp({ render: () => h(component as never, props) });
  app.config.globalProperties.$t = (key: string) => key;
  return renderToString(app);
}

it("labels unknown observations instead of rendering empty values", async () => {
  const details = await render(NodeObservedDetails, { node: sparseNode });
  expect(details).toContain("notAvailable");
  expect(details).toContain("recommendationNotConfigured");
  expect(details).toContain("notObserved");
  expect(details).not.toContain("undefined");
  // Identities without a value have nothing to copy.
  expect(details).not.toContain("copyValue");

  const summary = await render(NodeStatusSummary, {
    node: sparseNode,
    sessionCount: 0,
  });
  expect(summary).toContain("notObserved");
  expect(summary).toContain("unknown");
  expect(summary).not.toContain("undefined");
});

it("offers copy for long identity values", async () => {
  const details = await render(NodeObservedDetails, {
    node: { ...sparseNode, bootId: "b".repeat(64), agentInstanceId: "agent-1" },
  });
  expect(details).toContain("b".repeat(64));
  expect(details).toContain("break-all");
  expect(details.match(/copyValue/g)?.length).toBe(4); // aria-label + title ×2
});
