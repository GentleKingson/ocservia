<script setup lang="ts">
import { Label } from "@/components/ui/label";

defineProps<{ id: string; label: string; help?: string | undefined }>();
</script>

<template>
  <div class="grid min-w-0 gap-2">
    <Label :for="id">{{ label }}</Label>
    <slot :described-by="help ? `${id}-help` : undefined" />
    <p v-if="help" :id="`${id}-help`" class="text-muted-foreground m-0 text-xs">
      {{ help }}
    </p>
  </div>
</template>
