<script setup lang="ts">
import { computed, ref } from "vue";
import { Button } from "@/components/ui/button";
import { Textarea } from "@/components/ui/textarea";
import FormField from "@/components/common/FormField.vue";
import OperationDialog from "@/components/common/OperationDialog.vue";
import { useFleetStore } from "@/shared/fleet";

defineProps<{ nodeName: string | undefined }>();
const fleet = useFleetStore();
const pendingAction = ref<{
  kind: "disconnect" | "terminate" | "unban" | "reload" | "upgradeAgent";
  target: string;
  label: string;
}>();
const actionReady = computed(() => {
  if (!fleet.selected || fleet.selecting || fleet.selectionError) return false;
  switch (pendingAction.value?.kind) {
    case "disconnect":
    case "terminate":
      return fleet.sessionsState === "ready";
    case "unban":
      return fleet.ipBansState === "ready";
    case "reload":
    case "upgradeAgent":
      return true;
  }
  return false;
});
const reason = ref("");
function openAction(
  kind: "disconnect" | "terminate" | "unban" | "reload" | "upgradeAgent",
  target: string,
  label: string,
): void {
  reason.value = "";
  pendingAction.value = { kind, target, label };
}

async function submitAction(): Promise<void> {
  const action = pendingAction.value;
  const explanation = reason.value.trim();
  if (!action || !explanation || !actionReady.value) return;
  pendingAction.value = undefined;
  if (action.kind === "disconnect")
    await fleet.disconnectSession(action.target, explanation);
  else if (action.kind === "terminate")
    await fleet.terminateSession(action.target, explanation);
  else if (action.kind === "unban")
    await fleet.removeIpBan(action.target, explanation);
  else if (action.kind === "upgradeAgent")
    await fleet.upgradeAgent(action.target, explanation);
  else await fleet.reloadService(explanation);
}

function close() {
  pendingAction.value = undefined;
}
defineExpose({ open: openAction, close });
</script>
<template>
  <OperationDialog
    v-if="pendingAction"
    :title="pendingAction.label"
    :subject="pendingAction.target || nodeName"
    @close="close"
    @submit="submitAction"
  >
    <FormField
      v-if="pendingAction.kind === 'upgradeAgent'"
      id="upgrade-target"
      :label="$t('targetVersion')"
      :help="$t('upgradeTargetReadOnly')"
    >
      <output id="upgrade-target" class="font-mono text-sm">{{
        pendingAction.target
      }}</output>
    </FormField>
    <FormField id="operation-reason" :label="$t('reason')">
      <Textarea
        id="operation-reason"
        v-model="reason"
        maxlength="512"
        required
      />
    </FormField>
    <template #footer>
      <Button
        type="submit"
        :variant="
          pendingAction.kind === 'terminate' ? 'destructive' : 'default'
        "
        :disabled="!reason.trim() || !actionReady"
      >
        {{ $t("confirm") }}
      </Button>
    </template>
  </OperationDialog>
</template>
