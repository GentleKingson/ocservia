import { h, ssrContextKey, type Ref, type Component } from "vue";
import type { NodeObservedState } from "@/api/models";
import { renderer } from "./renderer";
import NodeDetailView from "../src/views/NodeDetailView.vue";
import NodeConfigurationDialog from "../src/features/configuration/NodeConfigurationDialog.vue";
import NodeCertificateDialog from "../src/features/certificates/NodeCertificateDialog.vue";
import NodeUserGroupDialog from "../src/components/nodes/NodeUserGroupDialog.vue";

const configuration = {
  ...NodeConfigurationDialog,
  render: () => null,
} as Component;
const certificates = {
  ...NodeCertificateDialog,
  render: () => null,
} as Component;
const userGroup = { ...NodeUserGroupDialog, render: () => null } as Component;

export function mountNodeDetail() {
  // Vitest loads SSR-compiled SFCs. Mount their real setups with a minimal
  // render tree so prop updates, template refs and unmount hooks run together.
  const app = renderer.createApp({
    ...NodeDetailView,
    render(this: { $: unknown }) {
      const page = (
        this.$ as {
          setupState: {
            dialogContext: {
              currentNode: Readonly<Ref<NodeObservedState | undefined>>;
              readReady: Readonly<Ref<boolean>>;
            };
            actionAvailable: (action: string) => boolean;
            actionExplanation: (action: string) => string;
          };
        }
      ).setupState;
      const props = {
        context: page.dialogContext,
        actionAvailable: page.actionAvailable,
        actionExplanation: page.actionExplanation,
      };
      return h("main", [
        h(configuration, { ...props, ref: "configuration" }),
        h(certificates, { ...props, ref: "certificates" }),
        h(userGroup, { context: page.dialogContext, ref: "userGroup" }),
      ]);
    },
  });
  app.provide(ssrContextKey, {});
  const instance = app.mount({});
  return { app, instance };
}
