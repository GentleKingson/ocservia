<script setup lang="ts">
defineProps<{ placeholder?: boolean }>();

const sections = [
  ["node-overview", "nodeOverview"],
  ["node-sessions", "sessions"],
  ["node-users-groups", "usersAndGroups"],
  ["node-configuration", "configurationSection"],
  ["node-certificates", "certificatesSection"],
] as const;
const item =
  "text-muted-foreground shrink-0 border-b-2 border-transparent px-3 py-2.5 text-sm font-medium";
</script>

<template>
  <div
    v-if="placeholder"
    class="border-border mb-4 flex overflow-x-auto border-b"
    aria-hidden="true"
  >
    <span v-for="[, label] in sections" :key="label" :class="item">{{
      $t(label)
    }}</span>
  </div>
  <nav
    v-else
    class="border-border mb-4 flex overflow-x-auto border-b"
    :aria-label="$t('nodeDetail')"
  >
    <a
      v-for="[id, label] in sections"
      :key="id"
      :href="`#${id}`"
      :class="item"
      class="hover:text-foreground hover:border-primary focus-visible:outline-ring focus-visible:text-foreground focus-visible:outline-2 focus-visible:-outline-offset-2"
      >{{ $t(label) }}</a
    >
  </nav>
</template>
