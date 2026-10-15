<script setup lang="ts">
defineProps<{ eyebrow?: string; title: string; description?: string }>();
</script>

<template>
  <div
    class="mb-6 flex flex-col items-start gap-3 md:flex-row md:items-end md:justify-between md:gap-6"
  >
    <div class="min-w-0">
      <p
        v-if="eyebrow"
        class="text-muted-foreground m-0 mb-1 text-xs font-medium tracking-wide"
      >
        {{ eyebrow }}
      </p>
      <h1
        class="text-foreground m-0 text-2xl leading-tight font-semibold tracking-tight wrap-anywhere md:text-3xl"
      >
        {{ title }}
      </h1>
      <p
        v-if="description"
        class="text-muted-foreground m-0 mt-2 max-w-2xl text-sm leading-relaxed"
      >
        {{ description }}
      </p>
    </div>
    <div
      v-if="$slots.actions"
      class="flex min-w-0 flex-wrap items-center gap-2"
    >
      <slot name="actions" />
    </div>
  </div>
</template>
