<script setup lang="ts">
import NodeDetailNav from "./NodeDetailNav.vue";
</script>

<template>
  <div role="status" aria-busy="true" :aria-label="$t('nodeLoading')">
    <div class="mb-6 grid grid-cols-2 gap-3 lg:grid-cols-5" aria-hidden="true">
      <div
        v-for="tile in 5"
        :key="tile"
        class="bg-card border-border h-[4.75rem] rounded-lg border"
      ></div>
    </div>
    <NodeDetailNav placeholder />
    <div
      class="bg-card border-border grid gap-4 rounded-lg border p-5"
      aria-hidden="true"
    >
      <div
        v-for="line in 6"
        :key="line"
        class="bg-muted h-3 rounded-sm motion-safe:animate-pulse"
        :class="line % 3 === 0 ? 'w-1/2' : 'w-3/4'"
      ></div>
    </div>
  </div>
</template>
