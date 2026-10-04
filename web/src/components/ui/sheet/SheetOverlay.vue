<script setup lang="ts">
import type { DialogOverlayProps } from "reka-ui";
import type { HTMLAttributes } from "vue";
import { reactiveOmit } from "@vueuse/core";
import { DialogOverlay } from "reka-ui";
import { cn } from "@/lib/utils";

const props = withDefaults(
  defineProps<DialogOverlayProps & { class?: HTMLAttributes["class"] }>(),
  { as: "div" },
);

const delegatedProps = reactiveOmit(props, "class");
</script>

<template>
  <DialogOverlay
    data-slot="sheet-overlay"
    :class="cn('fixed inset-0 z-50 bg-black/80', props.class)"
    v-bind="delegatedProps"
  >
    <slot />
  </DialogOverlay>
</template>
