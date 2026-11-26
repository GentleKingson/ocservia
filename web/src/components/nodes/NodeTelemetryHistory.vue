<script setup lang="ts">
import { ref, watchEffect } from "vue";
import type { NodeObservedState, Schemas } from "@/api/models";
import { listNodeTelemetry } from "@/api/nodes";
import { NativeSelect } from "@/components/ui/native-select";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
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

function setMetric(value: unknown): void {
  if (
    typeof value === "string" &&
    (metrics as readonly string[]).includes(value)
  ) {
    metric.value = value as Schemas["TelemetryMetric"];
  }
}

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
      <NativeSelect :model-value="metric" @update:model-value="setMetric">
        <option v-for="name in metrics" :key="name" :value="name">
          {{ $t(name) }}
        </option>
      </NativeSelect>
    </label>
    <p v-if="status" role="status">{{ $t(status) }}</p>
    <div v-else class="max-h-64 overflow-auto">
      <Table class="tabular-nums">
        <TableHeader>
          <TableRow class="hover:bg-transparent">
            <TableHead scope="col">{{ $t("telemetryTime") }}</TableHead>
            <TableHead scope="col">{{ $t(metric) }}</TableHead>
          </TableRow>
        </TableHeader>
        <TableBody>
          <TableRow v-for="sample in samples" :key="sample.at">
            <TableCell>
              <time :datetime="sample.at">{{
                formatTimestamp(sample.at)
              }}</time>
            </TableCell>
            <TableCell>
              {{
                (
                  sample.average * (metric === "cpu_usage_ratio" ? 100 : 1)
                ).toLocaleString()
              }}
            </TableCell>
          </TableRow>
        </TableBody>
      </Table>
    </div>
  </details>
</template>
