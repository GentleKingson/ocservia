<script setup lang="ts">
import { ref, watchEffect } from "vue";
import type { NodeObservedState, Schemas } from "@/api/models";
import { listNodeTelemetry } from "@/api/nodes";
import { formatTimestamp } from "@/shared/timestamp";

const props = defineProps<{ node: NodeObservedState }>();
const metrics = [
  "cpu_usage_ratio",
  "memory_used_bytes",
  "network_rx_bytes",
  "network_tx_bytes",
  "connection_rtt_ms",
  "session_count",
] as const;
const metric = ref<Schemas["TelemetryMetric"]>("cpu_usage_ratio");
const open = ref(false);
const samples = ref<Schemas["TelemetryPoint"][]>([]);
const status = ref<"loading" | "unavailable" | "telemetryEmpty" | null>(null);

watchEffect((cleanup) => {
  if (!open.value) return;
  const controller = new AbortController();
  cleanup(() => {
    controller.abort();
  });
  const since = new Date(Date.now() - 600_000).toISOString();
  const node = props.node;
  samples.value = [];
  status.value = "loading";
  listNodeTelemetry(node.id, metric.value, since, controller.signal)
    .then((page) => {
      if (controller.signal.aborted) return;
      samples.value = page.items.slice().reverse();
      status.value = samples.value.length ? null : "telemetryEmpty";
    })
    .catch(() => {
      if (!controller.signal.aborted) status.value = "unavailable";
    });
});
</script>

<template>
  <details
    class="bg-card border-border mb-4 rounded-xl border px-4 py-3 text-sm"
    @toggle="open = ($event.currentTarget as HTMLDetailsElement).open"
  >
    <summary class="cursor-pointer font-semibold">
      {{ $t("telemetryHistory") }}
    </summary>
    <label class="my-3 flex items-center gap-3"
      >{{ $t("telemetryMetric") }}
      <select
        v-model="metric"
        class="border-border bg-card rounded-md border px-2 py-1"
      >
        <option v-for="name in metrics" :key="name" :value="name">
          {{ $t(name) }}
        </option>
      </select>
    </label>
    <p v-if="status" role="status">{{ $t(status) }}</p>
    <div v-else class="max-h-64 overflow-auto">
      <table class="w-full text-left tabular-nums">
        <thead>
          <tr>
            <th scope="col">{{ $t("telemetryTime") }}</th>
            <th scope="col">{{ $t(metric) }}</th>
          </tr>
        </thead>
        <tbody>
          <tr v-for="sample in samples" :key="sample.at">
            <td>
              <time :datetime="sample.at">{{
                formatTimestamp(sample.at)
              }}</time>
            </td>
            <td>
              {{
                (
                  sample.average * (metric === "cpu_usage_ratio" ? 100 : 1)
                ).toLocaleString()
              }}
            </td>
          </tr>
        </tbody>
      </table>
    </div>
  </details>
</template>
