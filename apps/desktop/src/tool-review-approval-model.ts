import {
  parseToolReviewSummary,
  type ToolReviewSummary,
} from "./tool-review-record-model";

export const MAX_PENDING_TOOL_APPROVALS = 16;

/** Tombstones belong only to the last successful snapshot. Failed/offline
 * observations do not reconcile, so reconnecting cannot replay a settled ID. */
export class ApprovalTombstones {
  private pending = new Set<string>();
  private settled = new Set<string>();
  get size(): number {
    return this.settled.size;
  }
  reconcile(snapshot: ReadonlyArray<{ id: string }>): void {
    if (snapshot.length > MAX_PENDING_TOOL_APPROVALS) invalid();
    this.pending = new Set(snapshot.map((row) => row.id));
    for (const id of this.settled)
      if (!this.pending.has(id)) this.settled.delete(id);
  }
  add(id: string): void {
    if (this.pending.has(id)) this.settled.add(id);
  }
  has(id: string): boolean {
    return this.settled.has(id);
  }
}

export interface PendingToolApproval {
  id: string;
  request_id: string;
  requested_at: string;
  expires_at: string;
  summary: ToolReviewSummary;
}
export interface ToolApprovalDetail {
  id: string;
  request_id: string;
  user_request: string;
  calls: Array<{ index: number; name: string; arguments: string }>;
}
function invalid(): never {
  throw new Error("tool_review_approval_invalid");
}
function object(value: unknown, keys: string[]): Record<string, unknown> {
  if (
    !value ||
    typeof value !== "object" ||
    Array.isArray(value) ||
    Object.keys(value).some((key) => !keys.includes(key))
  )
    invalid();
  return value as Record<string, unknown>;
}
export function validateApprovalId(value: unknown): asserts value is string {
  if (typeof value !== "string" || !/^[a-zA-Z0-9_-]{1,128}$/.test(value))
    invalid();
}
export function validateApprovalDecision(
  value: unknown,
): asserts value is "allow" | "deny" {
  if (value !== "allow" && value !== "deny") invalid();
}
export function parsePendingToolApprovals(
  value: unknown,
): PendingToolApproval[] {
  const list = object(value, ["items"]);
  if (
    !Array.isArray(list.items) ||
    list.items.length > MAX_PENDING_TOOL_APPROVALS
  )
    invalid();
  const seen = new Set<string>();
  return list.items.map((item) => {
    const row = object(item, [
      "id",
      "request_id",
      "requested_at",
      "expires_at",
      "summary",
    ]);
    validateApprovalId(row.id);
    validateApprovalId(row.request_id);
    if (seen.has(row.id)) invalid();
    seen.add(row.id);
    const summary = parseToolReviewSummary(row.summary);
    const approval = summary.approval;
    if (
      !approval ||
      summary.status !== "pending" ||
      approval.state !== "pending" ||
      approval.id !== row.id ||
      approval.requested_at !== row.requested_at ||
      approval.expires_at !== row.expires_at
    )
      invalid();
    return {
      id: row.id,
      request_id: row.request_id,
      requested_at: approval.requested_at,
      expires_at: approval.expires_at,
      summary,
    };
  });
}
export function parseToolApprovalDetail(
  value: unknown,
  target: { id: string; request_id?: string },
): ToolApprovalDetail {
  const detail = object(value, ["id", "request_id", "user_request", "calls"]);
  validateApprovalId(detail.id);
  validateApprovalId(detail.request_id);
  if (
    detail.id !== target.id ||
    (target.request_id !== undefined &&
      detail.request_id !== target.request_id) ||
    typeof detail.user_request !== "string" ||
    !Array.isArray(detail.calls) ||
    detail.calls.length > 64
  )
    invalid();
  const seen = new Set<number>();
  return {
    id: detail.id,
    request_id: detail.request_id,
    user_request: detail.user_request,
    calls: detail.calls.map((value) => {
      const call = object(value, ["index", "name", "arguments"]);
      if (
        typeof call.index !== "number" ||
        !Number.isSafeInteger(call.index) ||
        call.index < 0 ||
        seen.has(call.index) ||
        typeof call.name !== "string" ||
        typeof call.arguments !== "string"
      )
        invalid();
      seen.add(call.index);
      return { index: call.index, name: call.name, arguments: call.arguments };
    }),
  };
}
