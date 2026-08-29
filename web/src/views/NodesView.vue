<script setup lang="ts">
import { Radio, Server, Users } from "@lucide/vue";
import type { NodeObservedState } from "@ocservia/api-client";
import { computed, onMounted, ref } from "vue";
import { useI18n } from "vue-i18n";
import { useRouter } from "vue-router";

import { createAgentRollout } from "../api/client";
import { useFleetStore } from "../shared/fleet";

const fleet = useFleetStore();
const router = useRouter();
const { t } = useI18n();

onMounted(async () => {
  if (!fleet.initialized) await fleet.rebuild();
  if (fleet.initialized) void fleet.connect();
});

function pathMode(node: NodeObservedState): "relay" | "direct" | "unknown" {
  if (node.path?.mode === "relay") return "relay";
  if (node.path?.mode === "direct") return "direct";
  return "unknown";
}

function pathRtt(node: NodeObservedState): string {
  return node.path ? String(Math.round(node.path.rttMs)) : "";
}

function openNode(nodeId: string): void {
  void router.push({ name: "node-detail", params: { nodeId } });
}

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

function eligibleNodes(): NodeObservedState[] {
  return fleet.nodes.filter((node) => node.agentUpgradeEligible);
}

function selectAllEligible(event: Event): void {
  const checked = (event.target as HTMLInputElement).checked;
  selected.value = checked ? eligibleNodes().map((node) => node.id) : [];
}

const allEligibleSelected = computed(
  () =>
    eligibleNodes().length > 0 &&
    eligibleNodes().every((node) => selected.value.includes(node.id)),
);

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
  <main class="overview nodes-view">
    <div class="page-heading">
      <div>
        <p>{{ $t("fleet") }}</p>
        <h1>{{ $t("nodes") }}</h1>
      </div>
      <span class="health" :class="{ unavailable: fleet.unavailable }"
        ><i></i
        >{{
          $t(fleet.unavailable ? "systemsUnavailable" : "liveTelemetry")
        }}</span
      >
    </div>
    <section class="metrics" :aria-label="$t('fleetStatus')">
      <article>
        <div>
          <span>{{ $t("onlineNodes") }}</span
          ><strong>{{ fleet.online }} / {{ fleet.nodes.length }}</strong>
        </div>
        <Server :size="20" />
      </article>
      <article>
        <div>
          <span>{{ $t("relayPaths") }}</span
          ><strong>{{ fleet.relay }}</strong>
        </div>
        <Radio :size="20" />
      </article>
      <article>
        <div>
          <span>{{ $t("activeSessions") }}</span
          ><strong>{{ fleet.sessionCount }}</strong>
        </div>
        <Users :size="20" />
      </article>
    </section>
    <section class="fleet-list-panel">
      <div class="node-table-wrap">
        <table class="node-table fleet-table">
          <thead>
            <tr>
              <th>
                <input
                  type="checkbox"
                  :aria-label="$t('rollingUpgrade')"
                  :checked="allEligibleSelected"
                  :disabled="eligibleNodes().length === 0"
                  @change="selectAllEligible"
                />
              </th>
              <th>{{ $t("node") }}</th>
              <th>{{ $t("trust") }}</th>
              <th>{{ $t("connection") }}</th>
              <th>{{ $t("freshness") }}</th>
              <th>{{ $t("path") }}</th>
              <th>{{ $t("lastHeartbeat") }}</th>
              <th>{{ $t("agent") }}</th>
              <th>{{ $t("ocserv") }}</th>
              <th>{{ $t("sessions") }}</th>
            </tr>
          </thead>
          <tbody>
            <tr
              v-for="node in fleet.nodes"
              :key="node.id"
              :class="{ selected: fleet.selected?.id === node.id }"
              tabindex="0"
              @click="openNode(node.id)"
              @keydown.enter="openNode(node.id)"
            >
              <td @click.stop>
                <input
                  v-if="node.agentUpgradeEligible"
                  type="checkbox"
                  :aria-label="`${$t('rollingUpgrade')}: ${node.name}`"
                  :checked="selected.includes(node.id)"
                  @change="toggleSelection(node.id)"
                />
              </td>
              <td>
                <strong>{{ node.name }}</strong
                ><code>{{ node.id.slice(0, 8) }}</code>
              </td>
              <td>{{ $t(node.trustStatus) }}</td>
              <td>{{ $t(node.connectionState) }}</td>
              <td>
                <span class="state-dot" :class="node.freshness"></span
                >{{ $t(node.freshness) }}
              </td>
              <td>
                <span>{{ $t(pathMode(node)) }}</span
                ><small v-if="node.path"
                  >{{ pathRtt(node) }} {{ $t("milliseconds") }}</small
                >
              </td>
              <td>
                <time
                  v-if="node.lastHeartbeatAt"
                  :datetime="node.lastHeartbeatAt.toISOString()"
                  >{{ node.lastHeartbeatAt.toLocaleString() }}</time
                ><span v-else>{{ $t("notObserved") }}</span>
              </td>
              <td>
                <span>{{ node.agentVersion ?? $t("notAvailable") }}</span>
                <span
                  v-if="node.agentVersionState"
                  class="version-badge"
                  :class="node.agentVersionState"
                  >{{ $t(node.agentVersionState) }}</span
                >
              </td>
              <td>{{ node.ocservVersion ?? $t("notAvailable") }}</td>
              <td>{{ node.sessionCount }}</td>
            </tr>
          </tbody>
        </table>
        <div
          v-if="
            fleet.loading || (!fleet.unavailable && fleet.nodes.length === 0)
          "
          class="empty-state"
        >
          <Server :size="24" /><span>{{
            fleet.loading ? $t("loading") : $t("noNodes")
          }}</span>
        </div>
        <div v-else-if="fleet.unavailable" class="empty-state" role="alert">
          <Server :size="24" /><span>{{ $t("systemsUnavailable") }}</span>
        </div>
      </div>
      <div class="rollout-actions">
        <button
          type="button"
          class="primary"
          :disabled="selectedNodes.length === 0"
          @click="openRolloutDialog"
        >
          {{ $t("rollingUpgrade") }} ({{ selectedNodes.length }})
        </button>
      </div>
    </section>

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
        <output id="rollout-nodes" class="read-only-value">{{
          selectedNodes.length
        }}</output>
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
