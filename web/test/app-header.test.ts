import {
  createRenderer,
  createSSRApp,
  h,
  nextTick,
  ref,
  ssrContextKey,
} from "vue";
import { renderToString } from "vue/server-renderer";
import { expect, it, vi } from "vitest";
vi.mock("vue-router", () => ({
  useRoute: () => ({ fullPath: "/", path: "/" }),
}));
import AppHeader from "../src/components/layout/AppHeader.vue";

const workspace = (id: string, name: string) => ({
  id,
  name,
  slug: id,
  version: 1,
});

async function renderHeader(
  props: Record<string, unknown> = {},
): Promise<string> {
  const app = createSSRApp({
    render: () =>
      h(AppHeader, {
        workspaces: [],
        workspaceId: "",
        readiness: "ready",
        ...props,
      }),
  });
  app.config.globalProperties.$t = (key: string) => `[${key}]`;
  app.component("RouterLink", { template: "<a><slot /></a>" });
  return renderToString(app);
}

async function readinessText(
  readiness: "loading" | "ready" | "unavailable",
): Promise<string> {
  const html = await renderHeader({ readiness });
  return (
    html.match(/data-testid="readiness"[^>]*>([\s\S]*?)<\/div>/)?.[1] ?? ""
  );
}

it("keeps readiness neutral until the first check settles", async () => {
  const loading = await readinessText("loading");
  expect(loading).toContain("[loading]");
  expect(loading).not.toContain("bg-destructive");
  expect(await readinessText("ready")).toContain("[ready]");
  expect(await readinessText("unavailable")).toContain("bg-destructive");
});

it("shows a single Workspace as text instead of a locked selector", async () => {
  const html = await renderHeader({
    workspaces: [workspace("a", "Administration")],
    workspaceId: "a",
  });
  expect(html).toContain("Administration");
  expect(html).not.toContain("<select");
});

it("offers a selector when several Workspaces are authorized", async () => {
  const html = await renderHeader({
    workspaces: [workspace("a", "Alpha"), workspace("b", "Beta")],
    workspaceId: "a",
  });
  const select = html.match(/<select id="workspace-select"[^>]*>/)?.[0];
  expect(select).toBeDefined();
  expect(select).not.toContain("disabled");
});

// A select only reports a choice; the shown Workspace changes once its owner
// confirms it, so a rejected change keeps showing the current one.
it("keeps showing the owner's Workspace until the owner confirms a change", async () => {
  const renderer = createRenderer<object, object>({
    createElement: () => ({}),
    createText: () => ({}),
    createComment: () => ({}),
    insert: () => {},
    remove: () => {},
    setText: () => {},
    setElementText: () => {},
    parentNode: () => null,
    nextSibling: () => null,
    patchProp: () => {},
  });
  const workspaceId = ref("a");
  const requested: string[] = [];
  let header: { requestWorkspace(event: Event): void } | undefined;
  const app = renderer.createApp({
    render: () =>
      h(AppHeader, {
        workspaces: [workspace("a", "Alpha"), workspace("b", "Beta")],
        workspaceId: workspaceId.value,
        readiness: "ready",
        onChangeWorkspace: (id: string) => requested.push(id),
        onVnodeMounted: (vnode) => {
          header = (vnode.component as unknown as { setupState: typeof header })
            .setupState;
        },
      }),
  });
  app.provide(ssrContextKey, {});
  app.mount({});
  const select = { value: "a" };
  const choose = (value: string) => {
    select.value = value;
    header?.requestWorkspace({ target: select } as unknown as Event);
  };

  choose("b");
  expect(requested).toEqual(["b"]);
  expect(select.value).toBe("a");

  workspaceId.value = "b";
  await nextTick();
  choose("a");
  expect(requested).toEqual(["b", "a"]);
  expect(select.value).toBe("b");
  app.unmount();
});
