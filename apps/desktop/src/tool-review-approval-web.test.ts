// @vitest-environment happy-dom
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
vi.mock("./edition", () => ({ isWebEdition: true }));
import {
  getToolReviewApproval,
  decideToolReviewApproval,
  listToolReviewApprovals,
} from "./bridge";
const requested_at = "2026-10-10T10:00:00Z",
  expires_at = "2026-10-10T10:02:00Z";
const approval = {
  id: "approval_1",
  state: "pending",
  review_status: "failed",
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
const fetchMock = vi.fn();
const response = (value: unknown, status = 200) =>
  new Response(JSON.stringify(value), {
    status,
    headers: { "Content-Type": "application/json" },
  });
beforeEach(() => {
  fetchMock.mockReset();
  vi.stubGlobal("fetch", fetchMock);
});
afterEach(() => vi.unstubAllGlobals());
describe("operator Web approval transport", () => {
  it("uses real same-origin authenticated no-store control API, never fake success", async () => {
    fetchMock.mockResolvedValueOnce(response({ items: [item] }));
    expect(await listToolReviewApprovals()).toEqual([item]);
    expect(fetchMock).toHaveBeenLastCalledWith(
      "/control/v1/tool-review/approvals",
      expect.objectContaining({
        method: "GET",
        credentials: "same-origin",
        cache: "no-store",
        redirect: "error",
      }),
    );
    const decided = {
      ...approval,
      state: "denied",
      decided_at: "2026-10-10T10:01:00Z",
    };
    fetchMock.mockResolvedValueOnce(response(decided));
    expect(await decideToolReviewApproval(approval.id, "deny")).toEqual(
      decided,
    );
    expect(fetchMock).toHaveBeenLastCalledWith(
      "/control/v1/tool-review/approvals/approval_1/decision",
      expect.objectContaining({
        method: "POST",
        headers: expect.objectContaining({ "X-AstrLink-Console": "1" }),
        body: JSON.stringify({ decision: "deny" }),
      }),
    );
    expect(fetchMock).toHaveBeenCalledTimes(2);
  });
  it.each([
    [423, "raw_locked"],
    [409, "tool_review_approval_not_pending"],
    [410, "tool_review_approval_not_pending"],
    [403, "tool_review_approval_forbidden"],
    [503, "tool_review_approval_unavailable"],
  ])("maps %s to a bounded safe error", async (status, code) => {
    fetchMock.mockResolvedValueOnce(
      response({ error: { code: "private server content" } }, Number(status)),
    );
    await expect(getToolReviewApproval(approval.id)).rejects.toThrow(
      String(code),
    );
    expect(fetchMock).toHaveBeenCalledTimes(1);
  });
  it("rejects a malformed payload and invalid local input without exposing content", async () => {
    fetchMock.mockResolvedValueOnce(
      response({ items: [{ ...item, user_request: "private" }] }),
    );
    await expect(listToolReviewApprovals()).rejects.toThrow(
      "tool_review_approval_unavailable",
    );
    fetchMock.mockClear();
    await expect(decideToolReviewApproval("../bad", "allow")).rejects.toThrow(
      "tool_review_approval_invalid",
    );
    expect(fetchMock).not.toHaveBeenCalled();
  });
});
