<script setup lang="ts">
import {
  Activity,
  Boxes,
  FlaskConical,
  LayoutDashboard,
  ListChecks,
  ScrollText,
  Settings,
  ShieldCheck,
} from "@lucide/vue";
import { useRoute } from "vue-router";

import { developmentRuntime } from "../../shared/routes";

const emit = defineEmits<{ navigate: [] }>();
const route = useRoute();

const links = [
  { to: "/", label: "overview", icon: LayoutDashboard },
  { to: "/nodes", label: "nodes", icon: Boxes },
  { to: "/operations", label: "operations", icon: ListChecks },
  { to: "/approvals", label: "approvals", icon: ShieldCheck },
  { to: "/audit", label: "audit", icon: ScrollText },
  ...(developmentRuntime
    ? [{ to: "/dev", label: "development", icon: FlaskConical }]
    : []),
  { to: "/settings", label: "settings", icon: Settings },
];

// Detail routes are not nested records, so prefix matching keeps their
// section marked as current.
function isCurrent(to: string): boolean {
  if (to === "/") return route.path === "/";
  return route.path === to || route.path.startsWith(`${to}/`);
}
</script>

<template>
  <div class="flex h-full flex-col gap-6 px-3 py-5">
    <div
      class="text-foreground flex items-center gap-2.5 px-3 text-base font-bold"
    >
      <Activity class="text-primary" :size="20" stroke-width="2.4" />
      <span>{{ $t("brand") }}</span>
    </div>
    <nav :aria-label="$t('navigation')" class="flex flex-1">
      <ul class="m-0 flex flex-1 list-none flex-col gap-0.5 p-0">
        <li
          v-for="link in links"
          :key="link.to"
          :class="{ 'mt-auto': link.to === '/settings' }"
        >
          <RouterLink v-slot="{ href, navigate }" :to="link.to" custom>
            <a
              :href="href"
              :aria-current="isCurrent(link.to) ? 'page' : undefined"
              class="text-muted-foreground hover:bg-accent hover:text-accent-foreground focus-visible:outline-ring aria-[current=page]:bg-secondary aria-[current=page]:text-foreground flex min-h-10 items-center gap-3 rounded-md px-3 text-sm font-medium focus-visible:outline-2 focus-visible:outline-offset-2"
              @click="
                navigate($event);
                emit('navigate');
              "
            >
              <component :is="link.icon" :size="18" aria-hidden="true" />
              <span>{{ $t(link.label) }}</span>
            </a>
          </RouterLink>
        </li>
      </ul>
    </nav>
  </div>
</template>
