import type {
  ConfigPlan,
  NodeObservedState,
  Operation,
} from "@ocservia/api-client";
import { computed, onScopeDispose, ref, watch, type Ref } from "vue";
import {
  applyConfigPlan,
  createConfigPlan,
  getConfigPlan,
} from "../../api/configuration";
import { getOperation } from "../../api/operations";
import {
  beginNodeMutation,
  createNodeWorkflow,
  finishNodeMutation,
  readNodeReceipt,
  rememberNodeReceipt,
  waitForNodeMutation,
  waitForNodePoll,
  type NodeWorkflowContext,
} from "../node-workflow";
import type { WorkspaceContext } from "../../api/workspace";

interface ConfigurationInputs {
  currentNode: Readonly<Ref<NodeObservedState | undefined>>;
  readReady: Readonly<Ref<boolean>>;
  workspaceContext: () => WorkspaceContext;
  trackOperation: (id: string) => Promise<void>;
  t: (key: string) => string;
}

export function useNodeConfiguration({
  currentNode,
  readReady,
  workspaceContext,
  trackOperation,
  t,
}: ConfigurationInputs) {
  const configDialog = ref(false);
  const configPlanSource = ref<{
    nodeId: string;
    revision: number;
    workspace: WorkspaceContext;
  }>();
  const currentConfigRevision = computed(() => {
    if (!readReady.value) return undefined;
    const revision = currentNode.value?.configRevision;
    return typeof revision === "number" &&
      Number.isSafeInteger(revision) &&
      revision >= 0
      ? revision
      : undefined;
  });
  const canSubmitConfigPlan = computed(() => {
    const source = configPlanSource.value;
    const workspace = workspaceContext();
    return Boolean(
      configDialog.value &&
      source &&
      currentConfigRevision.value !== undefined &&
      source.nodeId === currentNode.value?.id &&
      source.workspace.id === workspace.id &&
      source.workspace.generation === workspace.generation,
    );
  });
  const configPlan = ref<ConfigPlan>();
  const configOperation = ref<Operation>();
  const configError = ref("");
  const configLoading = ref(false);
  const configPort = ref(443);
  const configMaxClients = ref(128);
  const configRoute = ref("default");
  const configCertificateSecretRefId = ref("");
  const configPrivateKeySecretRefId = ref("");
  const configReason = ref("");
  const configApplyApproval = ref("");
  const configApplyReason = ref("");
  const configWorkflow = createNodeWorkflow(
    () => currentNode.value?.id,
    () => configDialog.value,
    workspaceContext,
  );
  let configContext: NodeWorkflowContext | undefined;

  function resetDialog(): void {
    configWorkflow.cancel();
    configContext = undefined;
    configPlanSource.value = undefined;
    configPlan.value = undefined;
    configOperation.value = undefined;
    configError.value = "";
    configLoading.value = false;
  }
  watch(
    configDialog,
    (open) => {
      if (!open) resetDialog();
    },
    { flush: "sync" },
  );
  onScopeDispose(() => {
    configDialog.value = false;
    resetDialog();
  });

  async function openConfigPlan(): Promise<void> {
    const node = currentNode.value;
    const revision = currentConfigRevision.value;
    if (!node || revision === undefined) return;
    const context = configWorkflow.begin(node.id);
    configContext = context;
    configPlanSource.value = {
      nodeId: node.id,
      revision,
      workspace: context.workspace,
    };
    configDialog.value = true;
    configPlan.value = undefined;
    configOperation.value = undefined;
    configLoading.value = false;
    configError.value = "";
    configReason.value = "";
    configApplyApproval.value = "";
    configApplyReason.value = "";
    try {
      const pending = waitForNodeMutation(context, "config");
      if (pending) {
        configLoading.value = true;
        await pending;
        if (!configWorkflow.isCurrent(context)) return;
      }
      const receipt = readNodeReceipt(context, "config");
      if (!receipt.resourceId) return;
      configLoading.value = true;
      const plan = await getConfigPlan(receipt.resourceId, context.signal);
      if (!configWorkflow.isCurrent(context)) return;
      if (
        plan.nodeId !== context.nodeId ||
        plan.workspaceId !== context.workspace.id
      )
        return;
      await pollConfigPlan(context, plan);
      if (
        !configWorkflow.isCurrent(context) ||
        !receipt.operationId ||
        receipt.operationId === plan.operationId
      )
        return;
      const operation = await getOperation(receipt.operationId, context.signal);
      if (
        configWorkflow.isCurrent(context) &&
        operation.nodeId === context.nodeId
      )
        configOperation.value = operation;
    } catch (error) {
      if (configWorkflow.isCurrent(context))
        configError.value =
          error instanceof Error ? error.message : t("configPlanFailed");
    } finally {
      if (configWorkflow.isCurrent(context)) configLoading.value = false;
    }
  }

  async function pollConfigPlan(
    context: NodeWorkflowContext,
    plan: ConfigPlan,
  ): Promise<void> {
    if (!configWorkflow.isCurrent(context)) return;
    configPlan.value = plan;
    for (
      let attempt = 0;
      attempt < 30 &&
      !["succeeded", "failed", "rejected", "unknown", "expired"].includes(
        plan.state,
      );
      attempt += 1
    ) {
      await waitForNodePoll(context.signal);
      if (!configWorkflow.isCurrent(context)) return;
      plan = await getConfigPlan(plan.id, context.signal);
      if (!configWorkflow.isCurrent(context)) return;
      configPlan.value = plan;
    }
  }

  async function submitConfigPlan(): Promise<void> {
    const source = configPlanSource.value;
    const context = configContext;
    const workspace = workspaceContext();
    if (
      !source ||
      !context ||
      !configWorkflow.isCurrent(context) ||
      configLoading.value ||
      !canSubmitConfigPlan.value ||
      !configReason.value.trim() ||
      source.workspace.id !== workspace.id ||
      source.workspace.generation !== workspace.generation
    )
      return;
    const ticket = beginNodeMutation(context, "config");
    if (!ticket) return;
    configLoading.value = true;
    configError.value = "";
    try {
      // Do not abort mutations on UI teardown: retain the server's accepted IDs,
      // then fence all UI updates and subsequent reads by the captured context.
      const plan = await createConfigPlan(source.nodeId, {
        expectedRevision: source.revision,
        template: {
          name: "node-baseline",
          directives: [
            { name: "auth", value: "plain[passwd=/etc/ocserv/ocpasswd]" },
            { name: "max-clients", value: String(configMaxClients.value) },
            { name: "route", value: configRoute.value.trim() },
            {
              name: "server-cert",
              secretRef: {
                secretRefId: configCertificateSecretRefId.value.trim(),
              },
            },
            {
              name: "server-key",
              secretRef: {
                secretRefId: configPrivateKeySecretRefId.value.trim(),
              },
            },
            { name: "socket-file", value: "/run/ocserv.socket" },
            { name: "tcp-port", value: String(configPort.value) },
          ],
        },
        ttlSeconds: 900,
        reason: configReason.value.trim(),
      });
      rememberNodeReceipt(ticket, {
        resourceId: plan.id,
        operationId: plan.operationId,
      });
      await pollConfigPlan(context, plan);
    } catch (error) {
      if (!configWorkflow.isCurrent(context)) return;
      configError.value =
        error instanceof Error ? error.message : t("configPlanFailed");
    } finally {
      finishNodeMutation(ticket);
      if (configWorkflow.isCurrent(context)) configLoading.value = false;
    }
  }

  async function submitConfigApply(): Promise<void> {
    const context = configContext;
    const plan = configPlan.value;
    if (
      !context ||
      !configWorkflow.isCurrent(context) ||
      configLoading.value ||
      !canSubmitConfigPlan.value ||
      !plan ||
      !configApplyApproval.value.trim() ||
      !configApplyReason.value.trim()
    )
      return;
    const ticket = beginNodeMutation(context, "config");
    if (!ticket) return;
    configLoading.value = true;
    configError.value = "";
    try {
      const operation = await applyConfigPlan(plan.id, {
        approvalId: configApplyApproval.value.trim(),
        reason: configApplyReason.value.trim(),
      });
      rememberNodeReceipt(ticket, {
        resourceId: plan.id,
        operationId: operation.id,
      });
      if (!configWorkflow.isCurrent(context)) return;
      configDialog.value = false;
      await trackOperation(operation.id);
    } catch (error) {
      if (!configWorkflow.isCurrent(context)) return;
      configError.value =
        error instanceof Error ? error.message : t("configApplyFailed");
    } finally {
      finishNodeMutation(ticket);
      if (configWorkflow.isCurrent(context)) configLoading.value = false;
    }
  }

  return {
    configDialog,
    currentConfigRevision,
    canSubmitConfigPlan,
    configPlan,
    configOperation,
    configError,
    configLoading,
    configPort,
    configMaxClients,
    configRoute,
    configCertificateSecretRefId,
    configPrivateKeySecretRefId,
    configReason,
    configApplyApproval,
    configApplyReason,
    openConfigPlan,
    submitConfigPlan,
    submitConfigApply,
  };
}
