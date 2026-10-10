// @vitest-environment happy-dom
import { act } from "react";
import { createRoot, type Root } from "react-dom/client";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import type {
  PendingToolApproval,
  ToolApprovalDetail,
} from "./tool-review-approval-model";
import { settleClosedLayer } from "./lib/test-layer-settle";
const mocks = vi.hoisted(() => ({
  list: vi.fn(),
  detail: vi.fn(),
  decide: vi.fn(),
  raw: vi.fn(),
  rawListener: vi.fn(),
  nativeListener: vi.fn(),
  unlock: vi.fn(),
  acknowledge: vi.fn(),
  setPassword: vi.fn(),
}));
vi.mock("./bridge", () => ({
  listToolReviewApprovals: mocks.list,
  getToolReviewApproval: mocks.detail,
  decideToolReviewApproval: mocks.decide,
  getRawSealingStatus: mocks.raw,
  listenRawSealingChanged: mocks.rawListener,
  listenToolReviewApprovalState: mocks.nativeListener,
  unlockRaw: mocks.unlock,
  acknowledgeRawKey: mocks.acknowledge,
  setRawPassword: mocks.setPassword,
}));
vi.mock("./edition", () => ({ isWebEdition: false }));
import {
  ToolReviewApprovalWindow,
  TOOL_APPROVAL_POLL_MS,
} from "./ToolReviewApprovalWindow";
function row(id = "approval_1", expires = 120000): PendingToolApproval {
  const requested_at = new Date(Date.now() - 1000).toISOString();
  const expires_at = new Date(Date.now() + expires).toISOString();
  return {
    id,
    request_id: `req_${id}`,
    requested_at,
    expires_at,
    summary: {
      mode: "ask",
      status: "pending",
      model: "jev",
      config_revision: "r1",
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
        id,
        requested_at,
        expires_at,
        state: "pending",
        review_status: "flagged",
      },
    },
  };
}
function detail(id = "approval_1"): ToolApprovalDetail {
  return {
    id,
    request_id: `req_${id}`,
    user_request: `private request ${id}`,
    calls: [{ index: 0, name: "run", arguments: `private args ${id}` }],
  };
}
let root: Root;
let container: HTMLDivElement;
let rawChanged: () => void;
let hostChanged: (status: "loading" | "ready" | "offline") => void;
function decision(value: string) {
  return container.querySelector<HTMLButtonElement>(
    `[data-decision="${value}"]`,
  );
}
async function tick(ms = TOOL_APPROVAL_POLL_MS) {
  await act(async () => {
    await vi.advanceTimersByTimeAsync(ms);
  });
}
async function render() {
  await act(async () => root.render(<ToolReviewApprovalWindow />));
}
function deferred<T>() {
  let resolve!: (value: T) => void;
  let reject!: (value: unknown) => void;
  const promise = new Promise<T>((yes, no) => {
    resolve = yes;
    reject = no;
  });
  return { promise, resolve, reject };
}
beforeEach(() => {
  vi.useFakeTimers();
  Reflect.set(globalThis, "IS_REACT_ACT_ENVIRONMENT", true);
  Object.values(mocks).forEach((mock) => mock.mockReset());
  mocks.list.mockResolvedValue([row()]);
  mocks.raw.mockResolvedValue({
    unlocked: true,
    password_set: true,
    key_replaced: false,
    unlock_expires_at: null,
  });
  mocks.detail.mockImplementation(async (id) => detail(id));
  mocks.decide.mockResolvedValue({ state: "approved" });
  mocks.rawListener.mockImplementation(async (callback) => {
    rawChanged = callback;
    return vi.fn();
  });
  mocks.nativeListener.mockImplementation(async (callback) => {
    hostChanged = callback;
    return vi.fn();
  });
  container = document.createElement("div");
  document.body.append(container);
  root = createRoot(container);
});
afterEach(async () => {
  await act(async () => root.unmount());
  container.remove();
  vi.useRealTimers();
});
describe("live tool approval queue", () => {
  it("shows review separately, never autofocuses or approves on opening/Enter, closes without decision", async () => {
    await render();
    const windowRoot = container.querySelector(
      '[data-testid="tool-review-approval-window"]',
    );
    expect(windowRoot).not.toBeNull();
    expect(
      windowRoot?.querySelector('[data-testid="tool-review-approval-primary"]'),
    ).not.toBeNull();
    expect(container.textContent).toContain("等待人工决定");
    expect(container.textContent).toContain("private args");
    const allow = decision("allow")!;
    expect(document.activeElement).not.toBe(allow);
    expect(allow.hasAttribute("autofocus")).toBe(false);
    expect(container.querySelector("form")).toBeNull();
    const event = new KeyboardEvent("keydown", {
      key: "Enter",
      bubbles: true,
      cancelable: true,
    });
    allow.dispatchEvent(event);
    expect(event.defaultPrevented).toBe(true);
    expect(mocks.decide).not.toHaveBeenCalled();
  });
  it("keeps metadata when raw is locked, uses explicit unlock, never fetches or enables allow", async () => {
    mocks.raw.mockResolvedValue({
      unlocked: false,
      password_set: true,
      key_replaced: false,
      unlock_expires_at: null,
    });
    await render();
    expect(container.textContent).toContain("req_approval_1");
    expect(mocks.detail).not.toHaveBeenCalled();
    expect(decision("allow")?.disabled).toBe(true);
    const unlock = Array.from(container.querySelectorAll("button")).find(
      (node) => node.textContent === "解锁原文",
    )!;
    await act(async () => unlock.click());
    expect(document.querySelector('[role="alertdialog"]')).not.toBeNull();
    expect(mocks.decide).not.toHaveBeenCalled();
  });
  it("unlocks through the existing password dialog without approving or autofocusing allow", async () => {
    const locked = {
      unlocked: false,
      password_set: true,
      key_replaced: false,
      unlock_expires_at: null,
      unlock_idle_seconds: 300,
    };
    mocks.raw.mockResolvedValue(locked);
    mocks.unlock.mockImplementation(async () => {
      const next = { ...locked, unlocked: true };
      mocks.raw.mockResolvedValue(next);
      return { outcome: "sealing", status: next };
    });
    await render();
    const unlock = Array.from(container.querySelectorAll("button")).find(
      (node) => node.textContent === "解锁原文",
    )!;
    await act(async () => unlock.click());
    const input = document.querySelector<HTMLInputElement>(
      "#raw-current-password",
    )!;
    await act(async () => {
      Object.getOwnPropertyDescriptor(
        HTMLInputElement.prototype,
        "value",
      )!.set!.call(input, "fixture-password");
      input.dispatchEvent(new Event("input", { bubbles: true }));
    });
    const submit = Array.from(
      document.querySelectorAll<HTMLButtonElement>(
        '[role="alertdialog"] button',
      ),
    ).find((node) => node.textContent === "解锁")!;
    await act(async () => submit.click());
    const settled = settleClosedLayer();
    await vi.advanceTimersByTimeAsync(0);
    await settled;
    expect(mocks.unlock).toHaveBeenCalledExactlyOnceWith({
      kind: "password",
      password: "fixture-password",
    });
    expect(container.textContent).toContain("private args");
    expect(document.activeElement).not.toBe(decision("allow"));
    expect(mocks.decide).not.toHaveBeenCalled();
  });
  it("revokes allow synchronously before a relock render can commit", async () => {
    await render();
    const allow = decision("allow")!;
    mocks.raw.mockResolvedValue({
      unlocked: false,
      password_set: true,
      key_replaced: false,
      unlock_expires_at: null,
    });
    await act(async () => {
      rawChanged();
      allow.click();
    });
    expect(mocks.decide).not.toHaveBeenCalled();
    expect(container.textContent).not.toContain("private args");
  });
  it("purges plaintext on raw lock including a late detail response", async () => {
    const late = deferred<ToolApprovalDetail>();
    mocks.detail.mockReturnValue(late.promise);
    await render();
    mocks.raw.mockResolvedValue({
      unlocked: false,
      password_set: true,
      key_replaced: false,
      unlock_expires_at: null,
    });
    await act(async () => rawChanged());
    await act(async () => late.resolve(detail()));
    expect(container.textContent).not.toContain("private");
    expect(decision("allow")?.disabled).toBe(true);
  });
  it("scopes details to each request and suppresses a stale selection response", async () => {
    mocks.list.mockResolvedValue([row(), row("approval_2")]);
    const late = deferred<ToolApprovalDetail>();
    mocks.detail.mockImplementation((id) =>
      id === "approval_1" ? late.promise : Promise.resolve(detail(id)),
    );
    await render();
    const select = Array.from(container.querySelectorAll("button")).find(
      (node) => node.textContent === "req_approval_2",
    )!;
    await act(async () => select.click());
    await act(async () => late.resolve(detail()));
    expect(container.textContent).toContain("private args approval_2");
    expect(container.textContent).not.toContain("private args approval_1");
  });
  it("purges on expiration and on server disappearance, cannot act after timeout", async () => {
    mocks.list.mockResolvedValue([row("approval_1", 1000)]);
    await render();
    await tick(1000);
    expect(decision("allow")).toBeNull();
    expect(container.textContent).not.toContain("private args");
    mocks.list.mockResolvedValue([row("approval_2")]);
    await tick();
    expect(container.textContent).toContain("private args approval_2");
    mocks.list.mockResolvedValue([]);
    await tick();
    expect(container.textContent).not.toContain("private args");
    expect(decision("deny")).toBeNull();
  });
  it("clears immediately on host offline and on poll errors, recovers without stale data", async () => {
    await render();
    await act(async () => hostChanged("offline"));
    expect(container.textContent).not.toContain("private args");
    expect(decision("deny")).toBeNull();
    mocks.list.mockRejectedValue(new Error("sensitive upstream text"));
    await tick();
    expect(container.textContent).not.toContain("sensitive upstream text");
    expect(decision("allow")).toBeNull();
    mocks.list.mockResolvedValue([row("approval_2")]);
    await tick();
    expect(container.textContent).toContain("private args approval_2");
  });
  it.each(["allow", "deny"] as const)(
    "serializes %s and waits for server before removing a request",
    async (action) => {
      const result = deferred<unknown>();
      mocks.decide.mockReturnValue(result.promise);
      mocks.list.mockResolvedValue([row(), row("approval_2")]);
      await render();
      await act(async () => {
        decision(action)!.click();
        decision(action)!.click();
      });
      expect(mocks.decide).toHaveBeenCalledExactlyOnceWith(
        "approval_1",
        action,
      );
      expect(container.textContent).not.toContain("private args");
      expect(container.textContent).toContain("req_approval_1");
      await act(async () =>
        result.resolve({ state: action === "allow" ? "approved" : "denied" }),
      );
      expect(container.textContent).toContain("private args approval_2");
      expect(container.textContent).not.toContain("req_approval_1");
    },
  );
  it.each(["allow", "deny"] as const)(
    "handles failed %s without optimistic success or leaked errors",
    async (action) => {
      mocks.decide.mockRejectedValue(new Error("private error"));
      mocks.list
        .mockResolvedValueOnce([row()])
        .mockRejectedValue(new Error("offline"));
      await render();
      await act(async () => decision(action)!.click());
      expect(container.textContent).not.toContain("private");
      expect(decision("allow")).toBeNull();
      expect(container.textContent).toContain("无法读取或更新审批");
    },
  );
  it("ignores a single-flight pre-decision snapshot instead of pruning tombstones or removing other requests", async () => {
    const late = deferred<PendingToolApproval[]>();
    mocks.list
      .mockResolvedValueOnce([row(), row("approval_2")])
      .mockReturnValueOnce(late.promise)
      .mockResolvedValue([row("approval_2")]);
    await render();
    await tick();
    expect(mocks.list).toHaveBeenCalledTimes(2);
    await act(async () => decision("deny")!.click());
    expect(container.textContent).toContain("private args approval_2");
    await act(async () => late.resolve([]));
    // The old empty snapshot cannot remove request 2 or prune request 1's tombstone.
    expect(container.textContent).toContain("private args approval_2");
    expect(container.textContent).not.toContain("req_approval_1");
    await tick();
    expect(container.textContent).toContain("private args approval_2");
    expect(mocks.decide).toHaveBeenCalledExactlyOnceWith("approval_1", "deny");
  });
  it("ignores pre-decision lists even when they finish while the decision is still in flight", async () => {
    const lateList = deferred<PendingToolApproval[]>();
    const post = deferred<unknown>();
    mocks.list
      .mockResolvedValueOnce([row(), row("approval_2")])
      .mockReturnValueOnce(lateList.promise)
      .mockResolvedValue([row("approval_2")]);
    mocks.decide.mockReturnValue(post.promise);
    await render();
    await tick();
    await act(async () => decision("deny")!.click());
    await act(async () => lateList.resolve([]));
    expect(container.textContent).toContain("req_approval_1");
    expect(container.textContent).toContain("req_approval_2");
    expect(container.textContent).not.toContain("private args");
    await act(async () => post.resolve({ state: "denied" }));
    expect(container.textContent).not.toContain("req_approval_1");
    expect(container.textContent).toContain("private args approval_2");
  });
  it("retains settled suppression across offline and replayed pending snapshots", async () => {
    mocks.list.mockResolvedValue([row(), row("approval_2")]);
    await render();
    await act(async () => decision("deny")!.click());
    expect(container.textContent).not.toContain("req_approval_1");
    await act(async () => hostChanged("offline"));
    mocks.list.mockRejectedValue(new Error("offline"));
    await tick();
    mocks.list.mockResolvedValue([row(), row("approval_2")]);
    await tick();
    expect(container.textContent).not.toContain("req_approval_1");
    expect(container.textContent).toContain("private args approval_2");
    expect(mocks.decide).toHaveBeenCalledTimes(1);
  });
  it("shows eight raw argument sets before the shared collapsed review details", async () => {
    const request = row();
    request.summary.calls = Array.from({ length: 8 }, (_, index) => ({
      ...request.summary.calls[0],
      index,
      name: `run_${index}`,
    }));
    mocks.list.mockResolvedValue([request]);
    mocks.detail.mockResolvedValue({
      ...detail(),
      calls: request.summary.calls.map((call) => ({
        index: call.index,
        name: call.name,
        arguments: `raw arguments ${call.index}`,
      })),
    });
    await render();
    const plaintext = container.querySelector(
      '[data-testid="tool-approval-plaintext"]',
    )!;
    const disclosure = container.querySelector(
      '[data-testid="tool-approval-review-disclosure"]',
    )!;
    expect(
      plaintext.compareDocumentPosition(disclosure) &
        Node.DOCUMENT_POSITION_FOLLOWING,
    ).toBe(Node.DOCUMENT_POSITION_FOLLOWING);
    expect(disclosure.querySelector("details")?.open).toBe(false);
    expect(plaintext.textContent).toContain("raw arguments 0");
    expect(plaintext.textContent).toContain("raw arguments 7");
    expect(decision("allow")?.disabled).toBe(false);
  });
  it("recovers a failed decision with a fresh pending snapshot without pretending approval or losing details", async () => {
    mocks.decide.mockRejectedValue(
      new Error("tool_review_approval_unavailable"),
    );
    mocks.list.mockResolvedValue([row(), row("approval_2")]);
    await render();
    await act(async () => decision("allow")!.click());
    expect(container.textContent).toContain("req_approval_1");
    expect(container.textContent).toContain("req_approval_2");
    expect(container.textContent).toContain("等待人工决定");
    expect(container.textContent).not.toContain("操作员已批准");
    expect(container.textContent).toContain("private args approval_1");
    expect(decision("allow")?.disabled).toBe(false);
  });
  it("handles late decisions as removed, not successful allows", async () => {
    mocks.decide.mockRejectedValue(
      new Error("tool_review_approval_not_pending"),
    );
    await render();
    await act(async () => decision("allow")!.click());
    expect(decision("allow")).toBeNull();
    expect(container.textContent).not.toContain("private args");
    expect(container.textContent).toContain("此审批已不再等待决定");
  });
  it("explicitly rejects unavailable operator access without fake empty success", async () => {
    mocks.list.mockRejectedValue(new Error("tool_review_approval_forbidden"));
    await render();
    expect(container.textContent).toContain("审批需要操作员权限");
    expect(container.textContent).not.toContain("暂无待审批");
    expect(decision("allow")).toBeNull();
    expect(mocks.detail).not.toHaveBeenCalled();
  });
  it("fails closed on a stalled poll and ignores its late lock response", async () => {
    const late = deferred<unknown>();
    mocks.raw
      .mockResolvedValueOnce({
        unlocked: true,
        password_set: true,
        key_replaced: false,
        unlock_expires_at: null,
      })
      .mockReturnValue(late.promise);
    await render();
    await tick(7000);
    expect(decision("allow")).toBeNull();
    expect(container.textContent).not.toContain("private args");
    expect(mocks.list).toHaveBeenCalledTimes(2);
    await act(async () =>
      late.resolve({
        unlocked: true,
        password_set: true,
        key_replaced: false,
        unlock_expires_at: null,
      }),
    );
    expect(decision("allow")).toBeNull();
  });
  it("reloads on close/reopen without retaining the old pending plaintext", async () => {
    const late = deferred<ToolApprovalDetail>();
    mocks.detail.mockReturnValue(late.promise);
    await render();
    await act(async () => root.unmount());
    root = createRoot(container);
    mocks.list.mockResolvedValue([row("approval_2")]);
    mocks.detail.mockResolvedValue(detail("approval_2"));
    await render();
    await act(async () => late.resolve(detail()));
    expect(container.textContent).toContain("private args approval_2");
    expect(container.textContent).not.toContain("private args approval_1");
    expect(mocks.decide).not.toHaveBeenCalled();
  });
  it("checks unlock expiration at click time even between clock ticks", async () => {
    mocks.raw.mockResolvedValue({
      unlocked: true,
      password_set: true,
      key_replaced: false,
      unlock_expires_at: new Date(Date.now() + 100).toISOString(),
    });
    await render();
    vi.setSystemTime(Date.now() + 150);
    await act(async () => decision("allow")!.click());
    expect(mocks.decide).not.toHaveBeenCalled();
  });
  it("clears on unlock expiry even without a lock event", async () => {
    mocks.raw.mockResolvedValue({
      unlocked: true,
      password_set: true,
      key_replaced: false,
      unlock_expires_at: new Date(Date.now() + 500).toISOString(),
    });
    await render();
    await tick(500);
    expect(container.textContent).not.toContain("private args");
    expect(decision("allow")?.disabled).toBe(true);
  });
});
