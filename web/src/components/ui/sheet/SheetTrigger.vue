<script setup lang="ts">
import type { DialogTriggerProps } from "reka-ui";
import { DialogTrigger } from "reka-ui";

const props = withDefaults(defineProps<DialogTriggerProps>(), {
  as: "button",
});
</script>

<template>
  <DialogTrigger data-slot="sheet-trigger" v-bind="props">
    <slot />
  </DialogTrigger>
</template>
