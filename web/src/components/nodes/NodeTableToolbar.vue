<script setup lang="ts">
import { ListFilter, Search, X } from "@lucide/vue";
import { computed, ref, useId } from "vue";

import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import {
  activeFilterCount,
  nodeFilterGroupNames,
  nodeFilterGroups,
  nodeSorts,
  optionalNodeColumns,
  type NodeFilterGroup,
  type NodeListState,
  type OptionalNodeColumn,
} from "@/features/nodes/node-list";

const props = defineProps<{
  state: NodeListState;
  counts: Record<NodeFilterGroup, Record<string, number>>;
  shown: number;
  total: number;
}>();
const emit = defineEmits<{ update: [state: NodeListState] }>();

const panelId = useId();
const panelOpen = ref(false);
const filterCount = computed(() => activeFilterCount(props.state));
const groupFilterCount = computed(
  () => filterCount.value - (props.state.search.trim() ? 1 : 0),
);

const columnLabels: Record<OptionalNodeColumn, string> = {
  path: "path",
  ocserv: "ocserv",
  platform: "archOs",
};

function update(patch: Partial<NodeListState>): void {
  emit("update", { ...props.state, ...patch });
}

function toggle<T extends string>(values: readonly T[], value: T): T[] {
  return values.includes(value)
    ? values.filter((item) => item !== value)
    : [...values, value];
}

function toggleFilter(group: NodeFilterGroup, value: string): void {
  update({
    filters: {
      ...props.state.filters,
      [group]: toggle(props.state.filters[group], value),
    },
  });
}

function clearFilters(): void {
  update({
    search: "",
    filters: Object.fromEntries(
      nodeFilterGroupNames.map((group) => [group, []]),
    ) as unknown as NodeListState["filters"],
  });
}

const chip =
  "border-input text-foreground has-checked:border-primary has-checked:bg-secondary has-focus-visible:outline-ring inline-flex min-h-9 cursor-pointer items-center gap-2 rounded-md border px-3 text-sm has-focus-visible:outline-2 has-focus-visible:outline-offset-1";
</script>

<template>
  <div class="mb-3 grid gap-2">
    <div class="flex flex-wrap items-center gap-2">
      <div class="relative min-w-0 flex-1 basis-56">
        <Search
          class="text-muted-foreground pointer-events-none absolute top-1/2 left-3 size-4 -translate-y-1/2"
          aria-hidden="true"
        />
        <Input
          type="search"
          class="bg-card h-9 pl-9"
          :aria-label="$t('searchNodes')"
          :placeholder="$t('searchNodesPlaceholder')"
          :model-value="state.search"
          @update:model-value="update({ search: String($event) })"
        />
      </div>
      <label class="text-muted-foreground flex items-center gap-2 text-sm">
        {{ $t("sortBy") }}
        <select
          class="border-input bg-card text-foreground focus-visible:outline-ring h-9 rounded-md border px-2 text-sm focus-visible:outline-2 focus-visible:outline-offset-1"
          :value="state.sort"
          @change="
            update({
              sort: ($event.target as HTMLSelectElement)
                .value as NodeListState['sort'],
            })
          "
        >
          <option v-for="sort in nodeSorts" :key="sort" :value="sort">
            {{ $t(sort === "name" ? "sortName" : "lastHeartbeat") }}
          </option>
        </select>
      </label>
      <Button
        type="button"
        variant="outline"
        class="h-9"
        :aria-expanded="panelOpen"
        :aria-controls="panelId"
        @click="panelOpen = !panelOpen"
      >
        <ListFilter aria-hidden="true" />
        {{ $t("filters") }}
        <span
          v-if="groupFilterCount > 0"
          class="bg-primary text-primary-foreground rounded-full px-1.5 text-xs"
          >{{ groupFilterCount }}</span
        >
      </Button>
      <Button
        v-if="filterCount > 0"
        type="button"
        variant="ghost"
        class="h-9"
        @click="clearFilters"
      >
        <X aria-hidden="true" />
        {{ $t("clearFilters") }}
      </Button>
    </div>
    <div
      v-if="panelOpen"
      :id="panelId"
      class="bg-card border-border grid gap-4 rounded-lg border p-4"
    >
      <fieldset
        v-for="group in nodeFilterGroupNames"
        :key="group"
        class="m-0 min-w-0 border-0 p-0"
      >
        <legend class="mb-2 text-xs font-medium uppercase">
          {{ $t(group) }}
        </legend>
        <div class="flex flex-wrap gap-2">
          <label
            v-for="value in nodeFilterGroups[group]"
            :key="value"
            :class="chip"
          >
            <input
              type="checkbox"
              class="accent-primary size-4"
              :checked="state.filters[group].includes(value)"
              @change="toggleFilter(group, value)"
            />
            {{ $t(value) }}
            <span class="text-muted-foreground tabular-nums">{{
              counts[group][value] ?? 0
            }}</span>
          </label>
        </div>
      </fieldset>
      <fieldset class="m-0 min-w-0 border-0 p-0">
        <legend class="mb-2 text-xs font-medium uppercase">
          {{ $t("optionalColumns") }}
        </legend>
        <div class="flex flex-wrap gap-2">
          <label
            v-for="column in optionalNodeColumns"
            :key="column"
            :class="chip"
          >
            <input
              type="checkbox"
              class="accent-primary size-4"
              :checked="state.columns.includes(column)"
              @change="update({ columns: toggle(state.columns, column) })"
            />
            {{ $t(columnLabels[column]) }}
          </label>
        </div>
      </fieldset>
    </div>
    <p class="text-muted-foreground text-sm" aria-live="polite">
      {{ $t("showingNodes", { shown, total }) }}
    </p>
  </div>
</template>
