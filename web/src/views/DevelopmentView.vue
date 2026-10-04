<script setup lang="ts">
import type { SimulationScenario } from "@ocservia/api-client";
import { Clock3, Play, Server, Workflow } from "@lucide/vue";
import { onMounted, ref } from "vue";

import PageHeader from "../components/common/PageHeader.vue";
import SectionCard from "../components/common/SectionCard.vue";
import StatusBadge from "../components/common/StatusBadge.vue";
import { Button } from "../components/ui/button";
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
  void slice.connect();
});

function eventLabel(type: string): string {
  const labels: Record<string, string> = {
    connected: "eventConnected",
    disconnected: "eventDisconnected",
    command_result: "eventCommandResult",
    simulation_result: "eventCommandResult",
    heartbeat: "eventHeartbeat",
    error: "eventError",
  };
  return labels[type] ?? type;
}

function timeLabel(value: string): string {
  if (!/^\d{4}-/.test(value)) return value;
  const date = new Date(value);
  return Number.isFinite(date.getTime()) ? date.toLocaleTimeString() : value;
}
</script>

<template>
  <main>
    <PageHeader :eyebrow="$t('workspace')" :title="$t('development')">
      <template #actions>
        <StatusBadge
          :tone="readiness.isReady ? 'success' : 'danger'"
          :label="$t(readiness.isReady ? 'allSystems' : 'systemsUnavailable')"
        />
      </template>
    </PageHeader>
    <section
      class="mb-6 grid gap-3.5 md:grid-cols-3"
      aria-label="Platform status"
    >
      <article
        v-for="metric in [
          {
            label: 'controlPlane',
            value: $t(readiness.isReady ? 'ready' : 'unavailable'),
            icon: Server,
          },
          {
            label: 'activeNodes',
            value: slice.activeNodes,
            icon: Workflow,
            testid: 'active-nodes',
          },
          {
            label: 'pendingOperations',
            value: slice.pendingOperations,
            icon: Clock3,
            testid: 'pending-operations',
          },
        ]"
        :key="metric.label"
        class="bg-card border-border flex items-start justify-between gap-3 rounded-lg border p-4"
      >
        <div class="grid gap-2">
          <span class="text-muted-foreground text-sm">{{
            $t(metric.label)
          }}</span>
          <strong
            class="text-2xl leading-none font-semibold"
            :data-testid="metric.testid"
            >{{ metric.value }}</strong
          >
        </div>
        <component
          :is="metric.icon"
          class="text-muted-foreground size-5 shrink-0"
          aria-hidden="true"
        />
      </article>
    </section>
    <SectionCard :title="$t('recentActivity')">
      <template #actions>
        <div
          class="border-border flex overflow-hidden rounded-md border"
          role="group"
          :aria-label="$t('probeMode')"
        >
          <Button
            v-for="choice in [
              'normal',
              'duplicate',
              'error',
              'disconnect',
            ] as const"
            :key="choice"
            type="button"
            size="sm"
            :variant="mode === choice ? 'default' : 'ghost'"
            class="border-border rounded-none border-r text-xs last:border-r-0"
            :aria-pressed="mode === choice"
            @click="mode = choice"
          >
            {{ $t(choice) }}
          </Button>
        </div>
        <Button
          type="button"
          size="icon-sm"
          :disabled="slice.running"
          :title="$t('runProbe')"
          :aria-label="$t('runProbe')"
          data-testid="run-probe"
          @click="slice.run(scenarios[mode])"
        >
          <Play fill="currentColor" aria-hidden="true" />
        </Button>
      </template>
      <div
        v-if="slice.events.length === 0"
        class="text-muted-foreground grid min-h-40 place-content-center justify-items-center gap-2.5 text-sm"
      >
        <Clock3 class="size-6" aria-hidden="true" /><span>{{
          $t("noActivity")
        }}</span>
      </div>
      <ol
        v-else
        class="m-0 max-h-80 list-none overflow-auto p-0"
        data-testid="event-list"
      >
        <li
          v-for="event in [...slice.events].reverse()"
          :key="event.id"
          class="border-border text-muted-foreground grid min-h-12 grid-cols-[minmax(92px,1fr)_auto] items-center gap-4 border-b text-xs last:border-b-0 md:grid-cols-[minmax(110px,1fr)_minmax(80px,1fr)_auto]"
        >
          <span class="text-foreground font-semibold">{{
            $t(eventLabel(event.type))
          }}</span>
          <code>{{ event.nodeId.slice(0, 8) }}</code>
          <time class="hidden md:block" :datetime="event.occurredAt">{{
            timeLabel(event.occurredAt)
          }}</time>
        </li>
      </ol>
    </SectionCard>
  </main>
</template>
