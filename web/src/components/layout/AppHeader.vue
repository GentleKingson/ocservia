<script setup lang="ts">
import { Menu } from "@lucide/vue";
import type { Workspace } from "@ocservia/api-client";
import { ref, watch } from "vue";
import { useRoute } from "vue-router";

import { Button } from "@/components/ui/button";
import {
  Sheet,
  SheetContent,
  SheetDescription,
  SheetTitle,
  SheetTrigger,
} from "@/components/ui/sheet";

import AppSidebar from "./AppSidebar.vue";

defineProps<{
  workspaces: Workspace[];
  readiness: "loading" | "ready" | "unavailable";
}>();
const selectedWorkspaceId = defineModel<string>("workspaceId", {
  required: true,
});
const emit = defineEmits<{
  changeWorkspace: [workspaceId: string];
  navigated: [];
}>();

const route = useRoute();
const navigationOpen = ref(false);
let closedByNavigation = false;

function closeForNavigation(): void {
  if (!navigationOpen.value) return;
  closedByNavigation = true;
  navigationOpen.value = false;
}
// Covers history navigation as well as link activation inside the sheet.
watch(() => route.fullPath, closeForNavigation);

function restoreFocus(event: Event): void {
  if (!closedByNavigation) return;
  closedByNavigation = false;
  event.preventDefault();
  emit("navigated");
}
</script>

<template>
  <header
    class="bg-card text-muted-foreground border-border flex min-h-14 items-center gap-3 border-b px-4 text-sm md:px-7"
  >
    <Sheet v-model:open="navigationOpen">
      <SheetTrigger as-child>
        <Button
          type="button"
          variant="ghost"
          size="icon"
          class="-ml-2 size-10 md:hidden"
          :aria-label="$t('openNavigation')"
        >
          <Menu class="size-5" aria-hidden="true" />
        </Button>
      </SheetTrigger>
      <SheetContent
        side="left"
        class="bg-card w-72 max-w-[85vw] gap-0 p-0"
        :close-label="$t('closeNavigation')"
        @close-auto-focus="restoreFocus"
      >
        <SheetTitle class="sr-only">{{ $t("navigation") }}</SheetTitle>
        <SheetDescription class="sr-only">
          {{ $t("navigationDescription") }}
        </SheetDescription>
        <AppSidebar @navigate="closeForNavigation" />
      </SheetContent>
    </Sheet>
    <span class="hidden md:inline">{{ $t("platform") }}</span>
    <div
      data-testid="workspace"
      class="flex min-w-0 flex-1 items-center gap-2 md:ml-6"
    >
      <template v-if="workspaces.length > 1">
        <label
          for="workspace-select"
          class="text-xs font-medium uppercase max-sm:sr-only"
        >
          {{ $t("workspace") }}
        </label>
        <select
          id="workspace-select"
          v-model="selectedWorkspaceId"
          class="border-input bg-background text-foreground focus-visible:outline-ring h-9 w-48 min-w-0 rounded-md border px-2 text-sm font-semibold focus-visible:outline-2 focus-visible:outline-offset-1"
          @change="
            emit('changeWorkspace', ($event.target as HTMLSelectElement).value)
          "
        >
          <option
            v-for="workspace in workspaces"
            :key="workspace.id"
            :value="workspace.id"
          >
            {{ workspace.name }}
          </option>
        </select>
      </template>
      <!-- A single authorized Workspace has nothing to choose, so it reads as text. -->
      <template v-else-if="workspaces.length === 1">
        <span class="text-xs font-medium uppercase max-sm:sr-only">
          {{ $t("workspace") }}
        </span>
        <span class="text-foreground truncate font-semibold">
          {{ workspaces[0]?.name }}
        </span>
      </template>
    </div>
    <div
      data-testid="readiness"
      class="ml-auto inline-flex shrink-0 items-center gap-2"
    >
      <span
        class="size-2 rounded-full"
        :class="{
          'bg-muted-foreground ring-muted-foreground/20 ring-3':
            readiness === 'loading',
          'bg-success ring-success/20 ring-3': readiness === 'ready',
          'bg-destructive ring-destructive/20 ring-3':
            readiness === 'unavailable',
        }"
        aria-hidden="true"
      ></span>
      {{ $t(readiness) }}
    </div>
  </header>
</template>
