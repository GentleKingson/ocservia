<script setup lang="ts">
import {
  ArrowUpCircle,
  Ban,
  CircleStop,
  FileCheck2,
  LogOut,
  Power,
  Server,
  ShieldOff,
} from "@lucide/vue";
import type { NodeObservedState } from "@/api/models";
import {
  computed,
  onBeforeUnmount,
  onMounted,
  ref,
  useTemplateRef,
  watch,
} from "vue";
import { useRoute } from "vue-router";
import { useI18n } from "vue-i18n";
import { Button } from "../components/ui/button";
import DataState from "../components/common/DataState.vue";
import SectionCard from "../components/common/SectionCard.vue";
import StatusBadge from "../components/common/StatusBadge.vue";
import NodeDetailHeader from "../components/nodes/NodeDetailHeader.vue";
import NodeDetailUsersGroupsSection from "../components/nodes/NodeDetailUsersGroupsSection.vue";
import NodeDetailNav from "../components/nodes/NodeDetailNav.vue";
import NodeDetailSkeleton from "../components/nodes/NodeDetailSkeleton.vue";
import NodeObservedDetails from "../components/nodes/NodeObservedDetails.vue";
import NodeStatusSummary from "../components/nodes/NodeStatusSummary.vue";
import UserPolicyDialog from "../components/nodes/UserPolicyDialog.vue";
import { useFleetStore } from "../shared/fleet";
import { operationTone } from "../features/operations/state-tone";
import { operationStatusKey } from "../shared/operation-status";
import NodeConfigurationDialog from "@/features/configuration/NodeConfigurationDialog.vue";
import NodeCertificateDialog from "@/features/certificates/NodeCertificateDialog.vue";
import NodeActionDialog from "@/components/nodes/NodeActionDialog.vue";
import NodeUserGroupDialog from "@/components/nodes/NodeUserGroupDialog.vue";

const route = useRoute();
const fleet = useFleetStore();
const { t } = useI18n();
const routeNodeId = computed(() => {
  const value = route.params.nodeId;
  return Array.isArray(value) ? (value[0] ?? "") : value;
});
const currentNode = computed<NodeObservedState | undefined>(() =>
  fleet.selected?.id === routeNodeId.value ? fleet.selected : undefined,
);
const detailLoading = ref(true);
const detailState = ref<"loading" | "unavailable" | "not-found">("loading");
let detailSequence = 0;
watch(currentNode, (node) => {
  // A foreground retry can recover the initial detail read without navigation.
  if (node) detailState.value = "loading";
});

const configuration =
  useTemplateRef<InstanceType<typeof NodeConfigurationDialog>>("configuration");

const certificates =
  useTemplateRef<InstanceType<typeof NodeCertificateDialog>>("certificates");

const userGroup =
  useTemplateRef<InstanceType<typeof NodeUserGroupDialog>>("userGroup");
const nodeAction =
  useTemplateRef<InstanceType<typeof NodeActionDialog>>("nodeAction");
const policyDialog = ref<{ username: string }>();
const readReady = computed(
  () => !detailLoading.value && !fleet.selecting && !fleet.selectionError,
);
const dialogContext = { currentNode, readReady };
const usersState = computed(() =>
  fleet.userGroupState.filter((item) => item.kind === "user"),
);
const groupsState = computed(() =>
  fleet.userGroupState.filter((item) => item.kind === "group"),
);
const currentOperation = computed(() => {
  const operation = fleet.latestOperation;
  return currentNode.value && operation?.nodeId === currentNode.value.id
    ? operation
    : undefined;
});
const operationBelongsToNode = computed(
  () => !!currentNode.value && fleet.operationNodeId === currentNode.value.id,
);
const operationBusy = computed(
  () =>
    operationBelongsToNode.value &&
    fleet.operationTracking &&
    (!fleet.latestOperation || !!currentOperation.value),
);
function closeNodeDialogs(): void {
  userGroup.value?.close();
  nodeAction.value?.close();
  configuration.value?.close();
  certificates.value?.close();
  policyDialog.value = undefined;
}

async function selectRouteNode(): Promise<void> {
  closeNodeDialogs();
  const sequence = ++detailSequence;
  const nodeId = routeNodeId.value;
  detailLoading.value = true;
  detailState.value = "loading";
  if (!nodeId) {
    fleet.detachOperation();
    detailLoading.value = false;
    detailState.value = "not-found";
    return;
  }
  await fleet.select(nodeId);
  if (sequence !== detailSequence) return;
  detailLoading.value = false;
  if (currentNode.value) {
    detailState.value = "loading";
    return;
  }
  detailState.value =
    fleet.selectionError === "notFound" ? "not-found" : "unavailable";
}

onMounted(() => {
  fleet.start();
  void selectRouteNode();
});
onBeforeUnmount(() => {
  closeNodeDialogs();
  detailSequence += 1;
  fleet.stop();
});
watch(routeNodeId, () => void selectRouteNode(), { flush: "sync" });

const detailStatus = computed<{
  key: string;
  tone: "ok" | "error" | "muted";
}>(() => {
  if (detailLoading.value) return { key: "nodeLoading", tone: "muted" };
  if (fleet.unavailable || detailState.value === "unavailable")
    return { key: "systemsUnavailable", tone: "error" };
  if (!currentNode.value) return { key: "notObserved", tone: "muted" };
  return currentNode.value.freshness === "fresh"
    ? { key: "latestObservation", tone: "ok" }
    : { key: currentNode.value.freshness, tone: "muted" };
});

const upgradeEligible = computed(
  () =>
    Boolean(currentNode.value?.agentUpgradeEligible) &&
    Boolean(currentNode.value?.recommendedAgentVersion),
);

function actionAvailable(action: string): boolean {
  return (
    readReady.value &&
    currentNode.value?.effectiveActions?.[action]?.allowed === true
  );
}
function actionExplanation(action: string): string {
  const availability = currentNode.value?.effectiveActions?.[action];
  return availability?.allowed
    ? ""
    : t("action_" + (availability?.reason ?? "unknown"));
}
function actionState(action: string) {
  return {
    allowed: actionAvailable(action),
    explanation: actionExplanation(action),
  };
}

function openPolicy(username: string): void {
  if (currentNode.value) policyDialog.value = { username };
}
</script>

<template>
  <main>
    <NodeDetailHeader
      :title="
        currentNode?.name ??
        fleet.nodes?.find((node) => node.id === routeNodeId)?.name ??
        $t('nodeDetail')
      "
      :node-id="routeNodeId"
      :status="$t(detailStatus.key)"
      :tone="detailStatus.tone"
    />

    <NodeDetailSkeleton v-if="detailLoading" />
    <div
      v-else-if="detailState === 'not-found'"
      class="bg-card border-border text-muted-foreground grid min-h-56 place-content-center justify-items-center gap-2.5 rounded-xl border p-7 text-center text-sm shadow-xs"
    >
      <Server class="size-6" aria-hidden="true" /><span>{{
        $t("nodeNotFound")
      }}</span>
    </div>
    <DataState
      v-else-if="detailState === 'unavailable'"
      kind="error"
      :message="$t('nodeUnavailable')"
    />

    <template v-else-if="currentNode">
      <NodeStatusSummary
        :node="currentNode"
        :session-count="fleet.sessions.length"
      />
      <NodeDetailNav />
      <NodeObservedDetails id="node-overview" :node="currentNode">
        <div class="mb-4 grid gap-3">
          <p
            v-if="currentNode.freshness === 'stale'"
            class="m-0 rounded-md border border-amber-300 bg-amber-50 px-3 py-2 text-sm text-amber-900"
            role="status"
          >
            {{ $t("staleNode") }}
          </p>
          <div class="flex flex-wrap gap-2">
            <Button
              type="button"
              variant="outline"
              size="sm"
              :disabled="operationBusy || !actionAvailable('service.reload')"
              :title="actionExplanation('service.reload') || $t('reloadOcserv')"
              @click="nodeAction?.open('reload', '', $t('reloadOcserv'))"
            >
              <Power aria-hidden="true" />{{ $t("reload") }}
            </Button>
            <Button
              v-if="upgradeEligible"
              type="button"
              variant="outline"
              size="sm"
              data-testid="upgrade-agent"
              :disabled="operationBusy"
              :title="$t('upgradeAgentTitle')"
              @click="
                nodeAction?.open(
                  'upgradeAgent',
                  currentNode.recommendedAgentVersion ?? '',
                  $t('upgradeAgentTitle'),
                )
              "
            >
              <ArrowUpCircle aria-hidden="true" />{{ $t("upgradeAgent") }}
            </Button>
          </div>
          <p
            v-if="actionExplanation('service.reload')"
            class="text-muted-foreground m-0 text-xs"
          >
            {{ $t("reload") }}: {{ actionExplanation("service.reload") }}
          </p>
          <div
            v-if="currentOperation"
            class="flex flex-wrap items-center gap-2 text-sm"
            data-testid="operation-status"
            aria-live="polite"
          >
            <span class="text-muted-foreground">{{
              $t("latestOperation")
            }}</span>
            <StatusBadge
              :tone="operationTone(currentOperation)"
              :label="$t(operationStatusKey(currentOperation))"
              data-testid="operation-state"
            />
            <code
              v-if="currentOperation.agentUpgradeTargetVersion"
              class="text-xs"
              >{{ currentOperation.agentUpgradeTargetVersion }}</code
            >
            <Button
              v-if="
                operationBusy &&
                (currentOperation.state === 'unknown' ||
                  currentOperation.state === 'offline_pending')
              "
              type="button"
              variant="ghost"
              size="icon-sm"
              :title="$t('stopTrackingOperation')"
              :aria-label="$t('stopTrackingOperation')"
              @click="fleet.detachOperation"
            >
              <CircleStop aria-hidden="true" />
            </Button>
          </div>
          <p
            v-if="operationBelongsToNode && fleet.operationError !== undefined"
            class="text-destructive m-0 text-sm break-words"
            role="alert"
          >
            {{ fleet.operationError || $t("operationTrackingFailed") }}
          </p>
        </div>
      </NodeObservedDetails>

      <SectionCard id="node-sessions" :title="$t('sessions')">
        <div class="grid gap-2">
          <h3 class="m-0 text-sm font-semibold">
            {{ $t("currentSessions") }}
          </h3>
          <ul v-if="fleet.sessions.length" class="m-0 grid list-none p-0">
            <li
              v-for="session in fleet.sessions"
              :key="session.id"
              class="border-border flex items-center justify-between gap-2 border-b py-1.5 last:border-b-0"
            >
              <span class="grid min-w-0 text-sm">
                <strong class="break-words">{{ session.username }}</strong>
                <span class="text-muted-foreground text-xs">{{
                  session.clientIp
                }}</span>
              </span>
              <span class="flex shrink-0 gap-1">
                <Button
                  type="button"
                  variant="ghost"
                  size="icon-sm"
                  :disabled="operationBusy || !currentNode.bootId"
                  :title="$t('disconnect')"
                  :aria-label="$t('disconnect')"
                  @click="
                    nodeAction?.open('disconnect', session.id, $t('disconnect'))
                  "
                >
                  <LogOut aria-hidden="true" />
                </Button>
                <Button
                  type="button"
                  variant="ghost"
                  size="icon-sm"
                  class="text-destructive hover:text-destructive"
                  :disabled="operationBusy || !currentNode.bootId"
                  :title="$t('terminate')"
                  :aria-label="$t('terminate')"
                  @click="
                    nodeAction?.open('terminate', session.id, $t('terminate'))
                  "
                >
                  <ShieldOff aria-hidden="true" />
                </Button>
              </span>
            </li>
          </ul>
          <p v-else class="text-muted-foreground m-0 text-sm">
            {{ $t("noSessions") }}
          </p>
        </div>
        <div class="grid gap-2">
          <h3 class="m-0 text-sm font-semibold">{{ $t("ipBans") }}</h3>
          <ul v-if="fleet.ipBans.length" class="m-0 grid list-none p-0">
            <li
              v-for="ban in fleet.ipBans"
              :key="ban.ip"
              class="border-border flex items-center justify-between gap-2 border-b py-1.5 last:border-b-0"
            >
              <strong class="min-w-0 text-sm break-all">{{ ban.ip }}</strong>
              <Button
                type="button"
                variant="ghost"
                size="icon-sm"
                :disabled="operationBusy"
                :title="$t('removeBan')"
                :aria-label="$t('removeBan')"
                @click="nodeAction?.open('unban', ban.ip, $t('removeBan'))"
              >
                <Ban aria-hidden="true" />
              </Button>
            </li>
          </ul>
          <p v-else class="text-muted-foreground m-0 text-sm">
            {{ $t("noIpBans") }}
          </p>
        </div>
      </SectionCard>

      <NodeDetailUsersGroupsSection
        :users="usersState"
        :groups="groupsState"
        :busy="operationBusy"
        :user-action="actionState('user.manage')"
        :group-action="actionState('group.manage')"
        @desired="(kind, name, version) => userGroup?.open(kind, name, version)"
        @policy="openPolicy"
      />

      <SectionCard id="node-configuration" :title="$t('configurationSection')">
        <div class="flex flex-wrap items-center justify-between gap-3">
          <span class="inline-flex items-center gap-2 text-sm">
            <FileCheck2
              class="text-muted-foreground size-4"
              aria-hidden="true"
            />
            {{ $t("configPlan") }}
          </span>
          <Button
            type="button"
            variant="outline"
            size="sm"
            :disabled="
              operationBusy ||
              configuration?.currentConfigRevision === undefined ||
              !actionAvailable('config.plan')
            "
            :title="$t('configPlan')"
            @click="configuration?.open()"
          >
            {{ $t("plan") }}
          </Button>
        </div>
        <p
          v-if="actionExplanation('config.plan')"
          class="text-muted-foreground m-0 text-xs"
        >
          {{ actionExplanation("config.plan") }}
        </p>
      </SectionCard>

      <SectionCard id="node-certificates" :title="$t('certificatesSection')">
        <div class="flex flex-wrap items-center justify-between gap-3">
          <span class="text-sm">{{ $t("certificateLifecycle") }}</span>
          <Button
            type="button"
            variant="outline"
            size="sm"
            :disabled="operationBusy || !actionAvailable('certificate.read')"
            :title="
              actionExplanation('certificate.read') ||
              $t('certificateLifecycle')
            "
            @click="certificates?.open()"
          >
            {{ $t("certificate") }}
          </Button>
        </div>
        <p
          v-if="actionExplanation('certificate.issue')"
          class="text-muted-foreground m-0 text-xs"
        >
          {{ actionExplanation("certificate.issue") }}
        </p>
      </SectionCard>
    </template>

    <NodeActionDialog ref="nodeAction" :node-name="currentNode?.name" />

    <UserPolicyDialog
      v-if="policyDialog"
      :key="policyDialog.username"
      :node-id="currentNode?.id"
      :username="policyDialog.username"
      @close="policyDialog = undefined"
    />

    <NodeConfigurationDialog
      ref="configuration"
      :context="dialogContext"
      :action-available="actionAvailable"
      :action-explanation="actionExplanation"
    />
    <NodeCertificateDialog
      ref="certificates"
      :context="dialogContext"
      :action-available="actionAvailable"
      :action-explanation="actionExplanation"
    />
    <NodeUserGroupDialog ref="userGroup" :context="dialogContext" />
  </main>
</template>
