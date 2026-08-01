<script setup lang="ts">
import { Clock3, Server, Workflow } from "@lucide/vue";
import { useReadinessStore } from "../shared/readiness";

const readiness = useReadinessStore();
</script>

<template>
  <main class="overview">
    <div class="page-heading">
      <div>
        <p>{{ $t("workspace") }}</p>
        <h1>{{ $t("overview") }}</h1>
      </div>
      <span class="health" :class="{ unavailable: !readiness.isReady }"
        ><i></i
        >{{ $t(readiness.isReady ? "allSystems" : "systemsUnavailable") }}</span
      >
    </div>
    <section class="metrics" aria-label="Platform status">
      <article>
        <div>
          <span>{{ $t("controlPlane") }}</span
          ><strong>{{
            $t(readiness.isReady ? "ready" : "unavailable")
          }}</strong>
        </div>
        <Server :size="20" />
      </article>
      <article>
        <div>
          <span>{{ $t("activeNodes") }}</span
          ><strong>0</strong>
        </div>
        <Workflow :size="20" />
      </article>
      <article>
        <div>
          <span>{{ $t("pendingOperations") }}</span
          ><strong>0</strong>
        </div>
        <Clock3 :size="20" />
      </article>
    </section>
    <section class="activity-panel">
      <header>
        <h2>{{ $t("recentActivity") }}</h2>
      </header>
      <div class="empty-state">
        <Clock3 :size="24" /><span>{{ $t("noActivity") }}</span>
      </div>
    </section>
  </main>
</template>
