<script setup lang="ts">
import { computed, onBeforeUnmount, ref } from "vue";
import { useI18n } from "vue-i18n";
import { createAgentRollout } from "@/api/agents";
import type { NodeObservedState } from "@/api/models";
import { ResponseError } from "@/api/transport";
import FormField from "@/components/common/FormField.vue";
import OperationDialog from "@/components/common/OperationDialog.vue";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Textarea } from "@/components/ui/textarea";

const props = defineProps<{
  selectedNodes: NodeObservedState[];
  onStarted: (rolloutId: string) => Promise<void>;
}>();

const { t } = useI18n();
const rolloutDialog = ref(false);
const rolloutBatchSize = ref(5);
const rolloutReason = ref("");
const rolloutStarting = ref(false);
const rolloutError = ref("");
// Bumped when unmounted; an accepted create keeps running server-side,
// but its late response no longer owns navigation.
let rolloutSubmission = 0;
// Bumped on every open; closing the dialog gives up the automatic navigation.
let rolloutOpening = 0;
// Set when a create may have succeeded without a response; unlike a definite
// failure, that warning survives reopening the dialog.
let rolloutUnconfirmed = false;

onBeforeUnmount(() => {
  rolloutSubmission += 1;
});

const rolloutTarget = computed(
  () => props.selectedNodes[0]?.recommended_agent_version ?? "",
);

// Only the stable Problem type selects a local message; server text is never
// shown. Unknown or unreadable bodies fall back to a generic rejection.
const rolloutRejections = new Map([
  ["https://ocservia.dev/problems/forbidden", "rolloutForbidden"],
  ["https://ocservia.dev/problems/no-eligible-nodes", "rolloutNoEligibleNodes"],
  ["https://ocservia.dev/problems/invalid-request", "rolloutRequestInvalid"],
]);

async function problemType(error: ResponseError): Promise<string> {
  if (error.type) return error.type;
  try {
    const problem: unknown = await error.response.json();
    return typeof problem === "object" &&
      problem !== null &&
      "type" in problem &&
      typeof problem.type === "string"
      ? problem.type
      : "";
  } catch {
    return "";
  }
}

function openRolloutDialog(): void {
  rolloutOpening += 1;
  if (!rolloutUnconfirmed) rolloutError.value = "";
  rolloutDialog.value = true;
}

async function submitRollout(): Promise<void> {
  if (rolloutStarting.value || !rolloutTarget.value) return;
  const ticket = ++rolloutSubmission;
  const opening = rolloutOpening;
  const owned = () => ticket === rolloutSubmission;
  rolloutStarting.value = true;
  if (!rolloutUnconfirmed) rolloutError.value = "";
  let createdId: string | undefined;
  let rejected: ResponseError | undefined;
  try {
    const rollout = await createAgentRollout(
      rolloutTarget.value,
      props.selectedNodes.map((node) => node.id),
      rolloutBatchSize.value,
      rolloutReason.value.trim(),
    );
    // Closing the dialog abandons only the navigation, never the request.
    if (!owned()) return;
    rolloutUnconfirmed = false;
    if (!rolloutDialog.value || opening !== rolloutOpening) return;
    rolloutDialog.value = false;
    rolloutReason.value = "";
    createdId = rollout.id;
  } catch (cause) {
    if (!owned()) return;
    // A lost POST response may still have created the rollout; never resend.
    // Until a create is confirmed, a definite rejection of an explicit retry
    // (e.g. a stale node revision) does not settle that earlier start.
    rolloutUnconfirmed ||= !(cause instanceof ResponseError);
    if (!rolloutUnconfirmed && cause instanceof ResponseError) rejected = cause;
    else rolloutError.value = t("rolloutStartUnconfirmed");
  } finally {
    if (ticket === rolloutSubmission) rolloutStarting.value = false;
  }
  if (rejected) {
    const key = rolloutRejections.get(await problemType(rejected));
    // The body is read after the request settled; a newer submit
    // or dialog owns the message by then.
    if (owned() && opening === rolloutOpening)
      rolloutError.value = t(key ?? "rolloutStartRejected");
  }
  // Outside the create's error handling: the rollout is confirmed even if
  // post-create handling fails.
  if (createdId) await props.onStarted(createdId);
}

defineExpose({ open: openRolloutDialog });
</script>

<template>
  <OperationDialog
    v-if="rolloutDialog"
    :title="$t('rollingUpgradeTitle')"
    :subject="rolloutTarget"
    :error="rolloutError"
    @close="rolloutDialog = false"
    @submit="submitRollout"
  >
    <FormField id="rollout-target" :label="$t('targetVersion')">
      <output id="rollout-target" class="font-mono text-sm break-all">{{
        rolloutTarget
      }}</output>
    </FormField>
    <FormField id="rollout-nodes" :label="$t('selectedNodes')">
      <output id="rollout-nodes" class="text-sm break-words"
        >{{ selectedNodes.length }}:
        {{ selectedNodes.map((node) => node.name).join(", ") }}</output
      >
    </FormField>
    <FormField id="rollout-canary" :label="$t('canary')">
      <output id="rollout-canary" class="text-sm">{{
        $t("canaryOneNode")
      }}</output>
    </FormField>
    <FormField id="rollout-batch-size" :label="$t('batchSize')">
      <Input
        id="rollout-batch-size"
        v-model.number="rolloutBatchSize"
        type="number"
        min="1"
        max="20"
        required
      />
    </FormField>
    <FormField id="rollout-reason" :label="$t('reason')">
      <Textarea
        id="rollout-reason"
        v-model="rolloutReason"
        maxlength="512"
        required
      />
    </FormField>
    <template #footer>
      <Button
        type="submit"
        :disabled="
          rolloutStarting ||
          !rolloutReason.trim() ||
          rolloutBatchSize < 1 ||
          rolloutBatchSize > 20
        "
      >
        {{ $t(rolloutStarting ? "rolloutStarting" : "startRollout") }}
      </Button>
    </template>
  </OperationDialog>
</template>
