<script setup lang="ts">
// Development-only coexistence sample for components/ui; not product copy.
import { Search, Trash2 } from "@lucide/vue";
import { ref } from "vue";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import {
  Table,
  TableBody,
  TableCaption,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";

const query = ref("");
const rows = [
  { id: "019fc0a4-6d92", name: "edge-tokyo-01", trust: "active" },
  { id: "019fc0a4-6d93", name: "edge-frankfurt-02", trust: "pending" },
  { id: "019fc0a4-6d94", name: "edge-retired-03", trust: "revoked" },
] as const;
</script>

<template>
  <section
    class="bg-card text-card-foreground border-border mt-6 grid gap-6 rounded-lg border p-6"
    data-testid="ui-preview"
    aria-labelledby="ui-preview-title"
  >
    <h2 id="ui-preview-title" class="text-base font-semibold">
      components/ui preview
    </h2>
    <div class="flex flex-wrap items-center gap-2">
      <Button type="button">Default</Button>
      <Button type="button" variant="outline">Outline</Button>
      <Button type="button" variant="secondary">Secondary</Button>
      <Button type="button" variant="ghost">Ghost</Button>
      <Button type="button" variant="destructive"><Trash2 />Destructive</Button>
      <Button type="button" variant="link">Link</Button>
      <Button type="button" disabled>Disabled</Button>
      <Button type="button" variant="outline" size="icon" aria-label="Search">
        <Search />
      </Button>
    </div>
    <div class="grid max-w-sm gap-2">
      <label for="ui-preview-query" class="text-sm font-medium">Search</label>
      <Input
        id="ui-preview-query"
        v-model="query"
        placeholder="Name or ID"
        data-testid="ui-preview-input"
      />
    </div>
    <div class="flex flex-wrap gap-2">
      <Badge>Default</Badge>
      <Badge variant="secondary">Secondary</Badge>
      <Badge variant="destructive">Destructive</Badge>
      <Badge variant="outline">Outline</Badge>
    </div>
    <Table>
      <TableCaption>Static sample rows.</TableCaption>
      <TableHeader>
        <TableRow>
          <TableHead>Name</TableHead>
          <TableHead>ID</TableHead>
          <TableHead>Trust</TableHead>
        </TableRow>
      </TableHeader>
      <TableBody>
        <TableRow v-for="row in rows" :key="row.id">
          <TableCell class="font-medium">{{ row.name }}</TableCell>
          <TableCell class="font-mono text-xs">{{ row.id }}</TableCell>
          <TableCell>
            <Badge variant="outline">{{ row.trust }}</Badge>
          </TableCell>
        </TableRow>
      </TableBody>
    </Table>
  </section>
</template>
