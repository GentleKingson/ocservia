<script setup lang="ts">
import type { NodeObservedState } from "@/api/models";
import { useIntervalFn, useNow } from "@vueuse/core";
import { useI18n } from "vue-i18n";

import { agentVersionLabel } from "@/shared/agent-version";
import { formatTimestamp, relativeTimestamp } from "@/shared/timestamp";

defineProps<{ node: NodeObservedState; sessionCount: number }>();

const { locale } = useI18n();
const now = useNow({ scheduler: (update) => useIntervalFn(update, 30_000) });
const relative = (value: string) =>
  relativeTimestamp(value, now.value.getTime(), locale.value);

const tile =
  "bg-card border-border grid min-w-0 grid-cols-1 content-start gap-1 rounded-xl border px-4 py-3 shadow-xs";
const label = "text-muted-foreground text-xs";
const value = "text-sm font-semibold wrap-anywhere";
</script>

<template>
  <section
    class="mb-6 grid grid-cols-[repeat(auto-fit,minmax(min(100%,8rem),1fr))] gap-3"
    :aria-label="$t('observedState')"
  >
    <div :class="tile">
      <span :class="label">{{ $t("trust") }}</span>
      <span
        :class="[
          value,
          node.trust_status === 'active'
            ? 'text-success'
            : node.trust_status === 'revoked'
              ? 'text-destructive'
              : '',
        ]"
        >{{ $t(node.trust_status) }}</span
      >
    </div>
    <div :class="tile">
      <span :class="label">{{ $t("connection") }}</span>
      <span :class="value" class="inline-flex items-center gap-1.5">
        <span
          class="size-2 rounded-full"
          :class="
            node.connection_state === 'online'
              ? 'bg-success'
              : 'bg-muted-foreground/50'
          "
          aria-hidden="true"
        ></span>
        {{ $t(node.connection_state) }}
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
      <template v-if="node.last_heartbeat_at">
        <span :class="value">{{
          relative(node.last_heartbeat_at) ??
          formatTimestamp(node.last_heartbeat_at)
        }}</span>
        <time
          v-if="relative(node.last_heartbeat_at)"
          class="text-muted-foreground text-xs break-words"
          :datetime="node.last_heartbeat_at"
          >{{ formatTimestamp(node.last_heartbeat_at) }}</time
        >
      </template>
      <span v-else :class="value">{{ $t("notObserved") }}</span>
    </div>
    <div :class="tile">
      <span :class="label">{{ $t("agent") }}</span>
      <span :class="value">{{ node.agent_version ?? $t("unknown") }}</span>
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
