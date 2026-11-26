<script setup lang="ts">
import {
  KeyRound,
  ListPlus,
  SlidersHorizontal,
  UserCheck,
  UserPlus,
  UserX,
} from "@lucide/vue";
import type { UserGroupResourceState } from "@/api/models";
import { ref, type Component } from "vue";

import NodeSectionState from "@/components/nodes/NodeSectionState.vue";
import type { DetailReadState } from "@/shared/fleet";
import SectionCard from "@/components/common/SectionCard.vue";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import {
  recoveryDialogKind,
  resourceStatusKey,
  type DesiredDialogKind,
} from "@/shared/desired-recovery";

interface ActionState {
  allowed: boolean;
  explanation: string;
}

// Presentation only: the page owns the dialogs, permissions and mutations.
defineProps<{
  users: UserGroupResourceState[];
  groups: UserGroupResourceState[];
  busy: boolean;
  readState: DetailReadState;
  userAction: ActionState;
  groupAction: ActionState;
}>();
defineEmits<{
  desired: [kind: DesiredDialogKind, name?: string, version?: number];
  policy: [username: string];
}>();

const stateTab = ref<"users" | "groups">("users");

function convergenceTone(key: string): string {
  if (key === "convergence_converged") return "text-success";
  return key === "convergence_drifted"
    ? "text-destructive"
    : "text-muted-foreground";
}

interface RowAction {
  kind: DesiredDialogKind;
  titleKey: string;
  icon: Component;
  destructive?: boolean;
}

const recoveryActions: Record<string, RowAction> = {
  create: { kind: "create", titleKey: "retryCreateUser", icon: UserPlus },
  rotate: { kind: "rotate", titleKey: "retryRotatePassword", icon: KeyRound },
  disable: {
    kind: "disable",
    titleKey: "retryDisableUser",
    icon: UserX,
    destructive: true,
  },
  enable: { kind: "enable", titleKey: "retryEnableUser", icon: UserCheck },
};

function userRowActions(item: UserGroupResourceState): RowAction[] {
  const actions: RowAction[] = [];
  const recovery = recoveryDialogKind(item);
  if (recovery && recovery in recoveryActions) {
    const action = recoveryActions[recovery];
    if (action) actions.push(action);
  }
  if (!item.recovery_required) {
    actions.push(
      { kind: "rotate", titleKey: "rotatePassword", icon: KeyRound },
      item.desired_enabled === false
        ? { kind: "enable", titleKey: "enableUser", icon: UserCheck }
        : {
            kind: "disable",
            titleKey: "disableUser",
            icon: UserX,
            destructive: true,
          },
    );
  }
  return actions;
}
</script>

<template>
  <SectionCard id="node-users-groups" :title="$t('usersAndGroups')">
    <template #actions>
      <div
        class="bg-muted inline-flex gap-0.5 rounded-md p-0.5"
        role="group"
        :aria-label="$t('usersAndGroups')"
      >
        <Button
          type="button"
          size="sm"
          :variant="stateTab === 'users' ? 'outline' : 'ghost'"
          :aria-pressed="stateTab === 'users'"
          @click="stateTab = 'users'"
        >
          {{ $t("users") }}
        </Button>
        <Button
          type="button"
          size="sm"
          :variant="stateTab === 'groups' ? 'outline' : 'ghost'"
          :aria-pressed="stateTab === 'groups'"
          @click="stateTab = 'groups'"
        >
          {{ $t("groups") }}
        </Button>
      </div>
      <Button
        v-if="stateTab === 'users'"
        type="button"
        variant="outline"
        size="icon-sm"
        :disabled="busy || !userAction.allowed"
        :title="userAction.explanation || $t('createUser')"
        :aria-label="$t('createUser')"
        @click="$emit('desired', 'create')"
      >
        <UserPlus aria-hidden="true" />
      </Button>
      <Button
        v-else
        type="button"
        variant="outline"
        size="icon-sm"
        :disabled="busy || !groupAction.allowed"
        :title="groupAction.explanation || $t('applyGroup')"
        :aria-label="$t('applyGroup')"
        @click="$emit('desired', 'group')"
      >
        <ListPlus aria-hidden="true" />
      </Button>
    </template>
    <p
      v-if="(stateTab === 'users' ? userAction : groupAction).explanation"
      class="text-muted-foreground m-0 text-xs"
    >
      {{ (stateTab === "users" ? userAction : groupAction).explanation }}
    </p>
    <NodeSectionState :state="readState" />
    <template
      v-if="
        (readState === 'ready' || readState === 'stale') && stateTab === 'users'
      "
    >
      <ul v-if="users.length" class="m-0 grid list-none p-0">
        <li
          v-for="item in users"
          :key="item.name"
          class="border-border flex flex-wrap items-center justify-between gap-2 border-b py-1.5 last:border-b-0"
        >
          <span class="flex min-w-0 flex-wrap items-center gap-2 text-sm">
            <strong class="break-words">{{ item.name }}</strong>
            <Badge
              variant="outline"
              :class="convergenceTone(resourceStatusKey(item))"
              >{{ $t(resourceStatusKey(item)) }}</Badge
            >
            <Badge
              v-if="item.recovery_required && !item.recovery_mutation_kind"
              variant="outline"
              class="text-destructive"
              >{{ $t("manualReconciliationRequired") }}</Badge
            >
          </span>
          <span class="flex shrink-0 gap-1">
            <Button
              v-if="item.desired_version"
              type="button"
              variant="ghost"
              size="icon-sm"
              :disabled="busy || !item.desired_version"
              :title="$t('quotaAndExpiry')"
              :aria-label="$t('quotaAndExpiry')"
              @click="$emit('policy', item.name)"
            >
              <SlidersHorizontal aria-hidden="true" />
            </Button>
            <Button
              v-for="action in userRowActions(item)"
              :key="`${action.kind}:${action.titleKey}`"
              type="button"
              variant="ghost"
              size="icon-sm"
              :class="
                action.destructive
                  ? 'text-destructive hover:text-destructive'
                  : undefined
              "
              :disabled="busy || !userAction.allowed || !item.desired_version"
              :title="$t(action.titleKey)"
              :aria-label="$t(action.titleKey)"
              @click="
                $emit(
                  'desired',
                  action.kind,
                  item.name,
                  item.desired_version ?? 0,
                )
              "
            >
              <component :is="action.icon" aria-hidden="true" />
            </Button>
          </span>
        </li>
      </ul>
      <p
        v-else-if="readState === 'ready'"
        class="text-muted-foreground m-0 text-sm"
      >
        {{ $t("noUsers") }}
      </p>
    </template>
    <template v-else-if="readState === 'ready' || readState === 'stale'">
      <ul v-if="groups.length" class="m-0 grid list-none p-0">
        <li
          v-for="item in groups"
          :key="item.name"
          class="border-border flex flex-wrap items-center justify-between gap-2 border-b py-1.5 last:border-b-0"
        >
          <span class="flex min-w-0 flex-wrap items-center gap-2 text-sm">
            <strong class="break-words">{{ item.name }}</strong>
            <Badge
              variant="outline"
              :class="convergenceTone(resourceStatusKey(item))"
              >{{ $t(resourceStatusKey(item)) }}</Badge
            >
            <Badge
              v-if="item.recovery_required && !item.recovery_mutation_kind"
              variant="outline"
              class="text-destructive"
              >{{ $t("manualReconciliationRequired") }}</Badge
            >
          </span>
          <Button
            v-if="
              recoveryDialogKind(item) === 'group' || !item.recovery_required
            "
            type="button"
            variant="ghost"
            size="icon-sm"
            :disabled="busy || !groupAction.allowed"
            :title="
              $t(
                recoveryDialogKind(item) === 'group'
                  ? 'retryApplyGroup'
                  : 'applyGroup',
              )
            "
            :aria-label="
              $t(
                recoveryDialogKind(item) === 'group'
                  ? 'retryApplyGroup'
                  : 'applyGroup',
              )
            "
            @click="
              $emit('desired', 'group', item.name, item.desired_version ?? 0)
            "
          >
            <ListPlus aria-hidden="true" />
          </Button>
        </li>
      </ul>
      <p
        v-else-if="readState === 'ready'"
        class="text-muted-foreground m-0 text-sm"
      >
        {{ $t("noGroups") }}
      </p>
    </template>
  </SectionCard>
</template>
