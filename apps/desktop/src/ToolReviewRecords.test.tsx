// @vitest-environment happy-dom
import { act } from "react";
import { createRoot, type Root } from "react-dom/client";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
const invoke = vi.hoisted(() => vi.fn());
vi.mock("@tauri-apps/api/core", () => ({ invoke }));
import { ToolReviewRecords } from "./ToolReviewRecords";
import { settleClosedLayer } from "./lib/test-layer-settle";

function record(id: string, status = "pending") {
  return {
    id,
    started_at: "2026-10-09T10:00:00Z",
    completed_at: "2026-10-09T10:00:01Z",
    status: "succeeded",
    input_protocol: "openai.responses",
    requested_model: "model",
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
    },
    privacy_restore: null,
    tool_review: {
      status,
      mode: "mark",
      model: "jev-latest",
      config_revision: "r1",
      started_at: "2026-10-09T10:00:00Z",
      latency_ms: 10,
      calls: [
        {
          index: 0,
          name: "shell",
          verdict: status === "pending" ? "error" : "flag",
          action: "delivered",
          latency_ms: 10,
          model: "jev-latest",
        },
      ],
    },
  };
}
let container: HTMLDivElement;
let root: Root;
async function click(label: string) {
  const button = Array.from(document.querySelectorAll("button")).find(
    (button) =>
      button.textContent?.trim() === label ||
      button.getAttribute("aria-label") === label,
  );
  if (!button) throw new Error(`Missing ${label}`);
  await act(async () => button.click());
}
async function chooseStatus(label: string) {
  await act(async () =>
    container.querySelector('[aria-label="审查结果"]')!.dispatchEvent(
      new PointerEvent("pointerdown", {
        bubbles: true,
        button: 0,
        pointerType: "mouse",
      }),
    ),
  );
  expect(document.body.textContent).not.toContain("未审查");
  const option = Array.from(
    document.querySelectorAll<HTMLElement>('[role="option"]'),
  ).find((item) => item.textContent === label)!;
  await act(async () => option.click());
  await settleClosedLayer();
}
beforeEach(() => {
  Reflect.set(globalThis, "IS_REACT_ACT_ENVIRONMENT", true);
  Reflect.set(window, "__TAURI_INTERNALS__", {});
  invoke.mockReset();
  container = document.createElement("div");
  document.body.append(container);
  root = createRoot(container);
});
afterEach(async () => {
  await act(async () => root.unmount());
  container.remove();
  vi.useRealTimers();
  vi.restoreAllMocks();
  Reflect.deleteProperty(window, "__TAURI_INTERNALS__");
});
describe("ToolReviewRecords", () => {
  it("filters historical placeholders without hiding zero-call failures", async () => {
    invoke.mockResolvedValue({
      items: [
        "not_applicable",
        "pending",
        "failed",
        "interrupted",
        "cancelled",
      ].map((status) => {
        const item = record(status, status);
        item.tool_review.calls = [];
        return item;
      }),
      next_cursor: null,
    });
    await act(async () =>
      root.render(<ToolReviewRecords coreSessionKey="test" isReady />),
    );
    const rows = container.querySelectorAll<HTMLButtonElement>(
      '[data-testid="tool-review-record-row"]',
    );
    expect(Array.from(rows, (row) => row.dataset.recordId)).toEqual([
      "failed",
      "interrupted",
      "cancelled",
    ]);
    expect(container.textContent).not.toContain("未审查");
    expect(
      Array.from(
        container.querySelectorAll('[data-testid="tool-review-badge"]'),
        (badge) => badge.textContent,
      ),
    ).not.toContain("审查中");
    await act(async () => rows[0].click());
    expect(
      container.querySelector('[data-testid="tool-review-details"]')
        ?.textContent,
    ).toContain("审查失败");
  });
  it("queries review filters on the server and carries them through pagination", async () => {
    invoke.mockImplementation(async (_command, { query }) => ({
      items: [
        record(
          query.cursor ? "req_old" : "req_new",
          query.tool_review_status ?? "pending",
        ),
      ],
      next_cursor: query.cursor ? null : "older",
    }));
    await act(async () =>
      root.render(<ToolReviewRecords coreSessionKey="test" isReady />),
    );
    expect(invoke).toHaveBeenLastCalledWith("list_request_records", {
      query: { limit: 50, tool_review_only: true },
    });
    await chooseStatus("拒绝（仅记录）");
    expect(invoke).toHaveBeenLastCalledWith("list_request_records", {
      query: {
        limit: 50,
        tool_review_only: true,
        tool_review_status: "flagged",
      },
    });
    await click("加载更早记录");
    expect(invoke).toHaveBeenLastCalledWith("list_request_records", {
      query: {
        limit: 50,
        tool_review_only: true,
        tool_review_status: "flagged",
        cursor: "older",
      },
    });
    expect(
      container.querySelectorAll('[data-testid="tool-review-record-row"]'),
    ).toHaveLength(2);
    expect(container.textContent).not.toContain("完成");
    expect(container.textContent).toContain("1 个工具调用");
    expect(container.textContent).toContain("拒绝（仅记录）");
  });
  it("returns to the selected row without losing list scroll or keyboard focus", async () => {
    invoke.mockResolvedValue({
      items: [record("req_one", "flagged"), record("req_two", "passed")],
      next_cursor: null,
    });
    await act(async () =>
      root.render(<ToolReviewRecords coreSessionKey="test" isReady />),
    );
    const scroller = container.querySelector<HTMLElement>(
      '[data-testid="tool-review-records-primary"]',
    )!;
    scroller.scrollTop = 80;
    await act(async () =>
      container
        .querySelector<HTMLButtonElement>('[data-record-id="req_two"]')!
        .click(),
    );
    expect(scroller.scrollTop).toBe(0);
    await click("返回列表");
    expect(scroller.scrollTop).toBe(80);
    const selected = container.querySelector<HTMLButtonElement>(
      '[data-record-id="req_two"]',
    )!;
    expect(document.activeElement).toBe(selected);
    expect(selected.dataset.state).toBe("selected");
  });
  it("sends nothing while hidden and refreshes once when visible again", async () => {
    let visibility: DocumentVisibilityState = "visible";
    vi.spyOn(document, "visibilityState", "get").mockImplementation(
      () => visibility,
    );
    let release: (() => void) | null = null;
    invoke.mockImplementation(async () => ({
      items: [record("req_new")],
      next_cursor: null,
    }));
    vi.useFakeTimers();
    await act(async () =>
      root.render(<ToolReviewRecords coreSessionKey="test" isReady />),
    );
    expect(invoke).toHaveBeenCalledTimes(1);
    visibility = "hidden";
    document.dispatchEvent(new Event("visibilitychange"));
    // The timer armed before hiding fires but sends no list or detail read.
    await act(async () => {
      await vi.advanceTimersByTimeAsync(20_000);
    });
    expect(invoke).toHaveBeenCalledTimes(1);
    invoke.mockImplementation(
      () =>
        new Promise((resolve) => {
          release = () => resolve({ items: [], next_cursor: null });
        }),
    );
    visibility = "visible";
    await act(async () => {
      document.dispatchEvent(new Event("visibilitychange"));
      document.dispatchEvent(new Event("visibilitychange"));
    });
    // Two visibility events while the refresh runs start only one request.
    expect(invoke).toHaveBeenCalledTimes(2);
    await act(async () => release?.());
  });
  it("refreshes a successful request's pending review and removes it from a pending filter", async () => {
    let complete = false;
    invoke.mockImplementation(async (command, args) =>
      command === "get_request_record"
        ? record("req_new", complete ? "flagged" : "pending")
        : {
            items:
              complete && args.query.tool_review_status === "pending"
                ? []
                : [record("req_new", complete ? "flagged" : "pending")],
            next_cursor: null,
          },
    );
    await act(async () =>
      root.render(<ToolReviewRecords coreSessionKey="test" isReady />),
    );
    await chooseStatus("审查中");
    // Start fake time after Radix has restored focus, then restart the poll
    // under that clock through the ordinary refresh action.
    vi.useFakeTimers();
    await click("刷新");
    await act(async () =>
      container
        .querySelector<HTMLButtonElement>(
          '[data-testid="tool-review-record-row"]',
        )!
        .click(),
    );
    complete = true;
    await act(async () => {
      await vi.advanceTimersByTimeAsync(2000);
    });
    expect(
      container.querySelector('[data-testid="tool-review-details"]')
        ?.textContent,
    ).toContain("拒绝（仅记录）");
    await click("返回列表");
    expect(
      container.querySelectorAll('[data-testid="tool-review-record-row"]'),
    ).toHaveLength(0);
  });
});
