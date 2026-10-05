<script setup lang="ts">
import { Activity } from "@lucide/vue";
import { computed } from "vue";
import { useRoute } from "vue-router";

import { isCurrentSection, navigationLinks } from "./navigation";

const emit = defineEmits<{ navigate: [] }>();
const route = useRoute();

const mainLinks = computed(() =>
  navigationLinks.filter((link) => link.to !== "/settings"),
);
const footerLinks = computed(() =>
  navigationLinks.filter((link) => link.to === "/settings"),
);
</script>

<template>
  <div class="text-sidebar-foreground flex h-full flex-col gap-2 p-2">
    <div class="flex h-12 items-center gap-2 px-2 text-sm font-semibold">
      <span
        class="bg-primary text-primary-foreground flex size-8 items-center justify-center rounded-lg"
      >
        <Activity class="size-4" stroke-width="2.4" aria-hidden="true" />
      </span>
      <span class="truncate text-base">{{ $t("brand") }}</span>
    </div>
    <nav :aria-label="$t('navigation')" class="flex flex-1 flex-col">
      <p
        class="text-sidebar-foreground/70 m-0 flex h-8 items-center px-2 text-xs font-medium"
      >
        {{ $t("platform") }}
      </p>
      <ul
        v-for="(group, index) in [mainLinks, footerLinks]"
        :key="index"
        class="m-0 flex list-none flex-col gap-1 p-0"
        :class="{ 'mt-auto': index === 1 }"
      >
        <li v-for="link in group" :key="link.to">
          <RouterLink v-slot="{ href, navigate }" :to="link.to" custom>
            <a
              :href="href"
              :aria-current="
                isCurrentSection(link.to, route.path) ? 'page' : undefined
              "
              class="hover:bg-sidebar-accent hover:text-sidebar-accent-foreground focus-visible:outline-ring aria-[current=page]:bg-sidebar-accent aria-[current=page]:text-sidebar-accent-foreground flex h-9 items-center gap-2 rounded-md px-2 text-sm focus-visible:outline-2 focus-visible:outline-offset-1 aria-[current=page]:font-medium md:h-8"
              @click="
                navigate($event);
                emit('navigate');
              "
            >
              <component :is="link.icon" class="size-4" aria-hidden="true" />
              <span>{{ $t(link.label) }}</span>
            </a>
          </RouterLink>
        </li>
      </ul>
    </nav>
  </div>
</template>
