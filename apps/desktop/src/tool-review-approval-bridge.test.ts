// @vitest-environment happy-dom
import { beforeEach, describe, expect, it, vi } from "vitest";
const { invoke } = vi.hoisted(() => ({ invoke: vi.fn() }));
vi.mock("@tauri-apps/api/core", () => ({ invoke }));
import {
  decideToolReviewApproval,
  getToolReviewApproval,
  listToolReviewApprovals,
} from "./bridge";
import {
  parsePendingToolApprovals,
  parseToolApprovalDetail,
} from "./tool-review-approval-model";
const requested_at = "2026-10-10T10:00:00Z",
  expires_at = "2026-10-10T10:02:00Z";
const approval = {
  id: "approval_1",
  state: "pending",
  review_status: "flagged",
  requested_at,
  expires_at,
};
const item = {
  id: approval.id,
  request_id: "req_1",
  requested_at,
  expires_at,
  summary: {
    mode: "ask",
    status: "pending",
    model: "jev",
    config_revision: "r1",
    started_at: requested_at,
    latency_ms: 10,
    calls: [],
    approval,
  },
};
const detail = {
  id: approval.id,
  request_id: "req_1",
  user_request: "private request",
  calls: [{ index: 0, name: "run", arguments: '{"private":"value"}' }],
};
beforeEach(() => {
  Reflect.set(window, "__TAURI_INTERNALS__", {});
  invoke.mockReset();
});
describe("approval bridge validates boundaries", () => {
  it("returns safe metadata and request-scoped temporary detail", async () => {
    invoke
      .mockResolvedValueOnce({ items: [item] })
      .mockResolvedValueOnce(detail);
    expect(await listToolReviewApprovals()).toEqual([item]);
    expect(await getToolReviewApproval(approval.id)).toEqual(detail);
    expect(invoke).toHaveBeenLastCalledWith("get_tool_review_approval", {
      approvalId: approval.id,
    });
  });
  it("sends a one-batch decision and accepts only matching server acknowledgement", async () => {
    const approved = {
      ...approval,
      state: "approved",
      decided_at: "2026-10-10T10:01:00Z",
    };
    invoke.mockResolvedValueOnce(approved);
    expect(await decideToolReviewApproval(approval.id, "allow")).toEqual(
      approved,
    );
    expect(invoke).toHaveBeenLastCalledWith("decide_tool_review_approval", {
      approvalId: approval.id,
      decision: "allow",
    });
    invoke.mockResolvedValueOnce({ ...approved, id: "another" });
    await expect(
      decideToolReviewApproval(approval.id, "allow"),
    ).rejects.toThrow("tool_review_approval_invalid");
    invoke.mockResolvedValueOnce({ ...approved, state: "denied" });
    await expect(
      decideToolReviewApproval(approval.id, "allow"),
    ).rejects.toThrow("tool_review_approval_invalid");
  });
  it.each(["", "../secret", "a/b", "a".repeat(129)])(
    "rejects unsafe ID %s without IPC",
    async (id) => {
      await expect(getToolReviewApproval(id)).rejects.toThrow();
      await expect(decideToolReviewApproval(id, "deny")).rejects.toThrow();
      expect(invoke).not.toHaveBeenCalled();
    },
  );
  it("rejects an unknown decision before IPC", async () => {
    await expect(
      decideToolReviewApproval(approval.id, "once" as "allow"),
    ).rejects.toThrow();
    expect(invoke).not.toHaveBeenCalled();
  });
  it("never accepts raw fields through the metadata list", () => {
    expect(() =>
      parsePendingToolApprovals({
        items: [{ ...item, user_request: "private" }],
      }),
    ).toThrow();
    expect(() =>
      parsePendingToolApprovals({
        items: [
          { ...item, summary: { ...item.summary, arguments: "private" } },
        ],
      }),
    ).toThrow();
    expect(() => parsePendingToolApprovals({ items: [item, item] })).toThrow();
    expect(() =>
      parsePendingToolApprovals({ items: [{ ...item, id: "wrong" }] }),
    ).toThrow();
    expect(() =>
      parsePendingToolApprovals({
        items: [{ ...item, summary: { ...item.summary, status: "flagged" } }],
      }),
    ).toThrow();
  });
  it("rejects cross-request, duplicate and malformed argument details", () => {
    expect(() => parseToolApprovalDetail(detail, { id: "wrong" })).toThrow();
    expect(() =>
      parseToolApprovalDetail(detail, {
        id: approval.id,
        request_id: "req_wrong",
      }),
    ).toThrow();
    expect(() =>
      parseToolApprovalDetail(
        { ...detail, calls: [detail.calls[0], detail.calls[0]] },
        { id: approval.id },
      ),
    ).toThrow();
    expect(() =>
      parseToolApprovalDetail(
        {
          ...detail,
          calls: [{ ...detail.calls[0], arguments: { private: true } }],
        },
        { id: approval.id },
      ),
    ).toThrow();
  });
  it("preserves safe locked and gone error codes", async () => {
    invoke.mockRejectedValueOnce("raw_locked");
    await expect(getToolReviewApproval(approval.id)).rejects.toThrow(
      "raw_locked",
    );
    invoke.mockRejectedValueOnce("tool_review_approval_not_pending");
    await expect(
      decideToolReviewApproval(approval.id, "allow"),
    ).rejects.toThrow("tool_review_approval_not_pending");
  });
});
