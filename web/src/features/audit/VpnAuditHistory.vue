<script setup lang="ts">
import { onBeforeUnmount, ref, shallowRef } from "vue";
import type {
  ListVpnAuditRecordsRequest,
  VpnAuditRecord,
} from "@ocservia/api-client";
import { listVpnAuditRecords } from "@/api/events";
import {
  getWorkspace,
  workspaceContext,
  type WorkspaceContext,
} from "@/api/workspace";
import { createReadRequest } from "@/shared/read-request";
import SectionCard from "@/components/common/SectionCard.vue";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";

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
  let context: WorkspaceContext | undefined;
  reading.value = true;
  if (!more) {
    items.value = undefined;
    nextCursor.value = undefined;
  }
  error.value = "";
  try {
    const workspace = await getWorkspace();
    if (!read.current()) return;
    context = workspaceContext();
    if (!more)
      submitted = {
        xWorkspaceID: workspace.id,
        ...(username.value.trim() ? { username: username.value.trim() } : {}),
        ...(nodeId.value.trim() ? { nodeId: nodeId.value.trim() } : {}),
        ...(since.value ? { since: new Date(since.value) } : {}),
        ...(until.value ? { until: new Date(until.value) } : {}),
        kind: kind.value,
        ...(kind.value === "network" && destination.value.trim()
          ? { destinationIp: destination.value.trim() }
          : {}),
        pageSize: 200,
      };
    if (submitted.xWorkspaceID !== workspace.id) return;
    const page = await listVpnAuditRecords(
      {
        ...submitted,
        ...(more && nextCursor.value ? { cursor: nextCursor.value } : {}),
      },
      read.signal,
    );
    if (read.current(context)) {
      items.value = more ? [...(items.value ?? []), ...page.items] : page.items;
      displayed.value = submitted.kind ?? "sessions";
      nextCursor.value = page.nextCursor;
    }
  } catch {
    if (read.current(context))
      error.value =
        "Session history could not be loaded. Check the filters and try again.";
  } finally {
    if (read.finish()) reading.value = false;
  }
}

function duration(row: VpnAuditRecord) {
  const start = row.flowStartedAt ?? row.connectedAt;
  return start
    ? Math.max(
        0,
        Math.floor((row.lastSeenAt.getTime() - start.getTime()) / 1000),
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
  <SectionCard title="VPN audit history">
    <form
      class="flex flex-wrap items-end gap-3"
      @submit.prevent="search(false)"
    >
      <label
        >Audit source<select v-model="kind" class="block rounded-md border p-2">
          <option value="sessions">Session events</option>
          <option value="observations">Session snapshots</option>
          <option value="network">Network connections</option>
        </select></label
      >
      <label v-if="kind === 'network'"
        >Destination IP<Input v-model="destination"
      /></label>
      <label>Username<Input v-model="username" maxlength="128" /></label>
      <label>Node ID<Input v-model="nodeId" /></label>
      <label>From<Input v-model="since" type="datetime-local" /></label>
      <label>Before<Input v-model="until" type="datetime-local" /></label>
      <Button type="submit" :disabled="reading">{{
        reading ? "Searching…" : "Search history"
      }}</Button>
      <Button
        type="button"
        variant="outline"
        :disabled="!items?.length"
        @click="download"
        >Export results</Button
      >
    </form>
    <p v-if="error" role="alert">{{ error }}</p>
    <p v-if="items && !items.length">No matching records.</p>
    <div v-if="items?.length" class="overflow-x-auto">
      <table class="w-full text-left text-sm">
        <caption class="text-left">
          Latest
          {{
            items.length
          }}
          matching records loaded
        </caption>
        <thead>
          <tr>
            <th>Event</th>
            <th>User</th>
            <th>Node / session</th>
            <th>Source IP</th>
            <th>VPN IP</th>
            <th>Connected (UTC)</th>
            <th>Observed (UTC)</th>
            <th v-if="displayed === 'observations'">First absent (UTC)</th>
            <th v-if="displayed === 'network'">Destination / protocol</th>
            <th v-if="displayed === 'network'">Flow start (UTC)</th>
            <th>Duration (s)</th>
            <th>RX bytes</th>
            <th>TX bytes</th>
          </tr>
        </thead>
        <tbody>
          <tr
            v-for="row in items"
            :key="
              row.eventId ??
              [row.nodeId, row.sessionId, row.connectedAt?.toISOString()].join(
                '/',
              )
            "
          >
            <td>{{ row.eventType }}</td>
            <td>{{ row.username }}</td>
            <td>{{ row.nodeId }} / {{ row.sessionId }}</td>
            <td>{{ row.clientIp }}</td>
            <td>{{ row.vpnIp || "—" }}</td>
            <td>{{ row.connectedAt?.toISOString() || "—" }}</td>
            <td>{{ row.lastSeenAt.toISOString() }}</td>
            <td v-if="displayed === 'observations'">
              {{ row.absentAt?.toISOString() || "—" }}
            </td>
            <td v-if="displayed === 'network'">
              {{ row.destinationIp }} {{ row.destinationPort ?? "" }} /
              {{ row.protocol }}
            </td>
            <td v-if="displayed === 'network'">
              {{ row.flowStartedAt?.toISOString() }}
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
      >Load older records</Button
    >
  </SectionCard>
</template>
