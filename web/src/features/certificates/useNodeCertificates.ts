import type {
  ArtifactGrant,
  Certificate,
  NodeObservedState,
  Operation,
} from "@/api/models";
import { onScopeDispose, ref, watch, type Ref } from "vue";
import {
  createCertificate,
  createCertificateP12,
  newArtifactRequestId,
  downloadCertificateArtifact,
  getCertificate,
  issueCertificate,
  listNodeCertificates,
  revokeCertificate,
} from "../../api/certificates";
import { getOperation } from "../../api/operations";
import { ResponseError } from "../../api/transport";
import { downloadBlob } from "../../shared/download";
import {
  consumeCertificateGrant,
  createNodeWorkflow,
  pollNodeResource,
  readCertificateGrant,
  readNodeWorkflowReference,
  rememberCertificateGrant,
  rememberNodeWorkflowReference,
  runNodeMutation,
  waitForNodeMutation,
  type NodeWorkflowContext,
} from "../node-workflow";

interface CertificatesInputs {
  currentNode: Readonly<Ref<NodeObservedState | undefined>>;
  readReady: Readonly<Ref<boolean>>;
  workspaceId: () => string | undefined;
  trackOperation: (id: string) => Promise<void>;
  t: (key: string) => string;
}

export function useNodeCertificates({
  currentNode,
  readReady,
  workspaceId,
  trackOperation,
  t,
}: CertificatesInputs) {
  const certificateDialog = ref(false);
  const certificate = ref<Certificate>();
  const certificateGrant = ref<ArtifactGrant>();
  const pendingExports = new Map<
    string,
    Parameters<typeof createCertificateP12>[1]
  >();
  const certificateOperation = ref<Operation>();
  const certificateCommonName = ref("");
  const certificateDnsNames = ref("");
  const certificateReason = ref("");
  const certificateError = ref("");
  const certificateLoading = ref(false);
  const certificateWorkflow = createNodeWorkflow(
    () => currentNode.value?.id,
    () => certificateDialog.value,
    workspaceId,
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
    certificateError.value = "";
    certificateLoading.value = true;
    try {
      const pending = waitForNodeMutation(context, "certificate");
      if (pending) await pending;
      if (!certificateWorkflow.isCurrent(context)) return;
      const reference = readNodeWorkflowReference(context, "certificate");
      const records = await listNodeCertificates(node.id, context.signal);
      if (!certificateWorkflow.isCurrent(context)) return;
      const record =
        records.find(
          (record) =>
            record.id === reference.resourceId && record.state !== "revoked",
        ) ?? records.find((record) => record.state !== "revoked");
      if (record) {
        if (
          record.node_id !== context.nodeId ||
          record.workspace_id !== context.workspaceId
        )
          return;
        await pollCertificate(context, record);
        if (!certificateWorkflow.isCurrent(context)) return;
        certificateGrant.value = readCertificateGrant(context, record.id);
      }
      if (
        !certificateWorkflow.isCurrent(context) ||
        !record ||
        reference.resourceId !== record.id ||
        !reference.operationId
      )
        return;
      const operation = await getOperation(
        reference.operationId,
        context.signal,
      );
      if (
        certificateWorkflow.isCurrent(context) &&
        operation.node_id === context.nodeId
      )
        certificateOperation.value = operation;
    } catch (error) {
      if (!certificateWorkflow.isCurrent(context)) return;
      certificateError.value =
        (error instanceof Error && error.message) ||
        t("certificateRecordsFailed");
    } finally {
      if (certificateWorkflow.isCurrent(context))
        certificateLoading.value = false;
    }
  }

  async function pollCertificate(
    context: NodeWorkflowContext,
    value: Certificate,
  ): Promise<void> {
    await pollNodeResource(
      certificateWorkflow,
      context,
      value,
      (current) => current.state === "csr_pending",
      (current, signal) => getCertificate(current.id, signal),
      (current) => {
        certificate.value = current;
      },
    );
  }

  async function submitCertificateRequest(): Promise<void> {
    const node = currentNode.value;
    if (
      !node ||
      !certificateCommonName.value.trim() ||
      !certificateReason.value.trim()
    )
      return;
    await runNodeMutation({
      workflow: certificateWorkflow,
      context: certificateContext,
      kind: "certificate",
      loading: certificateLoading,
      error: certificateError,
      fallbackError: () => t("certificateRequestFailed"),
      async run(ticket, context) {
        const value = await createCertificate(node.id, {
          expected_version: node.version,
          common_name: certificateCommonName.value.trim(),
          dns_names: [
            ...new Set(
              certificateDnsNames.value
                .split(",")
                .map((name) => name.trim())
                .filter(Boolean),
            ),
          ],
          key_bits: 3072,
          reason: certificateReason.value.trim(),
        });
        rememberNodeWorkflowReference(ticket, {
          resourceId: value.id,
          operationId: value.operation_id,
        });
        await pollCertificate(context, value);
      },
    });
  }

  async function submitCertificateIssue(): Promise<void> {
    const current = certificate.value;
    if (!current || !certificateReason.value.trim()) return;
    await runNodeMutation({
      workflow: certificateWorkflow,
      context: certificateContext,
      kind: "certificate",
      loading: certificateLoading,
      error: certificateError,
      fallbackError: () => t("certificateIssueFailed"),
      async run(ticket, context) {
        const issued = await issueCertificate(current.id, {
          reason: certificateReason.value.trim(),
        });
        rememberNodeWorkflowReference(ticket, {
          resourceId: issued.id,
          operationId: issued.operation_id,
        });
        if (certificateWorkflow.isCurrent(context)) certificate.value = issued;
      },
    });
  }

  async function createP12(): Promise<void> {
    const node = currentNode.value;
    const context = certificateContext;
    const current = certificate.value;
    if (
      !context ||
      !certificateWorkflow.isCurrent(context) ||
      !current ||
      !node ||
      !certificateReason.value.trim() ||
      readCertificateGrant(context, current.id)?.download_token
    )
      return;
    const exportKey = JSON.stringify([
      context.workspaceId,
      context.nodeId,
      current.id,
    ]);
    await runNodeMutation({
      workflow: certificateWorkflow,
      context,
      kind: "certificate",
      loading: certificateLoading,
      error: certificateError,
      fallbackError: () => t("p12CreationFailed"),
      async run(ticket, activeContext) {
        let request = pendingExports.get(exportKey);
        if (!request) {
          request = {
            artifact_request_id: newArtifactRequestId(),
            expected_version: node.version,
            certificate_version: current.version,
            reason: certificateReason.value.trim(),
          };
          pendingExports.set(exportKey, request);
        }
        const grant = await createCertificateP12(current.id, request);
        pendingExports.delete(exportKey);
        rememberCertificateGrant(activeContext, current.id, grant);
        rememberNodeWorkflowReference(ticket, {
          resourceId: current.id,
          operationId: grant.operation.id,
        });
        if (!certificateWorkflow.isCurrent(activeContext)) return;
        certificateGrant.value = readCertificateGrant(
          activeContext,
          current.id,
        );
        certificateOperation.value = grant.operation;
        if (!grant.download_token)
          certificateError.value = t("p12GrantUnavailable");
        await trackOperation(grant.operation.id);
      },
      onError(error) {
        if (
          error instanceof ResponseError &&
          [400, 409].includes(error.response.status)
        )
          pendingExports.delete(exportKey);
      },
    });
  }

  async function downloadP12(): Promise<void> {
    const grant = certificateGrant.value;
    const certificateId = certificate.value?.id;
    const token = grant?.download_token;
    if (!certificateId || !token) return;
    await runNodeMutation({
      workflow: certificateWorkflow,
      context: certificateContext,
      kind: "certificate",
      loading: certificateLoading,
      error: certificateError,
      fallbackError: () => t("p12DownloadFailed"),
      async run(_ticket, context) {
        const blob = await downloadCertificateArtifact(
          grant.artifact_id,
          token,
        );
        // The Controller consumes the artifact before sending its bytes. Finish
        // this user-initiated download even if its originating dialog has closed.
        consumeCertificateGrant(context, certificateId, grant.artifact_id);
        downloadBlob(blob, "certificate.p12");
      },
    });
  }

  async function revokeCurrentCertificate(): Promise<void> {
    const node = currentNode.value;
    const current = certificate.value;
    if (!current || !node || !certificateReason.value.trim()) return;
    await runNodeMutation({
      workflow: certificateWorkflow,
      context: certificateContext,
      kind: "certificate",
      loading: certificateLoading,
      error: certificateError,
      fallbackError: () => t("certificateRevokeFailed"),
      async run(ticket, context) {
        const operation = await revokeCertificate(current.id, {
          expected_version: node.version,
          certificate_version: current.version,
          reason: certificateReason.value.trim(),
        });
        rememberNodeWorkflowReference(ticket, {
          resourceId: current.id,
          operationId: operation.id,
        });
        if (!certificateWorkflow.isCurrent(context)) return;
        certificateOperation.value = operation;
        await trackOperation(operation.id);
        if (!certificateWorkflow.isCurrent(context)) return;
        const value = await getCertificate(current.id, context.signal);
        if (certificateWorkflow.isCurrent(context)) certificate.value = value;
      },
    });
  }
  return {
    certificateDialog,
    certificate,
    certificateGrant,
    certificateOperation,
    certificateCommonName,
    certificateDnsNames,
    certificateReason,
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
