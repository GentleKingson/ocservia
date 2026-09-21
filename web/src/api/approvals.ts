import {
  OperationsApi,
  type Approval,
  type ApprovalDecision,
} from "@ocservia/api-client";
import { configuration, requestInit } from "./transport";

const operations = new OperationsApi(configuration);

export function getApproval(
  approvalId: string,
  signal?: AbortSignal,
): Promise<Approval> {
  return operations.getApprovalRequest({ approvalId }, requestInit(signal));
}

export function approveRequest(
  approvalId: string,
  approvalDecision: ApprovalDecision,
): Promise<Approval> {
  return operations.approveRequest({ approvalId, approvalDecision });
}
