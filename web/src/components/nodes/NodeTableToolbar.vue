<script setup lang="ts">
import { Columns3, ListFilter, Search, X } from "@lucide/vue";
import { computed } from "vue";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import {
  DropdownMenu,
  DropdownMenuCheckboxItem,
  DropdownMenuContent,
  DropdownMenuLabel,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu";
import { Input } from "@/components/ui/input";
import { NativeSelect } from "@/components/ui/native-select";
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

const filterCount = computed(() => activeFilterCount(props.state));
const activeFilters = computed(() =>
  nodeFilterGroupNames.flatMap((group) =>
    props.state.filters[group].map((value) => ({ group, value })),
  ),
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

// Menus stay open while several values are toggled.
function keepOpen(event: Event): void {
  event.preventDefault();
}
</script>

<template>
  <div class="mb-3 grid gap-2">
    <div class="flex flex-wrap items-center gap-2">
      <div class="relative min-w-0 flex-1 basis-64">
        <Search
          class="text-muted-foreground pointer-events-none absolute top-1/2 left-3 size-4 -translate-y-1/2"
          aria-hidden="true"
        />
        <Input
          type="search"
          class="h-9 pl-9"
          :aria-label="$t('searchNodes')"
          :placeholder="$t('searchNodesPlaceholder')"
          :model-value="state.search"
          @update:model-value="update({ search: String($event) })"
        />
      </div>
      <DropdownMenu>
        <DropdownMenuTrigger as-child>
          <Button type="button" variant="outline" class="h-9">
            <ListFilter aria-hidden="true" />
            {{ $t("filters") }}
          </Button>
        </DropdownMenuTrigger>
        <DropdownMenuContent align="end" class="w-56">
          <template v-for="(group, index) in nodeFilterGroupNames" :key="group">
            <DropdownMenuSeparator v-if="index > 0" />
            <DropdownMenuLabel
              class="text-muted-foreground text-xs font-medium uppercase"
              >{{ $t(group) }}</DropdownMenuLabel
            >
            <DropdownMenuCheckboxItem
              v-for="value in nodeFilterGroups[group]"
              :key="value"
              :model-value="state.filters[group].includes(value)"
              @select="keepOpen"
              @update:model-value="toggleFilter(group, value)"
            >
              {{ $t(value) }}
              <span class="text-muted-foreground ml-auto tabular-nums">{{
                counts[group][value] ?? 0
              }}</span>
            </DropdownMenuCheckboxItem>
          </template>
        </DropdownMenuContent>
      </DropdownMenu>
      <DropdownMenu>
        <DropdownMenuTrigger as-child>
          <Button type="button" variant="outline" class="h-9">
            <Columns3 aria-hidden="true" />
            {{ $t("optionalColumns") }}
          </Button>
        </DropdownMenuTrigger>
        <DropdownMenuContent align="end" class="w-44">
          <DropdownMenuCheckboxItem
            v-for="column in optionalNodeColumns"
            :key="column"
            :model-value="state.columns.includes(column)"
            @select="keepOpen"
            @update:model-value="
              update({ columns: toggle(state.columns, column) })
            "
          >
            {{ $t(columnLabels[column]) }}
          </DropdownMenuCheckboxItem>
        </DropdownMenuContent>
      </DropdownMenu>
      <label class="text-muted-foreground flex items-center gap-2 text-sm">
        <span class="sr-only sm:not-sr-only">{{ $t("sortBy") }}</span>
        <NativeSelect
          class="h-9"
          :model-value="state.sort"
          @update:model-value="
            update({ sort: $event as NodeListState['sort'] })
          "
        >
          <option v-for="sort in nodeSorts" :key="sort" :value="sort">
            {{ $t(sort === "name" ? "sortName" : "lastHeartbeat") }}
          </option>
        </NativeSelect>
      </label>
    </div>
    <div class="flex min-h-7 flex-wrap items-center gap-2">
      <Badge
        v-for="filter in activeFilters"
        :key="`${filter.group}:${filter.value}`"
        variant="secondary"
        class="h-7 gap-1 pr-1 pl-2.5 text-sm font-normal"
      >
        <span class="text-muted-foreground">{{ $t(filter.group) }}:</span>
        {{ $t(filter.value) }}
        <Button
          type="button"
          variant="ghost"
          size="icon"
          class="size-5 rounded-full"
          :aria-label="
            $t('removeFilter', {
              filter: `${$t(filter.group)}: ${$t(filter.value)}`,
            })
          "
          @click="toggleFilter(filter.group, filter.value)"
        >
          <X class="size-3.5" aria-hidden="true" />
        </Button>
      </Badge>
      <Button
        v-if="filterCount > 0"
        type="button"
        variant="ghost"
        size="sm"
        class="h-7"
        @click="clearFilters"
      >
        {{ $t("clearFilters") }}
      </Button>
      <p class="text-muted-foreground m-0 ml-auto text-sm" aria-live="polite">
        {{ $t("showingNodes", { shown, total }) }}
      </p>
    </div>
  </div>
</template>
