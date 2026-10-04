<script setup lang="ts">
import { Radio, Server, Users } from "@lucide/vue";
import type { NodeObservedState } from "@ocservia/api-client";
import { defaultWindow, useEventListener, useNow } from "@vueuse/core";
import { computed, onMounted, ref } from "vue";
import { useI18n } from "vue-i18n";
import { useRoute, useRouter } from "vue-router";

import { createAgentRollout } from "../api/agents";
import { workspaceChangedEvent } from "../api/workspace";
import DataState from "../components/common/DataState.vue";
import PageHeader from "../components/common/PageHeader.vue";
import NodeTableToolbar from "../components/nodes/NodeTableToolbar.vue";
import { Badge } from "../components/ui/badge";
import { Button } from "../components/ui/button";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "../components/ui/table";
import {
  countFilterValues,
  filterNodes,
  readNodeListQuery,
  sortNodes,
  writeNodeListQuery,
  type NodeListState,
} from "../features/nodes/node-list";
import { agentVersionLabel } from "../shared/agent-version";
import { useFleetStore } from "../shared/fleet";
import { formatTimestamp, relativeTimestamp } from "../shared/timestamp";

const fleet = useFleetStore();
const route = useRoute();
const router = useRouter();
const { t, locale } = useI18n();
const now = useNow({ interval: 30_000 });

onMounted(async () => {
  if (!fleet.initialized) await fleet.rebuild();
  if (fleet.initialized) void fleet.connect();
});

// The URL query owns the list view state so refresh, history and returning
// to the list restore it. Filtering reads the store only; it never writes.
const listState = computed(() => readNodeListQuery(route.query));
function updateListState(state: NodeListState): void {
  void router.replace({ query: writeNodeListQuery(route.query, state) });
}
function clearFilters(): void {
  updateListState({
    ...listState.value,
    search: "",
    filters: readNodeListQuery({}).filters,
  });
}

// fleet.nodes only ever holds a complete paged snapshot, so filtering and
// counts below always cover the whole workspace.
const visibleNodes = computed(() =>
  sortNodes(filterNodes(fleet.nodes, listState.value), listState.value.sort),
);
const filterCounts = computed(() => countFilterValues(fleet.nodes));
const showColumn = (column: NodeListState["columns"][number]) =>
  listState.value.columns.includes(column);

function pathRtt(node: NodeObservedState): string {
  return node.path ? String(Math.round(node.path.rttMs)) : "";
}

function heartbeatRelative(value: string): string | undefined {
  return relativeTimestamp(value, now.value.getTime(), locale.value);
}

const trustBadge: Record<string, string> = {
  active: "border-success/40 text-success",
  pending: "border-primary/40 text-primary",
  revoked: "border-destructive/40 text-destructive",
};
const agentBadge: Record<string, "default" | "secondary" | "destructive"> = {
  upgrade_available: "default",
  ahead: "secondary",
  unsupported: "destructive",
};

const selected = ref<string[]>([]);
const rolloutDialog = ref(false);
const rolloutBatchSize = ref(5);
const rolloutReason = ref("");
const rolloutApprovalId = ref("");
const rolloutStarting = ref(false);
const rolloutError = ref("");

const selectedNodes = computed(() =>
  fleet.nodes.filter(
    (node) => selected.value.includes(node.id) && node.agentUpgradeEligible,
  ),
);
const rolloutTarget = computed(
  () => selectedNodes.value[0]?.recommendedAgentVersion ?? "",
);

function toggleSelection(nodeId: string): void {
  const index = selected.value.indexOf(nodeId);
  if (index >= 0) selected.value.splice(index, 1);
  else selected.value.push(nodeId);
}

// Selection stays keyed by node ID across filters. The header checkbox only
// covers visible rows so a filter cannot silently widen the rollout scope.
const visibleEligibleIds = computed(() =>
  visibleNodes.value
    .filter((node) => node.agentUpgradeEligible)
    .map((node) => node.id),
);
const allVisibleEligibleSelected = computed(
  () =>
    visibleEligibleIds.value.length > 0 &&
    visibleEligibleIds.value.every((id) => selected.value.includes(id)),
);
function selectVisibleEligible(event: Event): void {
  const visible = new Set(visibleEligibleIds.value);
  const kept = selected.value.filter((id) => !visible.has(id));
  selected.value = (event.target as HTMLInputElement).checked
    ? [...kept, ...visible]
    : kept;
}
const hiddenSelectedCount = computed(() => {
  const visible = new Set(visibleNodes.value.map((node) => node.id));
  return selectedNodes.value.filter((node) => !visible.has(node.id)).length;
});

// Selections never carry over into another workspace.
useEventListener(defaultWindow, workspaceChangedEvent, () => {
  selected.value = [];
  rolloutDialog.value = false;
});

function openRolloutDialog(): void {
  rolloutError.value = "";
  rolloutDialog.value = true;
}

async function submitRollout(): Promise<void> {
  if (rolloutStarting.value || !rolloutTarget.value) return;
  rolloutStarting.value = true;
  rolloutError.value = "";
  try {
    const rollout = await createAgentRollout(
      rolloutTarget.value,
      [...selectedNodes.value].map((node) => node.id),
      rolloutBatchSize.value,
      rolloutReason.value.trim(),
      rolloutApprovalId.value.trim(),
    );
    rolloutDialog.value = false;
    selected.value = [];
    rolloutReason.value = "";
    rolloutApprovalId.value = "";
    await router.push({
      name: "rollout-detail",
      params: { rolloutId: rollout.id },
    });
  } catch (cause) {
    rolloutError.value =
      cause instanceof Error ? cause.message : t("rolloutStartFailed");
  } finally {
    rolloutStarting.value = false;
  }
}
</script>

<template>
  <main class="overview">
    <PageHeader :eyebrow="$t('fleet')" :title="$t('nodes')">
      <template #actions>
        <span
          class="inline-flex items-center gap-2 text-sm font-medium"
          :class="fleet.unavailable ? 'text-destructive' : 'text-success'"
        >
          <span
            class="size-2 rounded-full"
            :class="fleet.unavailable ? 'bg-destructive' : 'bg-success'"
            aria-hidden="true"
          ></span>
          {{ $t(fleet.unavailable ? "systemsUnavailable" : "liveTelemetry") }}
        </span>
      </template>
    </PageHeader>

    <div v-if="!fleet.initialized && fleet.unavailable && !fleet.loading">
      <DataState kind="error" :message="$t('fleetLoadFailed')" />
      <Button
        type="button"
        variant="outline"
        class="mt-3"
        @click="fleet.rebuild()"
      >
        {{ $t("retry") }}
      </Button>
    </div>
    <DataState
      v-else-if="!fleet.initialized"
      kind="loading"
      :message="$t('loading')"
    />

    <template v-else>
      <section
        class="mb-2 grid gap-3 sm:grid-cols-3"
        :aria-label="$t('fleetStatus')"
      >
        <div
          v-for="metric in [
            {
              label: 'onlineNodes',
              value: `${fleet.online} / ${fleet.nodes.length}`,
              icon: Server,
            },
            { label: 'relayPaths', value: fleet.relay, icon: Radio },
            {
              label: 'activeSessions',
              value: fleet.sessionCount,
              icon: Users,
            },
          ]"
          :key="metric.label"
          class="bg-card border-border flex items-center justify-between rounded-lg border px-4 py-3"
        >
          <div class="grid gap-1">
            <span class="text-muted-foreground text-xs">{{
              $t(metric.label)
            }}</span>
            <strong class="text-xl font-semibold tabular-nums">{{
              metric.value
            }}</strong>
          </div>
          <component
            :is="metric.icon"
            class="text-muted-foreground size-5"
            aria-hidden="true"
          />
        </div>
      </section>
      <p class="text-muted-foreground mb-4 text-xs">
        {{
          $t(fleet.unavailable ? "fleetCountScopeStale" : "fleetCountScope", {
            count: fleet.nodes.length,
          })
        }}
      </p>

      <section class="min-w-0">
        <p
          class="mb-2 min-h-5 text-sm"
          :class="
            fleet.unavailable ? 'text-destructive' : 'text-muted-foreground'
          "
          role="status"
          aria-live="polite"
        >
          <template v-if="fleet.loading">{{ $t("fleetRefreshing") }}</template>
          <template v-else-if="fleet.unavailable">{{
            $t("fleetRefreshFailed")
          }}</template>
        </p>
        <p
          v-if="fleet.nodes.length === 0"
          class="bg-card border-border text-muted-foreground grid min-h-40 place-content-center rounded-lg border p-7 text-center text-sm"
        >
          {{ $t("noNodes") }}
        </p>
        <template v-else>
          <NodeTableToolbar
            :state="listState"
            :counts="filterCounts"
            :shown="visibleNodes.length"
            :total="fleet.nodes.length"
            @update="updateListState"
          />
          <div
            v-if="visibleNodes.length === 0"
            class="bg-card border-border text-muted-foreground grid min-h-40 place-content-center justify-items-center gap-3 rounded-lg border p-7 text-center text-sm"
          >
            <p>{{ $t("noMatchingNodes") }}</p>
            <Button type="button" variant="outline" @click="clearFilters">
              {{ $t("clearFilters") }}
            </Button>
          </div>
          <div
            v-else
            class="bg-card border-border overflow-hidden rounded-lg border"
          >
            <Table class="min-w-[44rem]">
              <TableHeader class="bg-muted/50">
                <TableRow class="hover:bg-transparent">
                  <TableHead class="w-10 pl-4">
                    <input
                      type="checkbox"
                      class="accent-primary size-4 align-middle"
                      :aria-label="$t('selectVisibleNodes')"
                      :checked="allVisibleEligibleSelected"
                      :disabled="visibleEligibleIds.length === 0"
                      @change="selectVisibleEligible"
                    />
                  </TableHead>
                  <TableHead class="text-muted-foreground">{{
                    $t("node")
                  }}</TableHead>
                  <TableHead class="text-muted-foreground">{{
                    $t("trust")
                  }}</TableHead>
                  <TableHead class="text-muted-foreground">{{
                    $t("connection")
                  }}</TableHead>
                  <TableHead class="text-muted-foreground">{{
                    $t("agent")
                  }}</TableHead>
                  <TableHead class="text-muted-foreground text-right">{{
                    $t("sessions")
                  }}</TableHead>
                  <TableHead class="text-muted-foreground">{{
                    $t("lastHeartbeat")
                  }}</TableHead>
                  <TableHead
                    v-if="showColumn('path')"
                    class="text-muted-foreground"
                    >{{ $t("path") }}</TableHead
                  >
                  <TableHead
                    v-if="showColumn('ocserv')"
                    class="text-muted-foreground"
                    >{{ $t("ocserv") }}</TableHead
                  >
                  <TableHead
                    v-if="showColumn('platform')"
                    class="text-muted-foreground pr-4"
                    >{{ $t("archOs") }}</TableHead
                  >
                </TableRow>
              </TableHeader>
              <TableBody>
                <TableRow
                  v-for="node in visibleNodes"
                  :key="node.id"
                  :data-state="
                    selected.includes(node.id) ? 'selected' : undefined
                  "
                >
                  <TableCell class="py-3 pl-4">
                    <input
                      v-if="node.agentUpgradeEligible"
                      type="checkbox"
                      class="accent-primary size-4 align-middle"
                      :aria-label="`${$t('rollingUpgrade')}: ${node.name}`"
                      :checked="selected.includes(node.id)"
                      @change="toggleSelection(node.id)"
                    />
                  </TableCell>
                  <TableCell class="max-w-64 py-3 whitespace-normal">
                    <RouterLink
                      :to="{
                        name: 'node-detail',
                        params: { nodeId: node.id },
                      }"
                      class="text-foreground focus-visible:outline-ring font-medium break-words hover:underline focus-visible:rounded-sm focus-visible:outline-2 focus-visible:outline-offset-2"
                      >{{ node.name }}</RouterLink
                    >
                    <code
                      class="text-muted-foreground block max-w-28 truncate text-xs"
                      :title="node.id"
                      >{{ node.id }}</code
                    >
                  </TableCell>
                  <TableCell class="py-3">
                    <Badge
                      variant="outline"
                      :class="trustBadge[node.trustStatus]"
                      >{{ $t(node.trustStatus) }}</Badge
                    >
                  </TableCell>
                  <TableCell class="py-3">
                    <span class="inline-flex items-center gap-1.5">
                      <span
                        class="size-2 rounded-full"
                        :class="
                          node.connectionState === 'online'
                            ? 'bg-success'
                            : 'bg-muted-foreground/50'
                        "
                        aria-hidden="true"
                      ></span>
                      {{ $t(node.connectionState) }}
                    </span>
                    <span
                      class="block text-xs"
                      :class="
                        node.freshness === 'stale'
                          ? 'text-destructive'
                          : 'text-muted-foreground'
                      "
                      >{{ $t(node.freshness) }}</span
                    >
                  </TableCell>
                  <TableCell class="py-3">
                    <span class="block">{{
                      node.agentVersion ?? $t("unknown")
                    }}</span>
                    <Badge
                      data-testid="agent-version-state"
                      :variant="
                        agentBadge[node.agentVersionState ?? ''] ?? 'outline'
                      "
                      >{{ $t(agentVersionLabel(node)) }}</Badge
                    >
                  </TableCell>
                  <TableCell class="py-3 text-right tabular-nums">{{
                    node.sessionCount
                  }}</TableCell>
                  <TableCell class="py-3">
                    <template v-if="node.lastHeartbeatAt">
                      <span class="block">{{
                        heartbeatRelative(node.lastHeartbeatAt) ??
                        formatTimestamp(node.lastHeartbeatAt)
                      }}</span>
                      <time
                        v-if="heartbeatRelative(node.lastHeartbeatAt)"
                        class="text-muted-foreground block text-xs"
                        :datetime="node.lastHeartbeatAt"
                        >{{ formatTimestamp(node.lastHeartbeatAt) }}</time
                      >
                    </template>
                    <span v-else class="text-muted-foreground">{{
                      $t("notObserved")
                    }}</span>
                  </TableCell>
                  <TableCell v-if="showColumn('path')" class="py-3">
                    {{ $t(node.path?.mode ?? "unknown") }}
                    <span
                      v-if="node.path"
                      class="text-muted-foreground block text-xs"
                      >{{ pathRtt(node) }} {{ $t("milliseconds") }}</span
                    >
                  </TableCell>
                  <TableCell v-if="showColumn('ocserv')" class="py-3">{{
                    node.ocservVersion ?? $t("notAvailable")
                  }}</TableCell>
                  <TableCell
                    v-if="showColumn('platform')"
                    class="py-3 pr-4 whitespace-normal"
                  >
                    {{ node.architecture || $t("unknown") }}
                    <span class="text-muted-foreground block text-xs">{{
                      node.osRelease ?? $t("notAvailable")
                    }}</span>
                  </TableCell>
                </TableRow>
              </TableBody>
            </Table>
          </div>
        </template>
        <div class="mt-4 flex flex-wrap items-center justify-end gap-3 text-sm">
          <span
            v-if="selectedNodes.length > 0"
            class="text-muted-foreground"
            aria-live="polite"
            >{{
              $t("selectionSummary", {
                selected: selectedNodes.length,
                hidden: hiddenSelectedCount,
              })
            }}</span
          >
          <Button
            v-if="selectedNodes.length > 0"
            type="button"
            variant="ghost"
            @click="selected = []"
          >
            {{ $t("clearSelection") }}
          </Button>
          <Button
            type="button"
            :disabled="selectedNodes.length === 0"
            @click="openRolloutDialog"
          >
            {{ $t("rollingUpgrade") }} ({{ selectedNodes.length }})
          </Button>
        </div>
      </section>
    </template>

    <div
      v-if="rolloutDialog"
      class="dialog-backdrop"
      @click.self="rolloutDialog = false"
    >
      <form class="operation-dialog" @submit.prevent="submitRollout">
        <header>
          <h2>{{ $t("rollingUpgradeTitle") }}</h2>
          <code>{{ rolloutTarget }}</code>
        </header>
        <label for="rollout-target">{{ $t("targetVersion") }}</label>
        <output id="rollout-target" class="read-only-value">{{
          rolloutTarget
        }}</output>
        <label for="rollout-nodes">{{ $t("selectedNodes") }}</label>
        <output id="rollout-nodes" class="read-only-value"
          >{{ selectedNodes.length }}:
          {{ selectedNodes.map((node) => node.name).join(", ") }}</output
        >
        <label for="rollout-canary">{{ $t("canary") }}</label>
        <output id="rollout-canary" class="read-only-value">{{
          $t("canaryOneNode")
        }}</output>
        <label for="rollout-batch-size">{{ $t("batchSize") }}</label>
        <input
          id="rollout-batch-size"
          v-model.number="rolloutBatchSize"
          type="number"
          min="1"
          max="20"
          required
        />
        <label for="rollout-reason">{{ $t("reason") }}</label>
        <textarea
          id="rollout-reason"
          v-model="rolloutReason"
          maxlength="512"
          required
        ></textarea>
        <label for="rollout-approval">{{ $t("approvalId") }}</label>
        <input
          id="rollout-approval"
          v-model="rolloutApprovalId"
          autocomplete="off"
          required
        />
        <p v-if="rolloutError" class="page-error" role="alert">
          {{ rolloutError }}
        </p>
        <footer>
          <button type="button" @click="rolloutDialog = false">
            {{ $t("cancel") }}
          </button>
          <button
            type="submit"
            class="primary"
            :disabled="
              rolloutStarting ||
              !rolloutReason.trim() ||
              !rolloutApprovalId.trim() ||
              rolloutBatchSize < 1 ||
              rolloutBatchSize > 20
            "
          >
            {{ $t(rolloutStarting ? "rolloutStarting" : "startRollout") }}
          </button>
        </footer>
      </form>
    </div>
  </main>
</template>
