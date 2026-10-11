import type {
  NodeObservedState,
  UserGroupResourceState,
} from "@ocservia/api-client";
import { createSSRApp, h } from "vue";
import { renderToString } from "vue/server-renderer";
import { expect, it, vi } from "vitest";

vi.mock("vue-i18n", () => ({
  useI18n: () => ({ t: (key: string) => key, locale: { value: "en" } }),
}));
import NodeDetailUsersGroupsSection from "../src/components/nodes/NodeDetailUsersGroupsSection.vue";
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

it("disables user actions and explains why when user.manage is denied", async () => {
  const users = [
    {
      kind: "user",
      name: "alice",
      desiredEnabled: true,
      desiredVersion: 4,
      convergence: "converged",
      recoveryRequired: false,
    },
    {
      kind: "user",
      name: "bob",
      desiredVersion: 2,
      convergence: "drifted",
      recoveryRequired: true,
      recoveryMutationKind: "user_password_rotate",
    },
  ] as UserGroupResourceState[];
  const section = (allowed: boolean, busy = false) =>
    render(NodeDetailUsersGroupsSection, {
      users,
      groups: [],
      busy,
      userAction: { allowed, explanation: allowed ? "" : "action_forbidden" },
      groupAction: { allowed: true, explanation: "" },
    });
  const disabledLabels = (html: string) =>
    [...html.matchAll(/<button[^>]*>/g)]
      .map((match) => match[0])
      .filter((tag) => / disabled[ >=]/.test(tag))
      .map((tag) => /aria-label="([^"]*)"/.exec(tag)?.[1]);

  const denied = await section(false);
  expect(denied).toContain("action_forbidden");
  // Everything that writes a user is disabled; quota needs only a version.
  expect(disabledLabels(denied).sort()).toEqual([
    "createUser",
    "disableUser",
    "retryRotatePassword",
    "rotatePassword",
  ]);

  const allowed = await section(true);
  expect(allowed).not.toContain("action_forbidden");
  expect(disabledLabels(allowed)).toEqual([]);
  // The recovery row offers only its retry, not the routine actions.
  expect(allowed.match(/aria-label="rotatePassword"/g)).toHaveLength(1);

  const busy = await section(true, true);
  expect(disabledLabels(busy)).toHaveLength(6);
});
