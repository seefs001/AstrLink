// @vitest-environment happy-dom
import { act } from "react";
import { createRoot } from "react-dom/client";
import { expect, it } from "vitest";
import { ToolReviewDetails } from "./ToolReviewDetails";
import type { ToolReviewSummary } from "./tool-review-record-model";

it("distinguishes a flagged call from a passing call withheld with its batch", async () => {
  Reflect.set(globalThis, "IS_REACT_ACT_ENVIRONMENT", true);
  const summary: ToolReviewSummary = {
    status: "blocked",
    mode: "reject",
    model: "jev-latest",
    config_revision: "revision-17",
    started_at: "2026-10-09T10:00:00Z",
    latency_ms: 40,
    calls: [
      {
        index: 0,
        name: "shell",
        verdict: "flag",
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
    expect(container.textContent).toContain("同批拦截");
    expect(container.textContent).toContain("revision-17");
    expect(container.textContent).toContain("95.0%");
    expect(container.textContent).toContain("0.0%");
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
    expect(container.textContent).toContain("审查命中");
    expect(container.textContent).not.toContain("同批拦截");
    expect(container.textContent).toContain("标记 (mark)");
    expect(container.textContent).toContain("命中");
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
    expect(container.textContent).toContain("审查通过");
    expect(container.textContent).toContain("拒绝 (reject)");
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
  } finally {
    await act(async () => root.unmount());
    container.remove();
  }
});
