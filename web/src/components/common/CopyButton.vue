<script setup lang="ts">
import { Check, Copy } from "@lucide/vue";
import { ref } from "vue";

import { Button } from "@/components/ui/button";

const props = defineProps<{ value: string; label: string }>();
const status = ref<"" | "copied" | "copyFailed">("");

// The clipboard is unavailable outside secure contexts or when denied; the
// value stays selectable text either way.
async function copy(): Promise<void> {
  try {
    await navigator.clipboard.writeText(props.value);
    status.value = "copied";
  } catch {
    status.value = "copyFailed";
  }
}
</script>

<template>
  <span class="inline-flex shrink-0 items-center gap-1">
    <Button
      type="button"
      variant="ghost"
      size="icon"
      class="text-muted-foreground size-7"
      :aria-label="$t('copyValue', { label })"
      :title="$t('copyValue', { label })"
      @click="copy"
      @blur="status = ''"
    >
      <Check v-if="status === 'copied'" aria-hidden="true" />
      <Copy v-else aria-hidden="true" />
    </Button>
    <span
      class="text-xs"
      :class="status === 'copyFailed' ? 'text-destructive' : 'sr-only'"
      role="status"
      >{{ status ? $t(status) : "" }}</span
    >
  </span>
</template>
