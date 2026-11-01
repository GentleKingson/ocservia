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
    class="bg-card border-border text-card-foreground hover:border-primary/50 focus-visible:ring-ring flex min-w-0 flex-col gap-3 rounded-xl border p-5 no-underline shadow-xs motion-safe:transition-[border-color,box-shadow] hover:shadow-sm focus-visible:ring-2 focus-visible:ring-offset-2 focus-visible:outline-none"
  >
    <div class="flex items-start justify-between gap-3">
      <h2 class="text-muted-foreground m-0 text-sm font-medium">
        {{ title }}
      </h2>
      <span
        class="bg-accent text-primary flex size-8 shrink-0 items-center justify-center rounded-lg"
      >
        <component :is="icon" class="size-4" aria-hidden="true" />
      </span>
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
    <p
      class="text-muted-foreground border-border m-0 mt-auto border-t pt-3 text-xs"
    >
      {{ source }}
    </p>
  </RouterLink>
</template>
