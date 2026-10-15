<script setup lang="ts">
import { useId } from "vue";

defineProps<{ title: string }>();
const headingId = useId();
</script>

<template>
  <section
    class="bg-card border-border mb-6 min-w-0 scroll-mt-4 rounded-xl border shadow-xs"
    :aria-labelledby="headingId"
  >
    <header
      class="border-border flex flex-wrap items-center justify-between gap-3 border-b wrap-anywhere px-4 py-3 md:px-5"
    >
      <h2 :id="headingId" class="m-0 text-sm font-semibold">{{ title }}</h2>
      <div v-if="$slots.actions" class="flex flex-wrap items-center gap-2">
        <slot name="actions" />
      </div>
    </header>
    <div class="grid min-w-0 grid-cols-1 gap-4 p-4 md:p-5">
      <slot />
    </div>
  </section>
</template>
