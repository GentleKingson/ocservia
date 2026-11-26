import type { ConfigPlan, NodeObservedState, Operation } from "@/api/models";
import { computed, onScopeDispose, ref, watch, type Ref } from "vue";
import {
  applyConfigPlan,
  createConfigPlan,
  getConfigPlan,
} from "../../api/configuration";
import { getOperation } from "../../api/operations";
import {
  createNodeWorkflow,
  pollNodeResource,
  readNodeWorkflowReference,
  rememberNodeWorkflowReference,
  runNodeMutation,
  waitForNodeMutation,
  type NodeWorkflowContext,
} from "../node-workflow";

interface ConfigurationInputs {
  currentNode: Readonly<Ref<NodeObservedState | undefined>>;
  readReady: Readonly<Ref<boolean>>;
  workspaceId: () => string | undefined;
  trackOperation: (id: string) => Promise<void>;
  t: (key: string) => string;
}

export function useNodeConfiguration({
  currentNode,
  readReady,
  workspaceId,
  trackOperation,
  t,
}: ConfigurationInputs) {
  const configDialog = ref(false);
  const configPlanSource = ref<{
    nodeId: string;
    revision: number;
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
    return Boolean(
      configDialog.value &&
      source &&
      currentConfigRevision.value !== undefined &&
      source.nodeId === currentNode.value?.id,
    );
  });
  const configPlan = ref<ConfigPlan>();
  const configOperation = ref<Operation>();
  const configError = ref("");
  const configLoading = ref(false);
  const configPort = ref(443);
  const configUdpPort = ref(0);
  const configDevice = ref("vpns");
  const configNetwork = ref("10.42.0.0/24");
  const configDns = ref("1.1.1.1");
  const configCookieTimeout = ref(300);
  const configMaxSameClients = ref(2);
  const configMaxClients = ref(128);
  const configRoute = ref("default");
  const configCertificateSecretRefId = ref("");
  const configPrivateKeySecretRefId = ref("");
  const configReason = ref("");
  const configApplyReason = ref("");
  const configWorkflow = createNodeWorkflow(
    () => currentNode.value?.id,
    () => configDialog.value,
    workspaceId,
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
    };
    configDialog.value = true;
    configPlan.value = undefined;
    configOperation.value = undefined;
    configLoading.value = false;
    configError.value = "";
    configReason.value = "";
    configApplyReason.value = "";
    try {
      const pending = waitForNodeMutation(context, "config");
      if (pending) {
        configLoading.value = true;
        await pending;
        if (!configWorkflow.isCurrent(context)) return;
      }
      const reference = readNodeWorkflowReference(context, "config");
      if (!reference.resourceId) return;
      configLoading.value = true;
      const plan = await getConfigPlan(reference.resourceId, context.signal);
      if (!configWorkflow.isCurrent(context)) return;
      if (
        plan.nodeId !== context.nodeId ||
        plan.workspaceId !== context.workspaceId
      )
        return;
      await pollConfigPlan(context, plan);
      if (
        !configWorkflow.isCurrent(context) ||
        !reference.operationId ||
        reference.operationId === plan.operationId
      )
        return;
      const operation = await getOperation(
        reference.operationId,
        context.signal,
      );
      if (
        configWorkflow.isCurrent(context) &&
        operation.nodeId === context.nodeId
      )
        configOperation.value = operation;
    } catch (error) {
      if (configWorkflow.isCurrent(context))
        configError.value =
          (error instanceof Error && error.message) || t("configPlanFailed");
    } finally {
      if (configWorkflow.isCurrent(context)) configLoading.value = false;
    }
  }

  async function pollConfigPlan(
    context: NodeWorkflowContext,
    plan: ConfigPlan,
  ): Promise<void> {
    await pollNodeResource(
      configWorkflow,
      context,
      plan,
      (current) =>
        !["succeeded", "failed", "rejected", "unknown", "expired"].includes(
          current.state,
        ),
      (current, signal) => getConfigPlan(current.id, signal),
      (current) => {
        configPlan.value = current;
      },
    );
  }

  async function submitConfigPlan(): Promise<void> {
    const source = configPlanSource.value;
    if (!source || !canSubmitConfigPlan.value || !configReason.value.trim())
      return;
    await runNodeMutation({
      workflow: configWorkflow,
      context: configContext,
      kind: "config",
      loading: configLoading,
      error: configError,
      fallbackError: () => t("configPlanFailed"),
      async run(ticket, context) {
        // Do not abort mutations on UI teardown: retain the server's accepted IDs,
        // then fence all UI updates and subsequent reads by the captured context.
        const plan = await createConfigPlan(source.nodeId, {
          expectedRevision: source.revision,
          template: {
            name: "node-baseline",
            directives: [
              { name: "auth", value: "plain[passwd=/etc/ocserv/ocpasswd]" },
              {
                name: "cookie-timeout",
                value: String(configCookieTimeout.value),
              },
              { name: "device", value: configDevice.value.trim() },
              { name: "dns", value: configDns.value.trim() },
              { name: "ipv4-network", value: configNetwork.value.trim() },
              { name: "max-clients", value: String(configMaxClients.value) },
              {
                name: "max-same-clients",
                value: String(configMaxSameClients.value),
              },
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
              { name: "udp-port", value: String(configUdpPort.value) },
            ],
          },
          ttlSeconds: 900,
          reason: configReason.value.trim(),
        });
        rememberNodeWorkflowReference(ticket, {
          resourceId: plan.id,
          operationId: plan.operationId,
        });
        await pollConfigPlan(context, plan);
      },
    });
  }

  async function submitConfigApply(): Promise<void> {
    const plan = configPlan.value;
    if (!canSubmitConfigPlan.value || !plan || !configApplyReason.value.trim())
      return;
    await runNodeMutation({
      workflow: configWorkflow,
      context: configContext,
      kind: "config",
      loading: configLoading,
      error: configError,
      fallbackError: () => t("configApplyFailed"),
      async run(ticket, context) {
        const operation = await applyConfigPlan(plan.id, {
          reason: configApplyReason.value.trim(),
        });
        rememberNodeWorkflowReference(ticket, {
          resourceId: plan.id,
          operationId: operation.id,
        });
        if (!configWorkflow.isCurrent(context)) return;
        configDialog.value = false;
        await trackOperation(operation.id);
      },
    });
  }

  return {
    configDialog,
    currentConfigRevision,
    configSourceRevision: computed(() => configPlanSource.value?.revision),
    canSubmitConfigPlan,
    configPlan,
    configOperation,
    configError,
    configLoading,
    configPort,
    configUdpPort,
    configDevice,
    configNetwork,
    configDns,
    configCookieTimeout,
    configMaxSameClients,
    configMaxClients,
    configRoute,
    configCertificateSecretRefId,
    configPrivateKeySecretRefId,
    configReason,
    configApplyReason,
    openConfigPlan,
    submitConfigPlan,
    submitConfigApply,
  };
}
