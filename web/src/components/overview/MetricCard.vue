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
    class="from-primary/5 to-card border-border text-card-foreground hover:border-primary/50 focus-visible:ring-ring grid content-start gap-3 rounded-xl border bg-gradient-to-t p-5 no-underline shadow-xs transition-[border-color,box-shadow] hover:shadow-sm focus-visible:ring-2 focus-visible:ring-offset-2 focus-visible:outline-none"
  >
    <div class="flex items-start justify-between gap-3">
      <h2 class="text-muted-foreground m-0 text-sm font-medium">
        {{ title }}
      </h2>
      <component
        :is="icon"
        class="text-muted-foreground size-5 shrink-0"
        aria-hidden="true"
      />
    </div>
    <div class="grid gap-1">
      <strong
        class="text-foreground text-2xl leading-none font-semibold tabular-nums md:text-3xl"
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
