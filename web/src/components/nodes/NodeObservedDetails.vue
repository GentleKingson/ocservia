<script setup lang="ts">
import type { NodeObservedState } from "@/api/models";
import { computed } from "vue";

import CopyButton from "@/components/common/CopyButton.vue";
import { Badge } from "@/components/ui/badge";
import { agentVersionLabel } from "@/shared/agent-version";
import { formatTimestamp } from "@/shared/timestamp";

const props = defineProps<{ node: NodeObservedState }>();

const pathMode = computed(() =>
  props.node.path?.mode === "relay" || props.node.path?.mode === "direct"
    ? props.node.path.mode
    : "unknown",
);
const identities = computed(() =>
  (
    [
      ["bootId", props.node.boot_id],
      ["agentInstanceId", props.node.agent_instance_id],
    ] as const
  ).map(([label, value]) => ({ label, value })),
);

const row =
  "border-border grid gap-1 border-b py-2.5 last:border-b-0 @min-[32rem]:grid-cols-[12rem_minmax(0,1fr)] @min-[32rem]:gap-4";
const term = "text-muted-foreground text-sm";
const detail = "m-0 min-w-0 text-sm break-words";
</script>

<template>
  <section
    class="bg-card border-border @container mb-6 min-w-0 scroll-mt-4 rounded-xl border p-4 shadow-xs md:p-5"
    aria-labelledby="node-overview-title"
  >
    <h2 id="node-overview-title" class="mb-3 text-base font-semibold">
      {{ $t("nodeOverview") }}
    </h2>
    <slot />
    <div class="grid min-w-0 grid-cols-1 gap-x-8 @min-[56rem]:grid-cols-2">
      <dl class="m-0">
        <div :class="row">
          <dt :class="term">{{ $t("path") }}</dt>
          <dd :class="detail">
            {{ $t(pathMode) }}
            <template v-if="node.path">
              · {{ Math.round(node.path.rtt_ms) }} {{ $t("milliseconds") }}
            </template>
          </dd>
        </div>
        <div :class="row">
          <dt :class="term">{{ $t("ocserv") }}</dt>
          <dd :class="detail">
            {{ node.ocserv_version ?? $t("notAvailable") }}
          </dd>
        </div>
        <div :class="row">
          <dt :class="term">{{ $t("agent") }}</dt>
          <dd :class="detail" class="flex flex-wrap items-center gap-2">
            {{ node.agent_version ?? $t("notAvailable") }}
            <Badge variant="outline" data-testid="agent-version-state">{{
              $t(agentVersionLabel(node))
            }}</Badge>
          </dd>
        </div>
        <div :class="row">
          <dt :class="term">{{ $t("versionState") }}</dt>
          <dd :class="detail">{{ $t(agentVersionLabel(node)) }}</dd>
        </div>
        <div :class="row">
          <dt :class="term">{{ $t("recommendedAgentVersion") }}</dt>
          <dd :class="detail">
            {{
              node.recommended_agent_version ||
              $t("recommendationNotConfigured")
            }}
          </dd>
        </div>
        <div :class="row">
          <dt :class="term">{{ $t("osRelease") }}</dt>
          <dd :class="detail">{{ node.os_release ?? $t("notAvailable") }}</dd>
        </div>
        <div :class="row">
          <dt :class="term">{{ $t("architecture") }}</dt>
          <dd :class="detail">{{ node.architecture || $t("unknown") }}</dd>
        </div>
      </dl>
      <dl class="m-0">
        <div :class="row">
          <dt :class="term">{{ $t("configRevision") }}</dt>
          <dd :class="detail">
            {{ node.config_revision ?? $t("notAvailable") }}
          </dd>
        </div>
        <div :class="row">
          <dt :class="term">{{ $t("observedAt") }}</dt>
          <dd :class="detail">
            {{
              node.observed_at
                ? formatTimestamp(node.observed_at)
                : $t("notObserved")
            }}
          </dd>
        </div>
        <div v-for="identity in identities" :key="identity.label" :class="row">
          <dt :class="term">{{ $t(identity.label) }}</dt>
          <dd :class="detail" class="flex items-start gap-1">
            <template v-if="identity.value">
              <code class="min-w-0 py-1 text-xs break-all">{{
                identity.value
              }}</code>
              <CopyButton :value="identity.value" :label="$t(identity.label)" />
            </template>
            <template v-else>{{ $t("notAvailable") }}</template>
          </dd>
        </div>
      </dl>
    </div>
  </section>
</template>
