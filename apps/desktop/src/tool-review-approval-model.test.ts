import { describe, expect, it } from "vitest";
import {
  ApprovalTombstones,
  MAX_PENDING_TOOL_APPROVALS,
  parsePendingToolApprovals,
} from "./tool-review-approval-model";

describe("snapshot-scoped approval tombstones", () => {
  it("bounds history through many unique snapshots and releases completed IDs", () => {
    const history = new ApprovalTombstones();
    for (let round = 0; round < 128; round++) {
      const snapshot = Array.from(
        { length: MAX_PENDING_TOOL_APPROVALS },
        (_, index) => ({ id: `approval_${round}_${index}` }),
      );
      history.reconcile(snapshot);
      expect(history.size).toBe(0);
      snapshot.forEach((row) => history.add(row.id));
      expect(history.size).toBe(MAX_PENDING_TOOL_APPROVALS);
      // Disconnect/close produces no authoritative snapshot. Replaying the
      // same pending snapshot must preserve its existing terminal suppression.
      history.reconcile(snapshot);
      expect(history.size).toBe(MAX_PENDING_TOOL_APPROVALS);
      snapshot.forEach((row) => expect(history.has(row.id)).toBe(true));
      history.add("late_decision_for_retired_request");
      expect(history.size).toBe(MAX_PENDING_TOOL_APPROVALS);
      if (round > 0) expect(history.has(`approval_${round - 1}_0`)).toBe(false);
    }
    history.reconcile([]);
    expect(history.size).toBe(0);
    history.add("late_decision_for_retired_request");
    expect(history.size).toBe(0);
  });
  it("rejects oversized snapshots rather than retaining unbounded history", () => {
    const history = new ApprovalTombstones();
    expect(() =>
      history.reconcile(
        Array.from({ length: MAX_PENDING_TOOL_APPROVALS + 1 }, (_, index) => ({
          id: `approval_${index}`,
        })),
      ),
    ).toThrow("tool_review_approval_invalid");
    const requested_at = "2026-10-10T10:00:00Z",
      expires_at = "2026-10-10T10:02:00Z";
    const items = Array.from(
      { length: MAX_PENDING_TOOL_APPROVALS + 1 },
      (_, index) => {
        const id = `approval_${index}`;
        return {
          id,
          request_id: `req_${index}`,
          requested_at,
          expires_at,
          summary: {
            status: "pending",
            mode: "ask",
            model: "jev",
            config_revision: "r1",
            started_at: requested_at,
            latency_ms: 10,
            calls: [],
            approval: {
              id,
              state: "pending",
              review_status: "flagged",
              requested_at,
              expires_at,
            },
          },
        };
      },
    );
    expect(
      parsePendingToolApprovals({
        items: items.slice(0, MAX_PENDING_TOOL_APPROVALS),
      }),
    ).toHaveLength(MAX_PENDING_TOOL_APPROVALS);
    expect(() => parsePendingToolApprovals({ items })).toThrow(
      "tool_review_approval_invalid",
    );
  });
});
