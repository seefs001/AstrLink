// @vitest-environment happy-dom
import { act } from "react";
import { createRoot, type Root } from "react-dom/client";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import {
  TOOL_REVIEW_NAVIGATE_EVENT,
  parseToolReviewJump,
} from "./tool-review-navigation";
import { TrajectoryInspector } from "./TrajectoryInspector";
import {
  emptyTrajectoryFields,
  type RequestRecord,
} from "./request-record-model";
import { inspectorChainRows } from "./request-trajectory-model";
import type { ToolReviewSummary } from "./tool-review-record-model";

const record: RequestRecord = {
  ...emptyTrajectoryFields,
  id: "req_review",
  parent_request_id: null,
  attempt_index: 1,
  child_count: 0,
  started_at: "2026-10-09T10:00:00Z",
  completed_at: "2026-10-09T10:00:01Z",
  status: "succeeded",
  input_protocol: "openai.responses",
  requested_model: "test-model",
  streaming: true,
  route_id: null,
  service_id: null,
  local_access_token_id: null,
  http_status: 200,
  latency_ms: 1000,
  usage: null,
  error: null,
  audit: {
    request_body_captured: false,
    response_content_captured: false,
    request_body_truncated: false,
    response_content_truncated: false,
    upstream_request_body_captured: false,
    upstream_response_content_captured: false,
    upstream_request_body_truncated: false,
    upstream_response_content_truncated: false,
  },
  privacy_restore: null,
};
const summary: ToolReviewSummary = {
  status: "passed",
  mode: "mark",
  model: "jev-latest",
  config_revision: "r1",
  started_at: record.started_at,
  latency_ms: 10,
  calls: [
    {
      index: 0,
      name: "read_file",
      verdict: "pass",
      reason_code: "authorized",
      action: "delivered",
      latency_ms: 10,
      model: "jev-latest",
    },
  ],
};
let container: HTMLDivElement;
let root: Root;
beforeEach(() => {
  Reflect.set(globalThis, "IS_REACT_ACT_ENVIRONMENT", true);
  container = document.createElement("div");
  document.body.append(container);
  root = createRoot(container);
});
afterEach(async () => {
  await act(async () => root.unmount());
  container.remove();
});
async function render(
  current: RequestRecord,
  auditError: string | null = null,
) {
  await act(async () =>
    root.render(
      <TrajectoryInspector
        record={current}
        row={inspectorChainRows(current)[0]!}
        auditContent={null}
        auditLoading={false}
        auditError={auditError}
        copyFeedback={{ activeKey: null, state: "idle", copy: () => {} }}
      />,
    ),
  );
}
function reviewTab() {
  return container.querySelector<HTMLButtonElement>(
    '[data-testid="inspector-tab"][data-chip="TOOL_REVIEW"]',
  );
}
it.each(["delivered", "withheld", "pending"] as const)(
  "keeps the locate entry for %s calls and sends only exact navigation identity",
  async (action) => {
    const current = {
      ...record,
      session_id: "session_review",
      tool_review: { ...summary, calls: [{ ...summary.calls[0], action }] },
    };
    await render(current);
    await act(async () => reviewTab()!.click());
    const receive = vi.fn();
    window.addEventListener(TOOL_REVIEW_NAVIGATE_EVENT, receive);
    try {
      const locate = container.querySelector<HTMLButtonElement>(
        '[data-testid="tool-review-locate"]',
      )!;
      expect(locate).not.toBeNull();
      await act(async () => locate.click());
      await act(async () => locate.click());
      expect(receive).toHaveBeenCalledTimes(2);
      const first = (receive.mock.calls[0][0] as CustomEvent).detail;
      expect(parseToolReviewJump(first)).toEqual(first);
      expect(first).toMatchObject({
        requestId: record.id,
        sessionId: "session_review",
        reviewIndex: 0,
      });
      expect(first.nonce).not.toBe(
        (receive.mock.calls[1][0] as CustomEvent).detail.nonce,
      );
    } finally {
      window.removeEventListener(TOOL_REVIEW_NAVIGATE_EVENT, receive);
    }
  },
);

it.each([
  [undefined, false],
  [{ ...summary, status: "not_applicable" }, false],
  [{ ...summary, status: "pending", calls: [] }, false],
  [{ ...summary, status: "pending" }, true],
  [{ ...summary, status: "failed", calls: [] }, true],
  [{ ...summary, status: "interrupted", calls: [] }, true],
  [{ ...summary, status: "cancelled", calls: [] }, true],
] satisfies Array<[ToolReviewSummary | undefined, boolean]>)(
  "shows a review tab only for real review metadata (%j)",
  async (tool_review, visible) => {
    await render({ ...record, tool_review });
    expect(!!reviewTab()).toBe(visible);
    expect(
      container.querySelector('[data-testid="tool-review-details"]'),
    ).toBeNull();
    if (visible) {
      await act(async () => reviewTab()!.click());
      expect(
        container.querySelectorAll('[data-testid="tool-review-details"]'),
      ).toHaveLength(1);
      expect(
        container.querySelector('[data-testid="inspector-section"]'),
      ).toBeNull();
      const primary = container.querySelector<HTMLElement>(
        '[data-testid="trajectory-inspector-primary"]',
      )!;
      expect(primary.className).toContain("overflow-y-auto");
      expect(primary.textContent).not.toContain("工具审查");
    }
  },
);
it("keeps review in the existing tab row, preserves the chosen tab on polls and shows only the current request", async () => {
  await render({ ...record, tool_review: summary }, "raw body unavailable");
  const normalTabs = Array.from(
    container.querySelectorAll<HTMLButtonElement>(
      '[data-testid="inspector-tab"]',
    ),
  ).filter((tab) => tab.dataset.chip !== "TOOL_REVIEW");
  expect(normalTabs.length).toBeGreaterThan(0);
  expect(container.querySelectorAll('[role="tablist"]')).toHaveLength(1);
  expect(reviewTab()?.querySelector("svg")).not.toBeNull();
  await act(async () => reviewTab()!.click());
  expect(container.textContent).not.toContain("raw body unavailable");
  expect(container.textContent).toContain("符合审查规则");
  expect(reviewTab()?.getAttribute("aria-selected")).toBe("true");
  await render({ ...record, tool_review: { ...summary, latency_ms: 20 } });
  expect(reviewTab()?.getAttribute("aria-selected")).toBe("true");
  expect(
    container.querySelectorAll('[data-testid="tool-review-details"]'),
  ).toHaveLength(1);
  await act(async () => normalTabs[0].click());
  expect(
    container.querySelector('[data-testid="tool-review-details"]'),
  ).toBeNull();
  await act(async () => reviewTab()!.click());
  await render({
    ...record,
    id: "req_other",
    tool_review: {
      ...summary,
      calls: [{ ...summary.calls[0], name: "other_call" }],
    },
  });
  expect(reviewTab()?.getAttribute("aria-selected")).toBe("false");
  expect(
    container.querySelector('[data-testid="tool-review-details"]'),
  ).toBeNull();
  await act(async () => reviewTab()!.click());
  expect(container.textContent).toContain("other_call");
  expect(container.textContent).not.toContain("read_file");
});
it("returns to an ordinary phase when review metadata disappears and does not reopen it on reappearance", async () => {
  await render({ ...record, tool_review: summary });
  await act(async () => reviewTab()!.click());
  await render({
    ...record,
    tool_review: { ...summary, status: "not_applicable", calls: [] },
  });
  expect(reviewTab()).toBeNull();
  expect(
    container.querySelector('[data-testid="tool-review-details"]'),
  ).toBeNull();
  expect(
    container.querySelectorAll('[role="tab"][aria-selected="true"]'),
  ).toHaveLength(1);
  await render({ ...record, tool_review: summary });
  expect(reviewTab()?.getAttribute("aria-selected")).toBe("false");
  expect(
    container.querySelector('[data-testid="tool-review-details"]'),
  ).toBeNull();
});
