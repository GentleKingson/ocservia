<script setup lang="ts">
import { CircleAlert, LoaderCircle } from "@lucide/vue";

defineProps<{ kind: "loading" | "error"; message: string }>();
</script>

<template>
  <div
    :role="kind === 'error' ? 'alert' : 'status'"
    class="bg-card border-border grid min-h-56 place-content-center justify-items-center gap-2.5 rounded-lg border p-7 text-center text-sm"
    :class="kind === 'error' ? 'text-destructive' : 'text-muted-foreground'"
  >
    <LoaderCircle
      v-if="kind === 'loading'"
      class="size-6 motion-safe:animate-spin"
      aria-hidden="true"
    />
    <CircleAlert v-else class="size-6" aria-hidden="true" />
    <span>{{ message }}</span>
  </div>
</template>
