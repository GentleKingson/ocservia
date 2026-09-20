import type {
  ArtifactGrant,
  Certificate,
  NodeObservedState,
  Operation,
} from "@ocservia/api-client";
import { onScopeDispose, ref, watch, type Ref } from "vue";
import {
  createCertificate,
  createCertificateP12,
  downloadCertificateArtifact,
  getCertificate,
  issueCertificate,
  listNodeCertificates,
  revokeCertificate,
} from "../../api/certificates";
import { getOperation } from "../../api/operations";
import {
  beginNodeMutation,
  createNodeWorkflow,
  finishNodeMutation,
  readNodeReceipt,
  rememberNodeReceipt,
  waitForNodeMutation,
  waitForNodePoll,
  type NodeWorkflowContext,
  consumeCertificateGrant,
  readCertificateGrant,
  rememberCertificateGrant,
} from "../node-workflow";
import type { WorkspaceContext } from "../../api/workspace";

interface CertificatesInputs {
  currentNode: Readonly<Ref<NodeObservedState | undefined>>;
  readReady: Readonly<Ref<boolean>>;
  workspaceContext: () => WorkspaceContext;
  trackOperation: (id: string) => Promise<void>;
  t: (key: string) => string;
}

export function useNodeCertificates({
  currentNode,
  readReady,
  workspaceContext,
  trackOperation,
  t,
}: CertificatesInputs) {
  const certificateDialog = ref(false);
  const certificate = ref<Certificate>();
  const certificateGrant = ref<ArtifactGrant>();
  const certificateOperation = ref<Operation>();
  const certificateCommonName = ref("");
  const certificateDnsNames = ref("");
  const certificateReason = ref("");
  const certificateApproval = ref("");
  const certificateError = ref("");
  const certificateLoading = ref(false);
  const certificateWorkflow = createNodeWorkflow(
    () => currentNode.value?.id,
    () => certificateDialog.value,
    workspaceContext,
  );
  let certificateContext: NodeWorkflowContext | undefined;

  function resetDialog(): void {
    certificateWorkflow.cancel();
    certificateContext = undefined;
    certificate.value = undefined;
    certificateGrant.value = undefined;
    certificateOperation.value = undefined;
    certificateError.value = "";
    certificateLoading.value = false;
  }
  watch(
    certificateDialog,
    (open) => {
      if (!open) resetDialog();
    },
    { flush: "sync" },
  );
  onScopeDispose(() => {
    certificateDialog.value = false;
    resetDialog();
  });

  async function openCertificate(): Promise<void> {
    const node = currentNode.value;
    const commonName = node?.name ?? "";
    if (!node || !readReady.value) return;
    const context = certificateWorkflow.begin(node.id);
    certificateContext = context;
    certificateDialog.value = true;
    certificate.value = undefined;
    certificateGrant.value = undefined;
    certificateOperation.value = undefined;
    certificateCommonName.value = commonName;
    certificateDnsNames.value = "";
    certificateReason.value = "";
    certificateApproval.value = "";
    certificateError.value = "";
    certificateLoading.value = true;
    try {
      const pending = waitForNodeMutation(context, "certificate");
      if (pending) await pending;
      if (!certificateWorkflow.isCurrent(context)) return;
      const receipt = readNodeReceipt(context, "certificate");
      const records = await listNodeCertificates(node.id, context.signal);
      if (!certificateWorkflow.isCurrent(context)) return;
      const record =
        records.find(
          (record) =>
            record.id === receipt.resourceId && record.state !== "revoked",
        ) ?? records.find((record) => record.state !== "revoked");
      if (record) {
        if (
          record.nodeId !== context.nodeId ||
          record.workspaceId !== context.workspace.id
        )
          return;
        await pollCertificate(context, record);
        if (!certificateWorkflow.isCurrent(context)) return;
        certificateGrant.value = readCertificateGrant(context, record.id);
      }
      if (!certificateWorkflow.isCurrent(context) || !receipt.operationId)
        return;
      const operation = await getOperation(receipt.operationId, context.signal);
      if (
        certificateWorkflow.isCurrent(context) &&
        operation.nodeId === context.nodeId
      )
        certificateOperation.value = operation;
    } catch (error) {
      if (!certificateWorkflow.isCurrent(context)) return;
      certificateError.value =
        error instanceof Error ? error.message : t("certificateRecordsFailed");
    } finally {
      if (certificateWorkflow.isCurrent(context))
        certificateLoading.value = false;
    }
  }

  async function pollCertificate(
    context: NodeWorkflowContext,
    value: Certificate,
  ): Promise<void> {
    if (!certificateWorkflow.isCurrent(context)) return;
    certificate.value = value;
    for (
      let attempt = 0;
      attempt < 30 && value.state === "csr_pending";
      attempt += 1
    ) {
      await waitForNodePoll(context.signal);
      if (!certificateWorkflow.isCurrent(context)) return;
      value = await getCertificate(value.id, context.signal);
      if (!certificateWorkflow.isCurrent(context)) return;
      certificate.value = value;
    }
  }

  async function submitCertificateRequest(): Promise<void> {
    const node = currentNode.value;
    const context = certificateContext;
    if (
      !node ||
      !context ||
      !certificateWorkflow.isCurrent(context) ||
      certificateLoading.value ||
      !certificateCommonName.value.trim() ||
      !certificateReason.value.trim()
    )
      return;
    const ticket = beginNodeMutation(context, "certificate");
    if (!ticket) return;
    certificateLoading.value = true;
    certificateError.value = "";
    try {
      const value = await createCertificate(node.id, {
        expectedVersion: node.version,
        commonName: certificateCommonName.value.trim(),
        dnsNames: new Set(
          certificateDnsNames.value
            .split(",")
            .map((name) => name.trim())
            .filter(Boolean),
        ),
        keyBits: 3072,
        reason: certificateReason.value.trim(),
      });
      rememberNodeReceipt(ticket, {
        resourceId: value.id,
        operationId: value.operationId,
      });
      await pollCertificate(context, value);
    } catch (error) {
      if (!certificateWorkflow.isCurrent(context)) return;
      certificateError.value =
        error instanceof Error ? error.message : t("certificateRequestFailed");
    } finally {
      finishNodeMutation(ticket);
      if (certificateWorkflow.isCurrent(context))
        certificateLoading.value = false;
    }
  }

  async function submitCertificateIssue(): Promise<void> {
    const context = certificateContext;
    const current = certificate.value;
    if (
      !context ||
      !certificateWorkflow.isCurrent(context) ||
      certificateLoading.value ||
      !current ||
      !certificateApproval.value.trim() ||
      !certificateReason.value.trim()
    )
      return;
    const ticket = beginNodeMutation(context, "certificate");
    if (!ticket) return;
    certificateLoading.value = true;
    certificateError.value = "";
    try {
      const issued = await issueCertificate(current.id, {
        approvalId: certificateApproval.value.trim(),
        reason: certificateReason.value.trim(),
      });
      rememberNodeReceipt(ticket, {
        resourceId: issued.id,
        operationId: issued.operationId,
      });
      if (certificateWorkflow.isCurrent(context)) certificate.value = issued;
    } catch (error) {
      if (!certificateWorkflow.isCurrent(context)) return;
      certificateError.value =
        error instanceof Error ? error.message : t("certificateIssueFailed");
    } finally {
      finishNodeMutation(ticket);
      if (certificateWorkflow.isCurrent(context))
        certificateLoading.value = false;
    }
  }

  async function createP12(): Promise<void> {
    const node = currentNode.value;
    const context = certificateContext;
    const current = certificate.value;
    if (
      !context ||
      !certificateWorkflow.isCurrent(context) ||
      certificateLoading.value ||
      !current ||
      !node ||
      !certificateReason.value.trim() ||
      !certificateApproval.value.trim()
    )
      return;
    if (readCertificateGrant(context, current.id)?.downloadToken) return;
    const ticket = beginNodeMutation(context, "certificate");
    if (!ticket) return;
    certificateLoading.value = true;
    certificateError.value = "";
    try {
      const grant = await createCertificateP12(current.id, {
        expectedVersion: node.version,
        certificateVersion: current.version,
        approvalId: certificateApproval.value.trim(),
        reason: certificateReason.value.trim(),
      });
      rememberCertificateGrant(context, current.id, grant);
      rememberNodeReceipt(ticket, {
        resourceId: current.id,
        operationId: grant.operation.id,
      });
      if (!certificateWorkflow.isCurrent(context)) return;
      certificateGrant.value = readCertificateGrant(context, current.id);
      certificateOperation.value = grant.operation;
      await trackOperation(grant.operation.id);
    } catch (error) {
      if (!certificateWorkflow.isCurrent(context)) return;
      certificateError.value =
        error instanceof Error ? error.message : t("p12CreationFailed");
    } finally {
      finishNodeMutation(ticket);
      if (certificateWorkflow.isCurrent(context))
        certificateLoading.value = false;
    }
  }

  async function downloadP12(): Promise<void> {
    const grant = certificateGrant.value;
    const context = certificateContext;
    const certificateId = certificate.value?.id;
    if (
      !context ||
      !certificateId ||
      !certificateWorkflow.isCurrent(context) ||
      certificateLoading.value ||
      !grant?.downloadToken
    )
      return;
    const ticket = beginNodeMutation(context, "certificate");
    if (!ticket) return;
    certificateLoading.value = true;
    certificateError.value = "";
    try {
      const blob = await downloadCertificateArtifact(
        grant.artifactId,
        grant.downloadToken,
      );
      // The Controller consumes the artifact before sending its bytes. Finish
      // this user-initiated download even if its originating dialog has closed.
      consumeCertificateGrant(context, certificateId, grant.artifactId);
      const link = document.createElement("a");
      link.href = URL.createObjectURL(blob);
      link.download = "certificate.p12";
      try {
        link.click();
      } finally {
        URL.revokeObjectURL(link.href);
      }
    } catch (error) {
      if (!certificateWorkflow.isCurrent(context)) return;
      certificateError.value =
        error instanceof Error ? error.message : t("p12DownloadFailed");
    } finally {
      finishNodeMutation(ticket);
      if (certificateWorkflow.isCurrent(context))
        certificateLoading.value = false;
    }
  }

  async function revokeCurrentCertificate(): Promise<void> {
    const node = currentNode.value;
    const context = certificateContext;
    const current = certificate.value;
    if (
      !context ||
      !certificateWorkflow.isCurrent(context) ||
      certificateLoading.value ||
      !current ||
      !node ||
      !certificateReason.value.trim() ||
      !certificateApproval.value.trim()
    )
      return;
    const ticket = beginNodeMutation(context, "certificate");
    if (!ticket) return;
    certificateLoading.value = true;
    certificateError.value = "";
    try {
      const operation = await revokeCertificate(current.id, {
        expectedVersion: node.version,
        certificateVersion: current.version,
        approvalId: certificateApproval.value.trim(),
        reason: certificateReason.value.trim(),
      });
      rememberNodeReceipt(ticket, {
        resourceId: current.id,
        operationId: operation.id,
      });
      if (!certificateWorkflow.isCurrent(context)) return;
      certificateOperation.value = operation;
      await trackOperation(operation.id);
      if (!certificateWorkflow.isCurrent(context)) return;
      const value = await getCertificate(current.id, context.signal);
      if (certificateWorkflow.isCurrent(context)) certificate.value = value;
    } catch (error) {
      if (!certificateWorkflow.isCurrent(context)) return;
      certificateError.value =
        error instanceof Error ? error.message : t("certificateRevokeFailed");
    } finally {
      finishNodeMutation(ticket);
      if (certificateWorkflow.isCurrent(context))
        certificateLoading.value = false;
    }
  }
  return {
    certificateDialog,
    certificate,
    certificateGrant,
    certificateOperation,
    certificateCommonName,
    certificateDnsNames,
    certificateReason,
    certificateApproval,
    certificateError,
    certificateLoading,
    openCertificate,
    submitCertificateRequest,
    submitCertificateIssue,
    createP12,
    downloadP12,
    revokeCurrentCertificate,
  };
}
