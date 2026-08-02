<script setup lang="ts">
import type { SimulationScenario } from "@ocservia/api-client";
import { Clock3, Play, Server, Workflow } from "@lucide/vue";
import { onMounted, ref } from "vue";

import { useLocalSliceStore } from "../shared/localSlice";
import { useReadinessStore } from "../shared/readiness";

const readiness = useReadinessStore();
const slice = useLocalSliceStore();
const mode = ref<"normal" | "duplicate" | "error" | "disconnect">("normal");

const scenarios: Record<typeof mode.value, SimulationScenario> = {
  normal: { heartbeatCount: 3, delayMillis: 100 },
  duplicate: { heartbeatCount: 3, delayMillis: 100, duplicateEvent: true },
  error: { heartbeatCount: 2, delayMillis: 100, returnError: true },
  disconnect: {
    heartbeatCount: 2,
    delayMillis: 100,
    disconnectAfter: true,
  },
};

onMounted(async () => {
  await slice.rebuild();
  slice.connect();
});

function eventLabel(type: string): string {
  const labels: Record<string, string> = {
    connected: "eventConnected",
    disconnected: "eventDisconnected",
    command_result: "eventCommandResult",
    heartbeat: "eventHeartbeat",
    error: "eventError",
  };
  return labels[type] ?? type;
}
</script>

<template>
  <main class="overview">
    <div class="page-heading">
      <div>
        <p>{{ $t("workspace") }}</p>
        <h1>{{ $t("overview") }}</h1>
      </div>
      <span class="health" :class="{ unavailable: !readiness.isReady }"
        ><i></i
        >{{ $t(readiness.isReady ? "allSystems" : "systemsUnavailable") }}</span
      >
    </div>
    <section class="metrics" aria-label="Platform status">
      <article>
        <div>
          <span>{{ $t("controlPlane") }}</span
          ><strong>{{
            $t(readiness.isReady ? "ready" : "unavailable")
          }}</strong>
        </div>
        <Server :size="20" />
      </article>
      <article>
        <div>
          <span>{{ $t("activeNodes") }}</span
          ><strong data-testid="active-nodes">{{ slice.activeNodes }}</strong>
        </div>
        <Workflow :size="20" />
      </article>
      <article>
        <div>
          <span>{{ $t("pendingOperations") }}</span
          ><strong data-testid="pending-operations">{{
            slice.pendingOperations
          }}</strong>
        </div>
        <Clock3 :size="20" />
      </article>
    </section>
    <section class="activity-panel">
      <header class="activity-header">
        <h2>{{ $t("recentActivity") }}</h2>
        <div class="probe-controls">
          <div class="segmented" :aria-label="$t('probeMode')">
            <button
              v-for="choice in [
                'normal',
                'duplicate',
                'error',
                'disconnect',
              ] as const"
              :key="choice"
              type="button"
              :class="{ active: mode === choice }"
              @click="mode = choice"
            >
              {{ $t(choice) }}
            </button>
          </div>
          <button
            class="run-probe"
            type="button"
            :disabled="slice.running"
            :title="$t('runProbe')"
            :aria-label="$t('runProbe')"
            data-testid="run-probe"
            @click="slice.run(scenarios[mode])"
          >
            <Play :size="16" fill="currentColor" />
          </button>
        </div>
      </header>
      <div v-if="slice.events.length === 0" class="empty-state">
        <Clock3 :size="24" /><span>{{ $t("noActivity") }}</span>
      </div>
      <ol v-else class="event-list" data-testid="event-list">
        <li v-for="event in [...slice.events].reverse()" :key="event.id">
          <span class="event-type">{{ $t(eventLabel(event.type)) }}</span>
          <code>{{ event.nodeId.slice(0, 8) }}</code>
          <time :datetime="event.occurredAt.toISOString()">{{
            event.occurredAt.toLocaleTimeString()
          }}</time>
        </li>
      </ol>
    </section>
  </main>
</template>
