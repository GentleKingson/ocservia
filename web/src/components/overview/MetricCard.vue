<script setup lang="ts">
import type { Component } from "vue";

defineProps<{
  title: string;
  value: string;
  valueTestid: string;
  icon: Component;
  source: string;
  warning?: string | undefined;
}>();
</script>

<template>
  <article
    class="bg-card border-border grid content-start gap-2 rounded-lg border p-4"
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
        class="text-foreground text-2xl leading-none font-semibold"
        :data-testid="valueTestid"
        >{{ value }}</strong
      >
      <p v-if="$slots.default" class="text-foreground m-0 text-sm">
        <slot />
      </p>
    </div>
    <p v-if="warning" class="m-0 text-xs text-amber-800">{{ warning }}</p>
    <p class="text-muted-foreground m-0 text-xs">{{ source }}</p>
  </article>
</template>
