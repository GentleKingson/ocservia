<script setup lang="ts">
import { Button } from "@/components/ui/button";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";

// Mount with v-if: the dialog opens on mount, and every way of closing it
// (Cancel, Escape, outside click, the close button) emits `close` so the
// owner keeps a single close path.
defineProps<{
  title: string;
  subject?: string | undefined;
  error?: string | undefined;
  wide?: boolean;
}>();
const emit = defineEmits<{ close: []; submit: [] }>();

function changeOpen(open: boolean): void {
  if (!open) emit("close");
}
</script>

<template>
  <Dialog :open="true" @update:open="changeOpen">
    <DialogContent
      :close-label="$t('closeDialog')"
      :class="wide ? 'sm:max-w-2xl' : undefined"
    >
      <form class="grid min-w-0 gap-4" @submit.prevent="emit('submit')">
        <DialogHeader class="pr-8">
          <DialogTitle>{{ title }}</DialogTitle>
          <DialogDescription class="break-words">
            <code v-if="subject" class="break-all">{{ subject }}</code>
            <slot name="description" />
          </DialogDescription>
        </DialogHeader>
        <slot />
        <p
          v-if="error"
          role="alert"
          class="text-destructive m-0 text-sm break-words"
        >
          {{ error }}
        </p>
        <DialogFooter>
          <Button type="button" variant="outline" @click="emit('close')">
            {{ $t("cancel") }}
          </Button>
          <slot name="footer" />
        </DialogFooter>
      </form>
    </DialogContent>
  </Dialog>
</template>
