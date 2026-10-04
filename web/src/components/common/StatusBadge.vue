<script setup lang="ts">
import { Badge } from "@/components/ui/badge";
import type { StateTone } from "@/features/operations/state-tone";

defineProps<{ tone: StateTone; label: string }>();

const toneClasses: Record<StateTone, string> = {
  success: "border-success/40 text-success",
  danger: "border-destructive/40 text-destructive",
  warning: "border-amber-300 text-amber-800",
  neutral: "text-muted-foreground",
};
</script>

<template>
  <Badge variant="outline" :class="toneClasses[tone]">{{ label }}</Badge>
</template>
