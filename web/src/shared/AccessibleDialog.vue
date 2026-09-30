<script setup lang="ts">
import { onBeforeUnmount, onMounted, ref } from "vue";

defineProps<{ labelledby: string }>();
const emit = defineEmits<{ close: [] }>();
const dialog = ref<HTMLDialogElement>();
let returnFocus: HTMLElement | undefined;
onMounted(() => {
  if (document.activeElement instanceof HTMLElement)
    returnFocus = document.activeElement;
  dialog.value?.showModal();
});
function cycleFocus(event: KeyboardEvent): void {
  if (event.key !== "Tab" || !dialog.value) return;
  const fields = Array.from(
    dialog.value.querySelectorAll<HTMLElement>(
      "button, input, select, textarea, a[href], [tabindex]",
    ),
  ).filter(
    (element) =>
      !element.matches(":disabled") &&
      element.tabIndex >= 0 &&
      element.getClientRects().length > 0,
  );
  const first = fields[0];
  const last = fields.at(-1);
  if (!first || !last) {
    event.preventDefault();
    return;
  }
  if (event.shiftKey && document.activeElement === first) {
    event.preventDefault();
    last.focus();
  } else if (!event.shiftKey && document.activeElement === last) {
    event.preventDefault();
    first.focus();
  }
}
onBeforeUnmount(() => {
  dialog.value?.close();
  if (returnFocus?.isConnected) returnFocus.focus();
});
</script>

<template>
  <dialog
    ref="dialog"
    class="accessible-dialog"
    :aria-labelledby="labelledby"
    aria-modal="true"
    @keydown="cycleFocus"
    @cancel.prevent="emit('close')"
    @click.self="emit('close')"
  >
    <slot />
  </dialog>
</template>
