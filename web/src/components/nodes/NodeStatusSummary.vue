<script setup lang="ts">
import type { NodeObservedState } from "@ocservia/api-client";
import { useNow } from "@vueuse/core";
import { useI18n } from "vue-i18n";

import { agentVersionLabel } from "@/shared/agent-version";
import { formatTimestamp, relativeTimestamp } from "@/shared/timestamp";

defineProps<{ node: NodeObservedState; sessionCount: number }>();

const { locale } = useI18n();
const now = useNow({ interval: 30_000 });
const relative = (value: string) =>
  relativeTimestamp(value, now.value.getTime(), locale.value);

const tile =
  "bg-card border-border grid content-start gap-1 rounded-lg border px-4 py-3";
const label = "text-muted-foreground text-xs";
const value = "text-sm font-semibold break-words";
</script>

<template>
  <section
    class="mb-6 grid grid-cols-2 gap-3 lg:grid-cols-5"
    :aria-label="$t('observedState')"
  >
    <div :class="tile">
      <span :class="label">{{ $t("trust") }}</span>
      <span
        :class="[
          value,
          node.trustStatus === 'active'
            ? 'text-success'
            : node.trustStatus === 'revoked'
              ? 'text-destructive'
              : '',
        ]"
        >{{ $t(node.trustStatus) }}</span
      >
    </div>
    <div :class="tile">
      <span :class="label">{{ $t("connection") }}</span>
      <span :class="value" class="inline-flex items-center gap-1.5">
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
        class="text-xs"
        :class="
          node.freshness === 'stale'
            ? 'text-destructive'
            : 'text-muted-foreground'
        "
        >{{ $t(node.freshness) }}</span
      >
    </div>
    <div :class="tile">
      <span :class="label">{{ $t("lastHeartbeat") }}</span>
      <template v-if="node.lastHeartbeatAt">
        <span :class="value">{{
          relative(node.lastHeartbeatAt) ??
          formatTimestamp(node.lastHeartbeatAt)
        }}</span>
        <time
          v-if="relative(node.lastHeartbeatAt)"
          class="text-muted-foreground text-xs break-words"
          :datetime="node.lastHeartbeatAt"
          >{{ formatTimestamp(node.lastHeartbeatAt) }}</time
        >
      </template>
      <span v-else :class="value">{{ $t("notObserved") }}</span>
    </div>
    <div :class="tile">
      <span :class="label">{{ $t("agent") }}</span>
      <span :class="value">{{ node.agentVersion ?? $t("unknown") }}</span>
      <span class="text-muted-foreground text-xs">{{
        $t(agentVersionLabel(node))
      }}</span>
    </div>
    <div :class="tile">
      <span :class="label">{{ $t("sessions") }}</span>
      <span :class="value" class="tabular-nums">{{ sessionCount }}</span>
    </div>
  </section>
</template>
