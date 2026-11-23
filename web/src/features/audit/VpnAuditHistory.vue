<script setup lang="ts">
import { onBeforeUnmount, ref, shallowRef } from "vue";
import { useI18n } from "vue-i18n";
import type { ListVpnAuditRecordsRequest } from "@/api/events";
import type { VpnAuditRecord } from "@/api/models";
import { listVpnAuditRecords } from "@/api/events";
import { formatTimestamp } from "@/shared/timestamp";
import { createReadRequest } from "@/shared/read-request";
import SectionCard from "@/components/common/SectionCard.vue";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";

const { t } = useI18n();
const kind = ref<"sessions" | "network" | "observations">("sessions");
const destination = ref("");
const displayed = ref(kind.value);
const username = ref("");
const nodeId = ref("");
const since = ref("");
const until = ref("");
const items = shallowRef<VpnAuditRecord[]>();
const reading = ref(false);
const nextCursor = ref<string>();
let submitted: ListVpnAuditRecordsRequest = {};
const error = ref("");
const reads = createReadRequest();
onBeforeUnmount(reads.cancel);

async function search(more = false) {
  const read = reads.begin();
  reading.value = true;
  if (!more) {
    items.value = undefined;
    nextCursor.value = undefined;
  }
  error.value = "";
  try {
    if (!more)
      submitted = {
        ...(username.value.trim() ? { username: username.value.trim() } : {}),
        ...(nodeId.value.trim() ? { node_id: nodeId.value.trim() } : {}),
        ...(since.value ? { since: new Date(since.value).toISOString() } : {}),
        ...(until.value ? { until: new Date(until.value).toISOString() } : {}),
        kind: kind.value,
        ...(kind.value === "network" && destination.value.trim()
          ? { destination_ip: destination.value.trim() }
          : {}),
        page_size: 200,
      };
    const page = await listVpnAuditRecords(
      {
        ...submitted,
        ...(more && nextCursor.value ? { cursor: nextCursor.value } : {}),
      },
      read.signal,
    );
    if (read.current()) {
      items.value = more ? [...(items.value ?? []), ...page.items] : page.items;
      displayed.value = submitted.kind ?? "sessions";
      nextCursor.value = page.nextCursor;
    }
  } catch {
    if (read.current()) error.value = t("vpnAuditLoadFailed");
  } finally {
    if (read.finish()) reading.value = false;
  }
}

function duration(row: VpnAuditRecord) {
  const start = row.flowStartedAt ?? row.connectedAt;
  return start
    ? Math.max(
        0,
        Math.floor((Date.parse(row.lastSeenAt) - Date.parse(start)) / 1000),
      )
    : "—";
}

function download() {
  if (!items.value?.length) return;
  const href = URL.createObjectURL(
    new Blob([JSON.stringify(items.value, null, 2)], {
      type: "application/json",
    }),
  );
  const link = document.createElement("a");
  link.href = href;
  link.download = `vpn-${displayed.value}.json`;
  link.click();
  setTimeout(() => {
    URL.revokeObjectURL(href);
  }, 1000);
}
</script>
<template>
  <SectionCard :title="t('vpnAuditHistory')">
    <form
      class="flex flex-wrap items-end gap-3"
      @submit.prevent="search(false)"
    >
      <label
        >{{ t("vpnAuditSource")
        }}<select v-model="kind" class="block rounded-md border p-2">
          <option value="sessions">{{ t("vpnAuditSessions") }}</option>
          <option value="observations">{{ t("vpnAuditSnapshots") }}</option>
          <option value="network">{{ t("vpnAuditNetwork") }}</option>
        </select></label
      >
      <label v-if="kind === 'network'"
        >{{ t("vpnAuditDestinationIp") }}<Input v-model="destination"
      /></label>
      <label
        >{{ t("vpnAuditUsername") }}<Input v-model="username" maxlength="128"
      /></label>
      <label>{{ t("vpnAuditNodeId") }}<Input v-model="nodeId" /></label>
      <label
        >{{ t("vpnAuditFrom") }}<Input v-model="since" type="datetime-local"
      /></label>
      <label
        >{{ t("vpnAuditBefore") }}<Input v-model="until" type="datetime-local"
      /></label>
      <Button type="submit" :disabled="reading">{{
        t(reading ? "vpnAuditSearching" : "vpnAuditSearch")
      }}</Button>
      <Button
        type="button"
        variant="outline"
        :disabled="!items?.length"
        @click="download"
        >{{ t("vpnAuditExport") }}</Button
      >
    </form>
    <p v-if="error" role="alert">{{ error }}</p>
    <p v-if="items && !items.length">{{ t("vpnAuditEmpty") }}</p>
    <div v-if="items?.length" class="overflow-x-auto">
      <table class="w-full text-left text-sm">
        <caption class="text-left">
          {{
            t("vpnAuditCount", { count: items.length })
          }}
        </caption>
        <thead>
          <tr>
            <th>{{ t("vpnAuditEvent") }}</th>
            <th>{{ t("vpnAuditUser") }}</th>
            <th>{{ t("vpnAuditNodeSession") }}</th>
            <th>{{ t("vpnAuditSourceIp") }}</th>
            <th>{{ t("vpnAuditVpnIp") }}</th>
            <th>{{ t("vpnAuditConnected") }}</th>
            <th>{{ t("vpnAuditObserved") }}</th>
            <th v-if="displayed === 'observations'">
              {{ t("vpnAuditAbsent") }}
            </th>
            <th v-if="displayed === 'network'">
              {{ t("vpnAuditDestination") }}
            </th>
            <th v-if="displayed === 'network'">{{ t("vpnAuditFlowStart") }}</th>
            <th>{{ t("vpnAuditDuration") }}</th>
            <th>{{ t("vpnAuditRx") }}</th>
            <th>{{ t("vpnAuditTx") }}</th>
          </tr>
        </thead>
        <tbody>
          <tr
            v-for="row in items"
            :key="
              row.eventId ??
              [row.nodeId, row.sessionId, row.connectedAt].join('/')
            "
          >
            <td>{{ row.eventType }}</td>
            <td>{{ row.username }}</td>
            <td>{{ row.nodeId }} / {{ row.sessionId }}</td>
            <td>{{ row.clientIp }}</td>
            <td>{{ row.vpnIp || "—" }}</td>
            <td>{{ formatTimestamp(row.connectedAt) }}</td>
            <td>{{ formatTimestamp(row.lastSeenAt) }}</td>
            <td v-if="displayed === 'observations'">
              {{ formatTimestamp(row.absentAt) }}
            </td>
            <td v-if="displayed === 'network'">
              {{ row.destinationIp }} {{ row.destinationPort ?? "" }} /
              {{ row.protocol }}
            </td>
            <td v-if="displayed === 'network'">
              {{ formatTimestamp(row.flowStartedAt) }}
            </td>
            <td>{{ duration(row) }}</td>
            <td>{{ row.bytesIn ?? "—" }}</td>
            <td>{{ row.bytesOut ?? "—" }}</td>
          </tr>
        </tbody>
      </table>
    </div>
    <Button
      v-if="nextCursor"
      variant="outline"
      :disabled="reading"
      @click="search(true)"
      >{{ t("vpnAuditOlder") }}</Button
    >
  </SectionCard>
</template>
