<script setup lang="ts">
import type { HTMLAttributes } from "vue";
import { cn } from "@/lib/utils";

const props = defineProps<{
  class?: HTMLAttributes["class"];
}>();
</script>

<template>
  <tr
    data-slot="table-row"
    :class="
      cn(
        'hover:bg-muted/70 data-[state=selected]:bg-accent border-b motion-safe:transition-colors',
        props.class,
      )
    "
  >
    <slot />
  </tr>
</template>
