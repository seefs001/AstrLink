// @vitest-environment happy-dom
import { act } from "react";
import { createRoot } from "react-dom/client";
import { expect, it } from "vitest";
import { ToolReviewBadge, ToolReviewDetails } from "./ToolReviewDetails";
import {
  parseToolReviewSummary,
  toolReviewApprovalStates,
  type ToolReviewSummary,
} from "./tool-review-record-model";
import { applyLocale, i18n } from "./i18n";
import type { RequestRecord } from "./request-record-model";
import {
  TOOL_REVIEW_NAVIGATE_EVENT,
  type ToolReviewJump,
} from "./tool-review-navigation";

const approvalSummary: ToolReviewSummary = {
  status: "pending",
  mode: "ask",
  model: "jev-latest",
  config_revision: "r1",
  reject_threshold: 0.5,
  started_at: "2026-10-09T10:00:00Z",
  latency_ms: 10,
  calls: [
    {
      index: 0,
      name: "shell",
      verdict: "flag",
      reason_code: "destructive_action",
      flag_probability: 0.8,
      action: "pending",
      latency_ms: 10,
      model: "jev-latest",
    },
  ],
  approval: {
    id: "approval_1",
    state: "pending",
    review_status: "flagged",
    requested_at: "2026-10-09T10:00:01Z",
    expires_at: "2026-10-09T10:02:01Z",
  },
};

it("preserves unknown historical backend and accepts strict backend snapshots independently of decisions", () => {
  expect(parseToolReviewSummary(approvalSummary)).not.toHaveProperty("backend");
  for (const backend of ["jev", "responses"]) {
    const parsed = parseToolReviewSummary({ ...approvalSummary, backend });
    expect(parsed.backend).toBe(backend);
    expect(parsed.calls).toEqual(approvalSummary.calls);
    expect(parsed.approval).toEqual(approvalSummary.approval);
  }
  for (const backend of [null, "", "openai", "Responses", 0, true]) {
    expect(() =>
      parseToolReviewSummary({ ...approvalSummary, backend }),
    ).toThrow("Invalid tool review metadata");
  }
});

it("labels Responses scores as model-reported rather than equivalent Jev probabilities", async () => {
  Reflect.set(globalThis, "IS_REACT_ACT_ENVIRONMENT", true);
  const container = document.createElement("div");
  const root = createRoot(container);
  try {
    await act(async () =>
      root.render(
        <ToolReviewDetails
          summary={{
            ...approvalSummary,
            backend: "responses",
            status: "flagged",
            calls: [
              {
                ...approvalSummary.calls[0],
                pass_probability: 0.2,
                action: "delivered",
              },
            ],
            approval: {
              ...approvalSummary.approval!,
              state: "approved",
              decided_at: "2026-10-09T10:01:00Z",
            },
          }}
        />,
      ),
    );
    expect(
      container.querySelector('[data-testid="tool-review-backend-record"]')
        ?.textContent,
    ).toBe("审查后端: Responses 兼容");
    expect(container.textContent).toContain("模型自报风险评分");
    expect(container.textContent).toContain("模型自报安全评分");
    expect(container.textContent).toContain(
      i18n.t("toolReview.responsesScoreHint"),
    );
    expect(container.textContent).toContain("操作员已批准");
    expect(container.textContent).toContain("已交付");
    expect(container.textContent).toContain("80%");
    expect(container.textContent).not.toContain("风险概率");
    await act(async () =>
      root.render(<ToolReviewDetails summary={approvalSummary} />),
    );
    expect(
      container.querySelector('[data-testid="tool-review-backend-record"]')
        ?.textContent,
    ).toBe("审查后端: 未记录");
  } finally {
    await act(async () => root.unmount());
  }
});

it("preserves unknown historical batch mode and strictly parses explicit boolean snapshots", () => {
  expect(parseToolReviewSummary(approvalSummary)).not.toHaveProperty(
    "batch_calls",
  );
  for (const batch_calls of [false, true]) {
    const parsed = parseToolReviewSummary({ ...approvalSummary, batch_calls });
    expect(parsed.batch_calls).toBe(batch_calls);
    expect(parsed.calls).toEqual(approvalSummary.calls);
    expect(parsed.approval).toEqual(approvalSummary.approval);
  }
  for (const batch_calls of [null, 0, 1, "true", "false", [], {}]) {
    expect(() =>
      parseToolReviewSummary({ ...approvalSummary, batch_calls }),
    ).toThrow("Invalid tool review metadata");
  }
});

it.each([
  [undefined, "未记录"],
  [false, "逐条"],
  [true, "整批"],
] as const)(
  "shows batch snapshot %s separately from risk, human decision and delivery",
  async (batch_calls, label) => {
    Reflect.set(globalThis, "IS_REACT_ACT_ENVIRONMENT", true);
    const container = document.createElement("div");
    const root = createRoot(container);
    try {
      await act(async () =>
        root.render(
          <ToolReviewDetails
            summary={{
              ...approvalSummary,
              ...(batch_calls === undefined ? {} : { batch_calls }),
            }}
          />,
        ),
      );
      expect(
        container.querySelector('[data-testid="tool-review-method"]')
          ?.textContent,
      ).toBe(`审查方式: ${label}`);
      expect(container.textContent).toContain("等待人工决定");
      expect(container.textContent).toContain("破坏性操作");
      expect(container.textContent).toContain("80%");
      expect(container.textContent).toContain("待交付");
      expect(container.textContent).not.toContain("已交付");
    } finally {
      await act(async () => root.unmount());
    }
  },
);

it("strictly parses approval metadata without inferring human decisions or delivery", () => {
  expect(parseToolReviewSummary(approvalSummary)).toEqual(approvalSummary);
  for (const state of toolReviewApprovalStates) {
    const approval = {
      ...approvalSummary.approval!,
      state,
      ...(state === "pending" ? {} : { decided_at: "2026-10-09T10:01:00Z" }),
    };
    expect(
      parseToolReviewSummary({ ...approvalSummary, approval }).approval,
    ).toEqual(approval);
  }
  expect(
    parseToolReviewSummary({ ...approvalSummary, approval: undefined })
      .approval,
  ).toBeUndefined();
  for (const approval of [
    null,
    [],
    {},
    { ...approvalSummary.approval, arguments: "private content" },
    { ...approvalSummary.approval, id: "" },
    { ...approvalSummary.approval, id: "x".repeat(129) },
    { ...approvalSummary.approval, id: "approval/1" },
    { ...approvalSummary.approval, state: "allowed" },
    { ...approvalSummary.approval, review_status: "passed" },
    { ...approvalSummary.approval, requested_at: null },
    { ...approvalSummary.approval, requested_at: "2026-02-30T10:00:00Z" },
    { ...approvalSummary.approval, requested_at: "0001-01-01T00:00:00.000Z" },
    { ...approvalSummary.approval, expires_at: "2026-10-09T24:00:00Z" },
    {
      ...approvalSummary.approval,
      state: "approved",
      requested_at: "2026-10-09T10:00:01.123456789Z",
      decided_at: "2026-10-09T10:00:01.123456788Z",
    },
    { ...approvalSummary.approval, expires_at: "2026-10-09T10:00:01Z" },
    { ...approvalSummary.approval, expires_at: "2026-10-09T09:00:01Z" },
    { ...approvalSummary.approval, decided_at: "2026-10-09T10:01:00Z" },
    { ...approvalSummary.approval, state: "denied" },
    { ...approvalSummary.approval, state: "approved", decided_at: null },
    {
      ...approvalSummary.approval,
      state: "expired",
      decided_at: "2026-10-09T10:00:00Z",
    },
  ]) {
    expect(() =>
      parseToolReviewSummary({ ...approvalSummary, approval }),
    ).toThrow("Invalid tool review metadata");
  }
  expect(() =>
    parseToolReviewSummary({ ...approvalSummary, mode: "mark" }),
  ).toThrow();
  expect(() =>
    parseToolReviewSummary({ ...approvalSummary, mode: "reject" }),
  ).toThrow();
});

it("preserves the tool location action during pending approval without unlocking content", async () => {
  Reflect.set(globalThis, "IS_REACT_ACT_ENVIRONMENT", true);
  const container = document.createElement("div");
  const root = createRoot(container);
  const jumps: ToolReviewJump[] = [];
  const onJump = (event: Event) =>
    jumps.push((event as CustomEvent<ToolReviewJump>).detail);
  window.addEventListener(TOOL_REVIEW_NAVIGATE_EVENT, onJump);
  const record = {
    id: "request-1",
    session_id: "session-1",
    tool_review: approvalSummary,
  } as RequestRecord;
  try {
    await act(async () =>
      root.render(
        <ToolReviewDetails summary={approvalSummary} record={record} />,
      ),
    );
    const locate = container.querySelector<HTMLButtonElement>(
      '[data-testid="tool-review-locate"]',
    )!;
    expect(locate.textContent).toContain("查看工具调用");
    await act(async () => locate.click());
    expect(jumps).toHaveLength(1);
    expect(jumps[0]).toMatchObject({
      target: "main",
      requestId: "request-1",
      sessionId: "session-1",
      reviewIndex: 0,
    });
    expect(Object.keys(jumps[0]).sort()).toEqual([
      "nonce",
      "requestId",
      "reviewIndex",
      "sessionId",
      "target",
      "version",
    ]);
    expect(container.textContent).toContain("等待人工决定");
  } finally {
    window.removeEventListener(TOOL_REVIEW_NAVIGATE_EVENT, onJump);
    await act(async () => root.unmount());
  }
});

it.each(["flagged", "failed"] as const)(
  "shows the completed %s review while human approval and delivery remain pending",
  async (review_status) => {
    Reflect.set(globalThis, "IS_REACT_ACT_ENVIRONMENT", true);
    const container = document.createElement("div");
    const root = createRoot(container);
    const summary: ToolReviewSummary = {
      ...approvalSummary,
      approval: { ...approvalSummary.approval!, review_status },
      calls: [
        {
          ...approvalSummary.calls[0],
          verdict: review_status === "failed" ? "error" : "flag",
          ...(review_status === "failed" ? { error_code: "timeout" } : {}),
        },
      ],
    };
    try {
      await act(async () =>
        root.render(<ToolReviewDetails summary={summary} />),
      );
      expect(container.textContent).toContain("审查结果");
      expect(container.textContent).toContain("人工决定");
      expect(container.textContent).toContain("等待人工决定");
      expect(container.textContent).toContain("待交付");
      expect(container.textContent).not.toContain("审查中");
      expect(container.textContent).not.toContain("仅记录");
      expect(container.textContent).not.toContain("交付未确认");
      expect(container.textContent).toContain(
        review_status === "flagged" ? "80% ≥ 50%" : "无法作出风险判定",
      );
      await act(async () =>
        root.render(
          <ToolReviewDetails
            summary={{
              ...summary,
              status: "flagged",
              approval: {
                ...summary.approval!,
                state: "approved",
                decided_at: "2026-10-09T10:01:00Z",
              },
              calls: [{ ...summary.calls[0], action: "withheld" }],
            }}
          />,
        ),
      );
      expect(container.textContent).toContain("操作员已批准");
      expect(container.textContent).toContain("未交付");
      expect(container.textContent).not.toContain("已交付");
      expect(container.textContent).toContain(
        review_status === "flagged" ? "80% ≥ 50%" : "无法作出风险判定",
      );
      await act(async () =>
        root.render(<ToolReviewDetails summary={{ ...summary, calls: [] }} />),
      );
      expect(
        container.querySelector('[data-testid="tool-review-details"]'),
      ).not.toBeNull();
      expect(container.textContent).toContain("等待人工决定");
    } finally {
      await act(async () => root.unmount());
    }
  },
);

it.each([
  ["zh-CN", "符合审查规则", "已获授权"],
  ["en", "Allowed by review rules", "Authorized"],
] as const)(
  "describes authorized as rule compliance in %s, not proof of authorization",
  async (locale, label, oldLabel) => {
    Reflect.set(globalThis, "IS_REACT_ACT_ENVIRONMENT", true);
    const container = document.createElement("div");
    const root = createRoot(container);
    try {
      await act(async () => {
        await applyLocale(locale);
        root.render(
          <ToolReviewDetails
            summary={{
              status: "passed",
              mode: "mark",
              model: "jev-latest",
              config_revision: "r1",
              started_at: "2026-10-09T10:00:00Z",
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
            }}
          />,
        );
      });
      expect(container.textContent).toContain(label);
      expect(container.textContent).not.toContain(oldLabel);
    } finally {
      await act(async () => {
        root.unmount();
        await applyLocale("zh-CN");
      });
    }
  },
);

it.each([
  ["not_applicable", false],
  ["pending", false],
  ["failed", true],
  ["interrupted", true],
  ["cancelled", true],
] as const)(
  "renders an empty %s summary only for real review outcomes",
  async (status, visible) => {
    Reflect.set(globalThis, "IS_REACT_ACT_ENVIRONMENT", true);
    const container = document.createElement("div");
    const root = createRoot(container);
    const summary: ToolReviewSummary = {
      status,
      mode: "reject",
      model: "jev-latest",
      config_revision: "r1",
      started_at: "2026-10-09T10:00:00Z",
      latency_ms: 0,
      calls: [],
    };
    try {
      await act(async () =>
        root.render(
          <>
            <ToolReviewBadge summary={summary} />
            <ToolReviewDetails summary={summary} />
          </>,
        ),
      );
      expect(
        !!container.querySelector('[data-testid="tool-review-badge"]'),
      ).toBe(visible);
      expect(
        !!container.querySelector('[data-testid="tool-review-details"]'),
      ).toBe(visible);
      expect(container.textContent).not.toContain("接受");
    } finally {
      await act(async () => root.unmount());
    }
  },
);

it.each([
  ["conflicting_call", "工具调用的流式片段与最终内容不一致"],
  ["reviewer_invalid_json", "审查服务返回的 JSON 格式无效"],
  ["reviewer_invalid_review", "审查服务返回的判定无效"],
  ["reviewer_invalid_reason", "审查服务返回的原因分类无效"],
  ["reviewer_invalid_model", "审查服务返回的模型信息无效"],
  ["invalid_response", "响应不完整或格式不受支持"],
])("explains %s without blaming tool input", async (error_code, message) => {
  Reflect.set(globalThis, "IS_REACT_ACT_ENVIRONMENT", true);
  const container = document.createElement("div");
  const root = createRoot(container);
  try {
    await act(async () =>
      root.render(
        <ToolReviewDetails
          summary={{
            status: "failed",
            mode: "reject",
            model: "jev-latest",
            config_revision: "r1",
            started_at: "2026-10-09T10:00:00Z",
            latency_ms: 0,
            calls: [],
            error_code,
          }}
        />,
      ),
    );
    expect(container.textContent).toContain(message);
    expect(container.textContent).toContain(error_code);
    expect(container.textContent).not.toContain("工具调用信息不完整或无效");
  } finally {
    await act(async () => root.unmount());
  }
});

it("distinguishes a flagged call from a passing call withheld with its batch", async () => {
  Reflect.set(globalThis, "IS_REACT_ACT_ENVIRONMENT", true);
  const summary: ToolReviewSummary = {
    status: "blocked",
    mode: "reject",
    model: "jev-latest",
    config_revision: "revision-17",
    reject_threshold: 0.7,
    started_at: "2026-10-09T10:00:00Z",
    latency_ms: 40,
    calls: [
      {
        index: 0,
        name: "shell",
        verdict: "flag",
        model_verdict: "flag",
        reason_code: "destructive_action",
        pass_probability: 0,
        flag_probability: 1,
        action: "withheld",
        latency_ms: 30,
        model: "jev-latest",
      },
      {
        index: 1,
        name: "read_file",
        verdict: "pass",
        pass_probability: 0.95,
        flag_probability: 0.05,
        action: "withheld",
        latency_ms: 40,
        model: "jev-latest",
      },
    ],
  };
  const container = document.createElement("div");
  document.body.append(container);
  const root = createRoot(container);
  try {
    await act(async () => root.render(<ToolReviewDetails summary={summary} />));
    expect(container.textContent).toContain("read_file");
    expect(container.textContent).toContain("本调用接受，但同批其他调用");
    expect(container.textContent).toContain("破坏性操作");
    expect(container.textContent).toContain("模型未提供原因分类");
    expect(container.textContent).toContain("100% ≥ 70%");
    expect(container.textContent).toContain("5% < 70%");
    expect(container.textContent).toContain("revision-17");
    expect(container.textContent).toContain("95%");
    expect(container.textContent).toContain("0%");
    expect(container.textContent).not.toContain("已执行");
    await act(async () =>
      root.render(
        <ToolReviewDetails
          summary={{
            ...summary,
            status: "flagged",
            mode: "mark",
            calls: [{ ...summary.calls[0], action: "delivered" }],
          }}
        />,
      ),
    );
    expect(container.textContent).toContain("已交付");
    expect(container.textContent).toContain("拒绝（仅记录）");
    expect(container.textContent).not.toContain("本调用接受，但同批其他调用");
    expect(container.textContent).toContain("判定拒绝，仅记录，已交付");
    expect(container.textContent).toContain("仅记录");
    expect(container.textContent).toContain("拒绝");
    // A finished review with an unconfirmed write must not read as delivered,
    // and keeps its real verdict.
    await act(async () =>
      root.render(
        <ToolReviewDetails
          summary={{
            ...summary,
            status: "passed",
            error_code: "client_delivery_failed",
            calls: [{ ...summary.calls[1], action: "pending" }],
          }}
        />,
      ),
    );
    expect(container.textContent).toContain("交付未确认（可能部分交付）");
    expect(container.textContent).toContain("接受");
    expect(container.textContent).toContain("拦截");
    expect(container.textContent).not.toContain("已交付");
    expect(container.textContent).not.toContain("待交付");
    await act(async () =>
      root.render(
        <ToolReviewDetails
          summary={{
            ...summary,
            status: "pending",
            calls: [{ ...summary.calls[1], action: "pending" }],
          }}
        />,
      ),
    );
    expect(container.textContent).toContain("待交付");
    expect(container.textContent).not.toContain("交付未确认");
    expect(container.textContent).not.toContain("低于拒绝阈值");
    await act(async () =>
      root.render(
        <ToolReviewDetails
          summary={{
            ...summary,
            reject_threshold: undefined,
            status: "failed",
            error_code: "future_safe_code",
            calls: [
              {
                ...summary.calls[1],
                verdict: "error",
                error_code: "timeout",
                flag_probability: undefined,
              },
            ],
          }}
        />,
      ),
    );
    expect(container.textContent).toContain("拒绝阈值: 未记录");
    expect(container.textContent).not.toContain("50%");
    expect(container.textContent).toContain("审查超时");
    expect(container.textContent).toContain("future_safe_code");
    expect(container.textContent).toContain("无法作出风险判定");
    await act(async () =>
      root.render(
        <ToolReviewDetails
          summary={{
            ...summary,
            status: "failed",
            error_code: "constructor",
            calls: [],
          }}
        />,
      ),
    );
    expect(container.textContent).toContain("审查未能完成。");
    expect(container.textContent).not.toContain("function Object");
  } finally {
    await act(async () => root.unmount());
    container.remove();
  }
});
