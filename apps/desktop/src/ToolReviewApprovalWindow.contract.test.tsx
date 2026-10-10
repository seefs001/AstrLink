// @vitest-environment happy-dom
import { act } from "react";
import { createRoot, type Root } from "react-dom/client";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
const { invoke } = vi.hoisted(() => ({ invoke: vi.fn() }));
vi.mock("@tauri-apps/api/core", () => ({ invoke }));
vi.mock("@tauri-apps/api/event", () => ({
  listen: vi.fn(async () => () => {}),
}));
vi.mock("./edition", () => ({ isWebEdition: false }));
// Keep the real component, bridge, raw-status parser and approval parsers.
import { ToolReviewApprovalWindow } from "./ToolReviewApprovalWindow";
import { applyLocale } from "./i18n";

let root: Root;
let container: HTMLDivElement;
let batchCalls: unknown;
let backend: unknown;
function envelope() {
  const requested_at = new Date(Date.now() - 1000).toISOString();
  const expires_at = new Date(Date.now() + 120000).toISOString();
  return {
    items: [
      {
        id: "approval_contract",
        request_id: "req_contract",
        requested_at,
        expires_at,
        summary: {
          ...(backend === undefined ? {} : { backend }),
          status: "pending",
          mode: "ask",
          model: "jev",
          config_revision: "r1",
          ...(batchCalls === undefined ? {} : { batch_calls: batchCalls }),
          started_at: requested_at,
          latency_ms: 10,
          calls: [
            {
              index: 0,
              name: "run",
              verdict: "flag",
              model_verdict: "flag",
              reason_code: "destructive_action",
              flag_probability: 0.9,
              action: "pending",
              latency_ms: 10,
              model: "jev",
            },
          ],
          approval: {
            id: "approval_contract",
            state: "pending",
            review_status: "flagged",
            requested_at,
            expires_at,
          },
        },
      },
    ],
  };
}
beforeEach(async () => {
  Reflect.set(globalThis, "IS_REACT_ACT_ENVIRONMENT", true);
  Reflect.set(window, "__TAURI_INTERNALS__", {});
  await applyLocale("zh-CN");
  batchCalls = undefined;
  backend = undefined;
  invoke.mockReset();
  invoke.mockImplementation(async (command) => {
    switch (command) {
      case "list_tool_review_approvals":
        return envelope();
      case "raw_sealing_status":
        return {
          raw_available: true,
          configured: true,
          password_set: true,
          password_required: false,
          envelopes: ["password"],
          key_verified: true,
          unlocked: true,
          unlock_expires_at: null,
          unlock_idle_seconds: 300,
          retry_after_seconds: 0,
          password_min_length: 8,
          password_max_length: 128,
          key_replaced: false,
        };
      case "get_tool_review_approval":
        return {
          id: "approval_contract",
          request_id: "req_contract",
          user_request: "Synthetic user request",
          calls: [{ index: 0, name: "run", arguments: '{"synthetic":true}' }],
        };
      default:
        throw new Error("unexpected_fixture_command");
    }
  });
  container = document.createElement("div");
  document.body.append(container);
  root = createRoot(container);
});
afterEach(async () => {
  await act(async () => root.unmount());
  container.remove();
  Reflect.deleteProperty(window, "__TAURI_INTERNALS__");
});
describe("approval UI through the real native bridge and strict contract parsers", () => {
  it.each([
    [undefined, "未记录"],
    [false, "逐条"],
    [true, "整批"],
  ] as const)(
    "keeps summary.batch_calls=%s ready rather than offline",
    async (value, label) => {
      batchCalls = value;
      await act(async () => root.render(<ToolReviewApprovalWindow />));
      expect(
        container.querySelector('[data-testid="tool-review-approval-window"]'),
      ).not.toBeNull();
      expect(
        container.querySelector('[data-testid="tool-review-approval-primary"]'),
      ).not.toBeNull();
      expect(
        container.querySelector('[data-testid="tool-review-method"]')
          ?.textContent,
      ).toBe(`审查方式: ${label}`);
      expect(
        container.querySelector('[data-testid="tool-approval-plaintext"]'),
      ).not.toBeNull();
      expect(
        container.querySelector<HTMLButtonElement>('[data-decision="allow"]')
          ?.disabled,
      ).toBe(false);
      expect(container.textContent).toContain("等待人工决定");
      expect(container.textContent).toContain("待交付");
      expect(container.textContent).not.toContain("网关离线");
      expect(container.textContent).not.toContain("审批接口不可用");
      expect(invoke).toHaveBeenCalledWith("get_tool_review_approval", {
        approvalId: "approval_contract",
      });
      expect(
        invoke.mock.calls.some(
          ([command]) => command === "decide_tool_review_approval",
        ),
      ).toBe(false);
    },
  );
  it.each([
    ["jev", "Jev"],
    ["responses", "Responses 兼容"],
  ] as const)(
    "keeps %s backend compatible with batch review and approval through the real parser chain",
    async (value, label) => {
      backend = value;
      batchCalls = true;
      await act(async () => root.render(<ToolReviewApprovalWindow />));
      expect(
        container.querySelector('[data-testid="tool-approval-backend"]')
          ?.textContent,
      ).toContain(label);
      expect(
        container.querySelector('[data-testid="tool-review-backend-record"]')
          ?.textContent,
      ).toBe(`审查后端: ${label}`);
      expect(
        container.querySelector('[data-testid="tool-review-method"]')
          ?.textContent,
      ).toBe("审查方式: 整批");
      expect(
        container.querySelector<HTMLButtonElement>('[data-decision="allow"]')
          ?.disabled,
      ).toBe(false);
      expect(container.textContent).toContain("等待人工决定");
      expect(container.textContent).not.toContain("网关离线");
      expect(
        invoke.mock.calls.some(
          ([command]) => command === "decide_tool_review_approval",
        ),
      ).toBe(false);
    },
  );
  it("still fails closed for an invalid batch snapshot instead of silently discarding it", async () => {
    batchCalls = "true";
    await act(async () => root.render(<ToolReviewApprovalWindow />));
    expect(container.textContent).toContain("审批接口不可用");
    expect(container.querySelector('[data-decision="allow"]')).toBeNull();
    expect(
      container.querySelector('[data-testid="tool-approval-plaintext"]'),
    ).toBeNull();
    expect(
      invoke.mock.calls.some(
        ([command]) => command === "get_tool_review_approval",
      ),
    ).toBe(false);
  });
});
