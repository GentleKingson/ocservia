<script setup lang="ts">
import type { Component } from "vue";
import { RouterLink, type RouteLocationRaw } from "vue-router";

defineProps<{
  title: string;
  to: RouteLocationRaw;
  value: string;
  valueTestid: string;
  icon: Component;
  source: string;
  warning?: string | undefined;
}>();
</script>

<template>
  <RouterLink
    :to="to"
    :aria-label="title"
    class="bg-card border-border text-card-foreground hover:border-primary/50 focus-visible:ring-ring grid min-w-0 content-start gap-3 rounded-xl border p-5 no-underline shadow-xs motion-safe:transition-[border-color,box-shadow] hover:shadow-sm focus-visible:ring-2 focus-visible:ring-offset-2 focus-visible:outline-none"
  >
    <div class="flex items-start justify-between gap-3">
      <h2 class="text-muted-foreground m-0 text-sm font-medium">
        {{ title }}
      </h2>
      <component
        :is="icon"
        class="text-primary size-5 shrink-0"
        aria-hidden="true"
      />
    </div>
    <div class="grid gap-1">
      <strong
        class="text-foreground text-3xl leading-tight font-semibold tracking-tight wrap-anywhere tabular-nums md:text-4xl"
        :data-testid="valueTestid"
        >{{ value }}</strong
      >
      <p v-if="$slots.default" class="text-foreground m-0 text-sm">
        <slot />
      </p>
    </div>
    <p v-if="warning" class="m-0 text-xs text-amber-800">{{ warning }}</p>
    <p class="text-muted-foreground m-0 text-xs">{{ source }}</p>
  </RouterLink>
</template>
