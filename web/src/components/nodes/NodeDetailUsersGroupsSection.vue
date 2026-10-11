<script setup lang="ts">
import {
  KeyRound,
  ListPlus,
  SlidersHorizontal,
  UserCheck,
  UserPlus,
  UserX,
} from "@lucide/vue";
import type { UserGroupResourceState } from "@ocservia/api-client";
import { ref } from "vue";

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
      v-if="!(stateTab === 'users' ? userAction : groupAction).allowed"
      class="text-muted-foreground m-0 text-xs"
    >
      {{ (stateTab === "users" ? userAction : groupAction).explanation }}
    </p>
    <template v-if="stateTab === 'users'">
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
              v-if="item.recoveryRequired && !item.recoveryMutationKind"
              variant="outline"
              class="text-destructive"
              >{{ $t("manualReconciliationRequired") }}</Badge
            >
          </span>
          <span class="flex shrink-0 gap-1">
            <Button
              v-if="item.desiredVersion"
              type="button"
              variant="ghost"
              size="icon-sm"
              :disabled="busy || !item.desiredVersion"
              :title="$t('quotaAndExpiry')"
              :aria-label="$t('quotaAndExpiry')"
              @click="$emit('policy', item.name)"
            >
              <SlidersHorizontal aria-hidden="true" />
            </Button>
            <Button
              v-if="recoveryDialogKind(item) === 'create'"
              type="button"
              variant="ghost"
              size="icon-sm"
              :disabled="busy || !userAction.allowed || !item.desiredVersion"
              :title="$t('retryCreateUser')"
              :aria-label="$t('retryCreateUser')"
              @click="
                $emit('desired', 'create', item.name, item.desiredVersion ?? 0)
              "
            >
              <UserPlus aria-hidden="true" />
            </Button>
            <Button
              v-else-if="recoveryDialogKind(item) === 'rotate'"
              type="button"
              variant="ghost"
              size="icon-sm"
              :disabled="busy || !userAction.allowed || !item.desiredVersion"
              :title="$t('retryRotatePassword')"
              :aria-label="$t('retryRotatePassword')"
              @click="
                $emit('desired', 'rotate', item.name, item.desiredVersion ?? 0)
              "
            >
              <KeyRound aria-hidden="true" />
            </Button>
            <Button
              v-else-if="recoveryDialogKind(item) === 'disable'"
              type="button"
              variant="ghost"
              size="icon-sm"
              class="text-destructive hover:text-destructive"
              :disabled="busy || !userAction.allowed || !item.desiredVersion"
              :title="$t('retryDisableUser')"
              :aria-label="$t('retryDisableUser')"
              @click="
                $emit('desired', 'disable', item.name, item.desiredVersion ?? 0)
              "
            >
              <UserX aria-hidden="true" />
            </Button>
            <Button
              v-else-if="recoveryDialogKind(item) === 'enable'"
              type="button"
              variant="ghost"
              size="icon-sm"
              :disabled="busy || !userAction.allowed || !item.desiredVersion"
              :title="$t('retryEnableUser')"
              :aria-label="$t('retryEnableUser')"
              @click="
                $emit('desired', 'enable', item.name, item.desiredVersion ?? 0)
              "
            >
              <UserCheck aria-hidden="true" />
            </Button>
            <Button
              v-if="!item.recoveryRequired"
              type="button"
              variant="ghost"
              size="icon-sm"
              :disabled="busy || !userAction.allowed || !item.desiredVersion"
              :title="$t('rotatePassword')"
              :aria-label="$t('rotatePassword')"
              @click="
                $emit('desired', 'rotate', item.name, item.desiredVersion ?? 0)
              "
            >
              <KeyRound aria-hidden="true" />
            </Button>
            <Button
              v-if="!item.recoveryRequired && item.desiredEnabled === false"
              type="button"
              variant="ghost"
              size="icon-sm"
              :disabled="busy || !userAction.allowed || !item.desiredVersion"
              :title="$t('enableUser')"
              :aria-label="$t('enableUser')"
              @click="
                $emit('desired', 'enable', item.name, item.desiredVersion ?? 0)
              "
            >
              <UserCheck aria-hidden="true" />
            </Button>
            <Button
              v-else-if="!item.recoveryRequired"
              type="button"
              variant="ghost"
              size="icon-sm"
              class="text-destructive hover:text-destructive"
              :disabled="busy || !userAction.allowed || !item.desiredVersion"
              :title="$t('disableUser')"
              :aria-label="$t('disableUser')"
              @click="
                $emit('desired', 'disable', item.name, item.desiredVersion ?? 0)
              "
            >
              <UserX aria-hidden="true" />
            </Button>
          </span>
        </li>
      </ul>
      <p v-else class="text-muted-foreground m-0 text-sm">
        {{ $t("noUsers") }}
      </p>
    </template>
    <template v-else>
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
              v-if="item.recoveryRequired && !item.recoveryMutationKind"
              variant="outline"
              class="text-destructive"
              >{{ $t("manualReconciliationRequired") }}</Badge
            >
          </span>
          <Button
            v-if="recoveryDialogKind(item) === 'group'"
            type="button"
            variant="ghost"
            size="icon-sm"
            :disabled="busy || !groupAction.allowed"
            :title="$t('retryApplyGroup')"
            :aria-label="$t('retryApplyGroup')"
            @click="
              $emit('desired', 'group', item.name, item.desiredVersion ?? 0)
            "
          >
            <ListPlus aria-hidden="true" />
          </Button>
          <Button
            v-else-if="!item.recoveryRequired"
            type="button"
            variant="ghost"
            size="icon-sm"
            :disabled="busy || !groupAction.allowed"
            :title="$t('applyGroup')"
            :aria-label="$t('applyGroup')"
            @click="
              $emit('desired', 'group', item.name, item.desiredVersion ?? 0)
            "
          >
            <ListPlus aria-hidden="true" />
          </Button>
        </li>
      </ul>
      <p v-else class="text-muted-foreground m-0 text-sm">
        {{ $t("noGroups") }}
      </p>
    </template>
  </SectionCard>
</template>
