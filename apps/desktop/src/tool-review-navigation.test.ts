// @vitest-environment happy-dom
import { beforeEach, describe, expect, it, vi } from "vitest";

const host = vi.hoisted(() => ({
  label: null as string | null,
  invoke: vi.fn(),
  emitTo: vi.fn(),
}));
vi.mock("@tauri-apps/api/core", () => ({
  invoke: host.invoke,
  isTauri: () => true,
}));
vi.mock("@tauri-apps/api/event", () => ({ emitTo: host.emitTo }));
vi.mock("@tauri-apps/api/window", () => ({
  getCurrentWindow: () => ({ label: host.label }),
}));
vi.mock("./window-chrome", () => ({ getDesktopPlatform: () => "macos" }));

import {
  canLocateReviewedTool,
  navigateToReviewedTool,
  parseToolReviewJump,
  TOOL_REVIEW_NAVIGATE_EVENT,
  type ToolReviewJump,
} from "./tool-review-navigation";
import type { RequestRecord } from "./request-record-model";

const jump: ToolReviewJump = {
  version: 1,
  target: "main",
  sessionId: "session_1",
  requestId: "req_1",
  reviewIndex: 1,
  nonce: "nonce_1",
};
const record = {
  id: jump.requestId,
  session_id: jump.sessionId,
  status: "succeeded",
  tool_review: { calls: [{ index: 1, action: "delivered" }] },
} as RequestRecord;

beforeEach(() => {
  host.label = null;
  host.invoke.mockReset().mockResolvedValue(undefined);
  host.emitTo.mockReset().mockResolvedValue(undefined);
});

describe("tool review navigation envelope", () => {
  it("accepts only the exact versioned metadata envelope", () => {
    expect(parseToolReviewJump(jump)).toEqual(jump);
    for (const value of [
      null,
      [],
      "body",
      {},
      { ...jump, body: "private" },
      { ...jump, version: 2 },
      { ...jump, target: "inspector" },
      { ...jump, sessionId: "../session" },
      { ...jump, requestId: "" },
      { ...jump, nonce: "n".repeat(193) },
      { ...jump, reviewIndex: -1 },
      { ...jump, reviewIndex: 256 },
      { ...jump, reviewIndex: 0.5 },
      { ...jump, reviewIndex: "1" },
      { ...jump, reviewIndex: NaN },
    ]) {
      expect(parseToolReviewJump(value)).toBeNull();
    }
  });

  it("validates identity and a unique safe index, not delivery or body access", () => {
    for (const action of ["pending", "withheld", "delivered"] as const) {
      expect(
        canLocateReviewedTool(
          {
            ...record,
            status: "pending",
            tool_review: {
              ...record.tool_review!,
              calls: [{ ...record.tool_review!.calls[0], action }],
            },
          },
          1,
        ),
      ).toBe(true);
    }
    expect(canLocateReviewedTool(record, 0)).toBe(false);
    expect(canLocateReviewedTool({ ...record, session_id: null }, 1)).toBe(
      false,
    );
    expect(
      canLocateReviewedTool(
        {
          ...record,
          tool_review: {
            ...record.tool_review!,
            calls: [record.tool_review!.calls[0], record.tool_review!.calls[0]],
          },
        },
        1,
      ),
    ).toBe(false);
  });

  it.each([null, "main"])(
    "uses same-page DOM navigation with host label %s even when isTauri is true",
    async (label) => {
      host.label = label;
      const receive = vi.fn();
      window.addEventListener(TOOL_REVIEW_NAVIGATE_EVENT, receive);
      try {
        await navigateToReviewedTool(record, 1);
        await navigateToReviewedTool(record, 1);
        const first = (receive.mock.calls[0][0] as CustomEvent).detail;
        const second = (receive.mock.calls[1][0] as CustomEvent).detail;
        expect(parseToolReviewJump(first)).toEqual(first);
        expect(first.nonce).not.toBe(second.nonce);
        expect(host.invoke).not.toHaveBeenCalled();
        expect(host.emitTo).not.toHaveBeenCalled();
      } finally {
        window.removeEventListener(TOOL_REVIEW_NAVIGATE_EVENT, receive);
      }
    },
  );

  it("awaits inspector-only focus before sending metadata to main", async () => {
    host.label = "trajectory-inspector-7";
    let focused!: () => void;
    host.invoke.mockReturnValue(
      new Promise<void>((resolve) => {
        focused = resolve;
      }),
    );
    const navigating = navigateToReviewedTool(record, 1);
    expect(host.invoke).toHaveBeenCalledExactlyOnceWith("focus_main_window");
    expect(host.emitTo).not.toHaveBeenCalled();
    focused();
    await navigating;
    expect(host.emitTo).toHaveBeenCalledTimes(1);
    const [target, event, payload] = host.emitTo.mock.calls[0];
    expect(target).toBe("main");
    expect(event).toBe(TOOL_REVIEW_NAVIGATE_EVENT);
    expect(parseToolReviewJump(payload)).toEqual(payload);
    expect(Object.keys(payload).sort()).toEqual(Object.keys(jump).sort());
    expect(payload).toMatchObject({
      sessionId: "session_1",
      requestId: "req_1",
      reviewIndex: 1,
    });
  });

  it("does not navigate if native focus fails", async () => {
    host.label = "trajectory-inspector-1";
    host.invoke.mockRejectedValue(new Error("focus failed"));
    await expect(navigateToReviewedTool(record, 1)).rejects.toThrow(
      "focus failed",
    );
    expect(host.emitTo).not.toHaveBeenCalled();
  });
});
