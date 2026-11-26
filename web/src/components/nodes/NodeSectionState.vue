<script setup lang="ts">
import DataState from "@/components/common/DataState.vue";
import type { DetailReadState } from "@/shared/fleet";

defineProps<{ state: DetailReadState }>();
</script>

<template>
  <DataState
    v-if="state !== 'ready'"
    :kind="state === 'loading' ? 'loading' : 'error'"
    :message="
      $t(
        state === 'loading'
          ? 'loading'
          : state === 'stale'
            ? 'nodeSectionStale'
            : 'nodeSectionUnavailable',
      )
    "
  />
</template>
