<script setup lang="ts">
import { ArrowLeft } from "@lucide/vue";

import CopyButton from "@/components/common/CopyButton.vue";

defineProps<{
  title: string;
  nodeId: string | undefined;
  status: string;
  tone: "ok" | "error" | "muted";
}>();

// Return to the list view the user came from, keeping its filters, when the
// previous history entry is the Nodes list.
const back = typeof window === "undefined" ? undefined : window.history.state;
const backTo =
  typeof back?.back === "string" && /^\/nodes(?:\?|$)/.test(back.back)
    ? back.back
    : "/nodes";

const tones = {
  ok: ["text-success", "bg-success"],
  error: ["text-destructive", "bg-destructive"],
  muted: ["text-muted-foreground", "bg-muted-foreground/50"],
} as const;
</script>

<template>
  <header class="mb-6 grid gap-3">
    <RouterLink
      :to="backTo"
      class="text-primary focus-visible:outline-ring inline-flex w-fit items-center gap-1.5 rounded-sm text-sm font-medium hover:underline focus-visible:outline-2 focus-visible:outline-offset-2"
    >
      <ArrowLeft class="size-4" aria-hidden="true" />
      {{ $t("backToNodes") }}
    </RouterLink>
    <div
      class="flex flex-col gap-2 md:flex-row md:items-end md:justify-between"
    >
      <div class="grid min-w-0 gap-1">
        <p class="text-muted-foreground text-xs font-semibold uppercase">
          {{ $t("nodeDetail") }}
        </p>
        <h1 class="text-2xl font-semibold break-words">{{ title }}</h1>
        <p
          v-if="nodeId"
          class="text-muted-foreground flex min-w-0 items-center gap-1 text-xs"
        >
          <span class="shrink-0">{{ $t("nodeId") }}</span>
          <code class="min-w-0 break-all">{{ nodeId }}</code>
          <CopyButton :value="nodeId" :label="$t('nodeId')" />
        </p>
      </div>
      <span
        class="inline-flex shrink-0 items-center gap-2 text-sm font-medium"
        :class="tones[tone][0]"
        data-testid="node-detail-status"
      >
        <span
          class="size-2 rounded-full"
          :class="tones[tone][1]"
          aria-hidden="true"
        ></span>
        {{ status }}
      </span>
    </div>
  </header>
</template>
