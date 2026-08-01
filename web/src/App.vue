<script setup lang="ts">
import {
  Activity,
  Boxes,
  LayoutDashboard,
  ListChecks,
  ScrollText,
  Settings,
} from "@lucide/vue";

const links = [
  { to: "/", label: "overview", icon: LayoutDashboard },
  { to: "/nodes", label: "nodes", icon: Boxes },
  { to: "/operations", label: "operations", icon: ListChecks },
  { to: "/audit", label: "audit", icon: ScrollText },
];
</script>

<template>
  <div class="app-shell">
    <aside class="sidebar">
      <div class="brand">
        <Activity :size="22" stroke-width="2.4" /><span>{{ $t("brand") }}</span>
      </div>
      <div class="workspace-switcher">
        <span>{{ $t("workspace") }}</span
        ><strong>Default</strong>
      </div>
      <nav :aria-label="$t('navigation')">
        <RouterLink
          v-for="link in links"
          :key="link.to"
          :to="link.to"
          :aria-label="$t(link.label)"
          :title="$t(link.label)"
        >
          <component :is="link.icon" :size="18" /><span>{{
            $t(link.label)
          }}</span>
        </RouterLink>
      </nav>
      <RouterLink
        class="settings-link"
        to="/settings"
        :aria-label="$t('settings')"
        :title="$t('settings')"
        ><Settings :size="18" /><span>{{ $t("settings") }}</span></RouterLink
      >
    </aside>
    <section class="content-shell">
      <header class="topbar">
        <span>{{ $t("platform") }}</span>
        <div class="status"><i></i>{{ $t("ready") }}</div>
      </header>
      <RouterView />
    </section>
  </div>
</template>
