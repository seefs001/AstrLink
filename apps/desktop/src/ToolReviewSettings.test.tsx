// @vitest-environment happy-dom
import { act } from "react";
import { createRoot, type Root } from "react-dom/client";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
const { invoke, openApprovals } = vi.hoisted(() => ({
  invoke: vi.fn(),
  openApprovals: vi.fn(),
}));
vi.mock("@tauri-apps/api/core", () => ({ invoke }));
vi.mock("./tool-review-approval-window", () => ({
  openToolReviewApprovalWindow: openApprovals,
}));
import { ToolReviewSettings } from "./ToolReviewSettings";
import { highRiskToolReviewPrompt } from "./tool-review-settings-model";
import { settleClosedLayer } from "./lib/test-layer-settle";
import { applyLocale, i18n } from "./i18n";

const initial = {
  settings: {
    enabled: false,
    endpoint: "https://api.typesafe.ai/v1/systemone",
    model: "jev-latest",
    prompt: "{{user_request}}\n{{tool_call}}",
    mode: "mark",
    timeout_ms: 5000,
    revision: "r1",
    credential_configured: true,
  },
  etag: '"sha256:' + "a".repeat(64) + '"',
};
let container: HTMLDivElement;
let root: Root;
function button(label: string): HTMLButtonElement {
  const result = Array.from(document.querySelectorAll("button")).find(
    (item) =>
      item.textContent === label || item.getAttribute("aria-label") === label,
  );
  if (!result) throw new Error(`Missing button: ${label}`);
  return result;
}
async function click(label: string) {
  await act(async () => button(label).click());
}
async function input(id: string, value: string) {
  const node = container.querySelector<HTMLInputElement>(id)!;
  await act(async () => {
    Object.getOwnPropertyDescriptor(
      HTMLInputElement.prototype,
      "value",
    )!.set!.call(node, value);
    node.dispatchEvent(new Event("input", { bubbles: true }));
  });
}
async function select(id: string, label: string) {
  const trigger = container.querySelector<HTMLButtonElement>(id)!;
  await act(async () =>
    trigger.dispatchEvent(
      new PointerEvent("pointerdown", {
        bubbles: true,
        button: 0,
        pointerType: "mouse",
      }),
    ),
  );
  const option = Array.from(
    document.querySelectorAll<HTMLElement>('[role="option"]'),
  ).find((node) => node.textContent === label)!;
  await act(async () => option.click());
  await settleClosedLayer();
}
function value(id: string) {
  return container.querySelector<HTMLInputElement>(id)!.value;
}
function configuredServer(overrides: Record<string, unknown>) {
  const saved = { ...initial.settings, backend: "jev", ...overrides };
  invoke.mockImplementation(async (command, args) => {
    if (command === "update_tool_review_settings") {
      const {
        api_key,
        data_transfer_acknowledged: _ack,
        ...fields
      } = args.patch;
      if (
        fields.backend !== saved.backend &&
        saved.credential_configured &&
        api_key === undefined
      )
        throw new Error("backend_key_replacement_required");
      Object.assign(saved, fields);
      if (api_key !== undefined) saved.credential_configured = api_key !== "";
    }
    return { settings: structuredClone(saved), etag: initial.etag };
  });
}
async function render() {
  await act(async () =>
    root.render(<ToolReviewSettings coreSessionKey="test" isReady />),
  );
}
beforeEach(() => {
  Reflect.set(globalThis, "IS_REACT_ACT_ENVIRONMENT", true);
  Reflect.set(window, "__TAURI_INTERNALS__", {});
  invoke.mockReset();
  openApprovals.mockReset();
  openApprovals.mockResolvedValue(undefined);
  invoke.mockImplementation(async (command, args) =>
    command === "get_tool_review_settings"
      ? structuredClone(initial)
      : {
          ...initial,
          settings: {
            ...initial.settings,
            ...args.patch,
            api_key: undefined,
            data_transfer_acknowledged: undefined,
          },
        },
  );
  container = document.createElement("div");
  document.body.append(container);
  root = createRoot(container);
});
afterEach(async () => {
  await act(async () => root.unmount());
  await applyLocale("zh-CN");
  container.remove();
  Reflect.deleteProperty(window, "__TAURI_INTERNALS__");
});

describe("ToolReviewSettings", () => {
  it("uses backend-neutral credential and batch copy with explicit switching consent and Responses score semantics in both locales", async () => {
    configuredServer({
      backend: "responses",
      enabled: false,
      credential_configured: false,
      endpoint: "https://response.example/full/v1/responses",
      model: "schema-fixture-model",
    });
    await render();
    await click(i18n.t("toolReview.enable"));
    for (const locale of ["zh-CN", "en"] as const) {
      await act(async () => applyLocale(locale));
      await click(i18n.t("common.save"));
      expect(
        container.querySelector('label[for="tool-review-key"]')?.textContent,
      ).toMatch(/^API Key/);
      expect(i18n.t("toolReview.key")).toBe("API Key");
      expect(container.textContent).toContain(i18n.t("toolReview.keyRequired"));
      expect(i18n.t("toolReview.keyRequired")).not.toMatch(/Jev/i);
      expect(container.textContent).not.toMatch(/Jev API key/i);
      expect(i18n.t("toolReview.batchCallsHelp")).not.toContain("Jev HTTP");
      expect(i18n.t("toolReview.batchCallsHelp")).toContain(
        locale === "zh-CN" ? "所选后端" : "selected backend",
      );
      expect(i18n.t("toolReview.confirmBody")).toContain(
        locale === "zh-CN" ? "切换后端即使地址相同" : "even at the same URL",
      );
      expect(i18n.t("toolReview.thresholdHint")).toContain(
        locale === "zh-CN"
          ? "Responses 使用模型自报评分"
          : "Responses uses model-reported scores",
      );
      expect(
        invoke.mock.calls.some(
          ([command]) => command === "update_tool_review_settings",
        ),
      ).toBe(false);
    }
  });
  it("isolates backend connection drafts and clears every unsaved key without changing ask, batch or custom timeout", async () => {
    configuredServer({
      backend: "jev",
      endpoint: "https://custom.example/jev",
      model: "custom-jev",
      mode: "ask",
      batch_calls: true,
      timeout_ms: 4321,
    });
    await render();
    await input("#tool-review-endpoint", "https://edited.example/exact/jev");
    await input("#tool-review-model", "edited-jev");
    await input("#tool-review-key", "synthetic-unsaved-jev-key");
    await select("#tool-review-backend", "Responses 兼容");
    expect(value("#tool-review-endpoint")).toBe("");
    expect(value("#tool-review-model")).toBe("");
    expect(value("#tool-review-key")).toBe("");
    expect(value("#tool-review-timeout")).toBe("4321");
    expect(
      container.querySelector<HTMLInputElement>("#tool-review-endpoint")!
        .placeholder,
    ).toBe("https://api.openai.com/v1/responses");
    expect(
      container
        .querySelector<HTMLInputElement>("#tool-review-endpoint")!
        .closest("details")!.open,
    ).toBe(true);
    expect(container.textContent).toContain(i18n.t("toolReview.keyMissing"));
    await input(
      "#tool-review-endpoint",
      "https://response.example/exact/path/responses",
    );
    await input("#tool-review-model", "schema-fixture-model");
    await input("#tool-review-key", "synthetic-unsaved-responses-key");
    await select("#tool-review-backend", "Jev");
    expect(value("#tool-review-endpoint")).toBe(
      "https://edited.example/exact/jev",
    );
    expect(value("#tool-review-model")).toBe("edited-jev");
    expect(value("#tool-review-key")).toBe("");
    await select("#tool-review-backend", "Responses 兼容");
    expect(value("#tool-review-endpoint")).toBe(
      "https://response.example/exact/path/responses",
    );
    expect(value("#tool-review-model")).toBe("schema-fixture-model");
    expect(value("#tool-review-key")).toBe("");
    expect(container.querySelector("#tool-review-mode")!.textContent).toBe(
      "拦截并请求许可",
    );
    expect(
      container.querySelector("#tool-review-batch-calls")!.textContent,
    ).toBe("整批");
    expect(
      invoke.mock.calls.some(
        ([command]) => command === "update_tool_review_settings",
      ),
    ).toBe(false);
  });
  it("requires a new key and consent when an enabled backend changes even at the same exact endpoint", async () => {
    configuredServer({
      enabled: true,
      backend: "jev",
      endpoint: "https://gateway.example/exact/review",
      model: "saved-jev",
      mode: "ask",
      batch_calls: true,
      timeout_ms: 7654,
    });
    await render();
    await select("#tool-review-backend", "Responses 兼容");
    await input(
      "#tool-review-endpoint",
      "https://gateway.example/exact/review",
    );
    await input("#tool-review-model", "schema-fixture-model");
    await click("保存");
    expect(container.textContent).toContain(i18n.t("toolReview.keyRequired"));
    expect(
      invoke.mock.calls.some(
        ([command]) => command === "update_tool_review_settings",
      ),
    ).toBe(false);
    await input("#tool-review-key", "synthetic-new-responses-key");
    await click("保存");
    expect(
      document.querySelector('[role="alertdialog"]')?.textContent,
    ).toContain("Responses 兼容");
    expect(
      invoke.mock.calls.some(
        ([command]) => command === "update_tool_review_settings",
      ),
    ).toBe(false);
    await click(i18n.t("toolReview.confirm"));
    await settleClosedLayer();
    expect(invoke).toHaveBeenLastCalledWith(
      "update_tool_review_settings",
      expect.objectContaining({
        patch: expect.objectContaining({
          backend: "responses",
          api_key: "synthetic-new-responses-key",
          endpoint: "https://gateway.example/exact/review",
          model: "schema-fixture-model",
          mode: "ask",
          batch_calls: true,
          timeout_ms: 7654,
          data_transfer_acknowledged: true,
        }),
      }),
    );
    expect(value("#tool-review-key")).toBe("");
    await select("#tool-review-batch-calls", "逐条");
    await click("保存");
    const patch = invoke.mock.calls.at(-1)![1].patch;
    expect(patch).toMatchObject({
      backend: "responses",
      mode: "ask",
      batch_calls: false,
    });
    expect(patch).not.toHaveProperty("api_key");
  });
  it("confirms disabled backend changes and explicitly clears the previous credential instead of omitting it", async () => {
    configuredServer({
      enabled: false,
      backend: "jev",
      credential_configured: true,
    });
    await render();
    await select("#tool-review-backend", "Responses 兼容");
    await input(
      "#tool-review-endpoint",
      "https://response.example/full/v1/responses",
    );
    await input("#tool-review-model", "schema-fixture-model");
    await click("保存");
    expect(document.querySelector('[role="alertdialog"]')).not.toBeNull();
    await click(i18n.t("common.cancel"));
    await settleClosedLayer();
    expect(
      invoke.mock.calls.some(
        ([command]) => command === "update_tool_review_settings",
      ),
    ).toBe(false);
    await click("保存");
    await click(i18n.t("toolReview.confirm"));
    await settleClosedLayer();
    expect(invoke).toHaveBeenLastCalledWith(
      "update_tool_review_settings",
      expect.objectContaining({
        patch: expect.objectContaining({
          backend: "responses",
          enabled: false,
          api_key: "",
          data_transfer_acknowledged: true,
        }),
      }),
    );
    expect(container.textContent).toContain(i18n.t("toolReview.keyMissing"));
  });
  it("restores a saved Responses draft without guessing an unknown Jev connection or reusing the Responses key", async () => {
    configuredServer({
      enabled: true,
      backend: "responses",
      endpoint: "https://response.example/custom/path",
      model: "saved-schema-model",
      timeout_ms: 30001,
    });
    await render();
    await input("#tool-review-key", "synthetic-unsaved-responses-key");
    await select("#tool-review-backend", "Jev");
    expect(value("#tool-review-endpoint")).toBe("");
    expect(value("#tool-review-model")).toBe("");
    expect(value("#tool-review-key")).toBe("");
    expect(container.textContent).toContain(i18n.t("toolReview.keyMissing"));
    await select("#tool-review-backend", "Responses 兼容");
    expect(value("#tool-review-endpoint")).toBe(
      "https://response.example/custom/path",
    );
    expect(value("#tool-review-model")).toBe("saved-schema-model");
    expect(value("#tool-review-timeout")).toBe("30001");
    expect(value("#tool-review-key")).toBe("");
    expect(container.textContent).toContain(i18n.t("toolReview.keyConfigured"));
  });
  it("defaults to individual review and saves batch selection without changing ask or delivery policy", async () => {
    invoke.mockImplementation(async (command, args) => ({
      ...initial,
      settings: {
        ...initial.settings,
        mode: "ask",
        ...(command === "update_tool_review_settings" ? args.patch : {}),
      },
    }));
    await render();
    const trigger = () =>
      container.querySelector<HTMLButtonElement>("#tool-review-batch-calls")!;
    expect(trigger().textContent).toBe("逐条");
    const choose = async (label: string) => {
      await act(async () =>
        trigger().dispatchEvent(
          new PointerEvent("pointerdown", {
            bubbles: true,
            button: 0,
            pointerType: "mouse",
          }),
        ),
      );
      const option = Array.from(
        document.querySelectorAll<HTMLElement>('[role="option"]'),
      ).find((node) => node.textContent === label)!;
      await act(async () => option.click());
      await settleClosedLayer();
    };
    await choose("整批");
    expect(trigger().textContent).toBe("整批");
    await click("保存");
    expect(invoke).toHaveBeenLastCalledWith(
      "update_tool_review_settings",
      expect.objectContaining({
        patch: expect.objectContaining({
          batch_calls: true,
          mode: "ask",
          ask_on_review_error: false,
          approval_timeout_ms: 120000,
        }),
      }),
    );
    await click("审查方式说明");
    expect(document.body.textContent).toContain("128 KiB");
    expect(document.body.textContent).toContain(
      "超限不会拆分，也不会自动退回逐条",
    );
    await act(async () =>
      document.dispatchEvent(
        new KeyboardEvent("keydown", { key: "Escape", bubbles: true }),
      ),
    );
    await settleClosedLayer();
    await choose("逐条");
    await click("保存");
    expect(invoke).toHaveBeenLastCalledWith(
      "update_tool_review_settings",
      expect.objectContaining({
        patch: expect.objectContaining({ batch_calls: false, mode: "ask" }),
      }),
    );
    await act(async () => applyLocale("en"));
    expect(trigger().textContent).toBe("Individual");
  });
  it("edits ask settings with default fail-closed behavior and bounded approval timeout", async () => {
    invoke.mockImplementation(async (command, args) => ({
      ...initial,
      settings: {
        ...initial.settings,
        mode: "ask",
        ...(command === "update_tool_review_settings" ? args.patch : {}),
      },
    }));
    await render();
    const timeout = () =>
      container.querySelector<HTMLInputElement>(
        "#tool-review-approval-timeout",
      )!;
    const askOnError = () =>
      container.querySelector<HTMLButtonElement>("#tool-review-ask-on-error")!;
    expect(timeout().value).toBe("120000");
    expect(timeout().min).toBe("1000");
    expect(timeout().max).toBe("600000");
    expect(askOnError().getAttribute("aria-checked")).toBe("false");
    await click("审查失败时也询问");
    expect(askOnError().getAttribute("aria-checked")).toBe("true");
    for (const value of ["", "999", "600001", "1000.5"]) {
      await input("#tool-review-approval-timeout", value);
      await click("保存");
      expect(container.textContent).toContain("配置无效");
    }
    expect(
      invoke.mock.calls.filter(
        ([command]) => command === "update_tool_review_settings",
      ),
    ).toHaveLength(0);
    await input("#tool-review-approval-timeout", "600000");
    await act(async () => applyLocale("en"));
    expect(timeout().value).toBe("600000");
    expect(askOnError().getAttribute("aria-checked")).toBe("true");
    await click("Save");
    expect(invoke).toHaveBeenLastCalledWith(
      "update_tool_review_settings",
      expect.objectContaining({
        patch: expect.objectContaining({
          mode: "ask",
          ask_on_review_error: true,
          approval_timeout_ms: 600000,
        }),
      }),
    );
  });
  it("opens the approval queue without saving a draft, and bounds opening failures", async () => {
    await render();
    await input("#tool-review-key", "unsaved-key");
    await click("打开审批队列");
    expect(openApprovals).toHaveBeenCalledOnce();
    expect(
      invoke.mock.calls.filter(
        ([command]) => command === "update_tool_review_settings",
      ),
    ).toHaveLength(0);
    expect(
      container.querySelector<HTMLInputElement>("#tool-review-key")!.value,
    ).toBe("unsaved-key");
    openApprovals.mockRejectedValueOnce(
      new Error("private raw approval arguments"),
    );
    await click("打开审批队列");
    expect(container.textContent).toContain("无法读取或更新审批");
    expect(container.textContent).not.toContain(
      "private raw approval arguments",
    );
    await act(async () =>
      root.render(<ToolReviewSettings coreSessionKey={null} isReady={false} />),
    );
    expect(button("打开审批队列").disabled).toBe(true);
  });
  it("applies the high-risk preset only to the prompt draft until saved", async () => {
    invoke.mockImplementation(async (command, args) => ({
      ...initial,
      settings: {
        ...initial.settings,
        prompt: "saved custom policy {{user_request}} {{tool_call}}",
        reject_threshold: 0.6,
        ...(command === "update_tool_review_settings" ? args.patch : {}),
      },
    }));
    await render();
    const prompt = () =>
      container.querySelector<HTMLTextAreaElement>("#tool-review-prompt")!
        .value;
    const threshold = () =>
      container.querySelector<HTMLInputElement>("#tool-review-threshold")!
        .value;
    expect(prompt()).toMatch(/^saved custom policy/);
    expect(threshold()).toBe("60");
    await click("使用高危预设");
    expect(prompt()).toBe(highRiskToolReviewPrompt);
    expect(prompt()).toContain("{{user_request}}");
    expect(prompt()).toContain("{{tool_call}}");
    expect(threshold()).toBe("60");
    expect(container.textContent).toContain("高危预设已填入草稿，保存后生效");
    expect(
      invoke.mock.calls.filter(
        ([command]) => command === "update_tool_review_settings",
      ),
    ).toHaveLength(0);
    await act(async () => applyLocale("en"));
    expect(prompt()).toBe(highRiskToolReviewPrompt);
    await click("Save");
    expect(invoke).toHaveBeenLastCalledWith(
      "update_tool_review_settings",
      expect.objectContaining({
        patch: expect.objectContaining({
          prompt: highRiskToolReviewPrompt,
          reject_threshold: 0.6,
        }),
      }),
    );
  });
  it("expands the prompt without other settings and retains edits without saving", async () => {
    await render();
    await click("展开编辑器");
    const dialog = document.querySelector<HTMLElement>('[role="dialog"]')!;
    const editor = dialog.querySelector<HTMLTextAreaElement>("textarea")!;
    expect(editor).not.toBeNull();
    expect(document.activeElement).toBe(editor);
    expect(dialog.querySelector("input")).toBeNull();
    const prompt = "expanded draft {{user_request}} ";
    await act(async () => {
      Object.getOwnPropertyDescriptor(
        HTMLTextAreaElement.prototype,
        "value",
      )!.set!.call(editor, prompt);
      editor.dispatchEvent(new Event("input", { bubbles: true }));
      editor.setSelectionRange(prompt.length, prompt.length);
    });
    const insert = Array.from(dialog.querySelectorAll("button")).find(
      (item) => item.textContent === "插入工具调用",
    )!;
    await act(async () => insert.click());
    expect(editor.value).toBe("expanded draft {{user_request}} {{tool_call}}");
    await click("返回设置");
    await settleClosedLayer();
    expect(document.querySelector('[role="dialog"]')).toBeNull();
    expect(
      container.querySelector<HTMLTextAreaElement>("#tool-review-prompt")!
        .value,
    ).toBe("expanded draft {{user_request}} {{tool_call}}");
    expect(
      invoke.mock.calls.filter(
        ([command]) => command === "update_tool_review_settings",
      ),
    ).toHaveLength(0);
    await click("展开编辑器");
    expect(
      document.querySelector<HTMLTextAreaElement>('[role="dialog"] textarea')!
        .value,
    ).toBe("expanded draft {{user_request}} {{tool_call}}");
  });
  it("closes the expanded editor when the gateway session changes", async () => {
    await render();
    await click("展开编辑器");
    await act(async () =>
      root.render(<ToolReviewSettings coreSessionKey="replacement" isReady />),
    );
    expect(document.querySelector('[role="dialog"]')).toBeNull();
  });
  it("edits a percentage threshold, rejects invalid values and preserves it across locale changes", async () => {
    invoke.mockImplementation(async (command, args) =>
      command === "get_tool_review_settings"
        ? structuredClone(initial)
        : {
            ...initial,
            settings: {
              ...initial.settings,
              reject_threshold: args.patch.reject_threshold,
            },
          },
    );
    await render();
    const threshold = () =>
      container.querySelector<HTMLInputElement>("#tool-review-threshold")!;
    expect(threshold().value).toBe("50");
    expect(threshold().min).toBe("0");
    expect(threshold().max).toBe("100");
    expect(container.textContent).toContain(i18n.t("toolReview.thresholdHint"));
    for (const value of ["", "-1", "101"]) {
      await input("#tool-review-threshold", value);
      await click("保存");
      expect(container.textContent).toContain("配置无效");
    }
    expect(
      invoke.mock.calls.filter(
        ([command]) => command === "update_tool_review_settings",
      ),
    ).toHaveLength(0);
    await input("#tool-review-threshold", "75");
    await act(async () => applyLocale("en"));
    expect(threshold().value).toBe("75");
    await click("Save");
    expect(invoke).toHaveBeenLastCalledWith(
      "update_tool_review_settings",
      expect.objectContaining({
        patch: expect.objectContaining({ reject_threshold: 0.75 }),
      }),
    );
    for (const value of ["0", "100"]) {
      await input("#tool-review-threshold", value);
      await click("Save");
      expect(invoke).toHaveBeenLastCalledWith(
        "update_tool_review_settings",
        expect.objectContaining({
          patch: expect.objectContaining({
            reject_threshold: Number(value) / 100,
          }),
        }),
      );
    }
  });
  it("shows the 5000 ms default and saves an advanced timeout up to 120000 ms", async () => {
    invoke.mockImplementation(async (command, args) =>
      command === "get_tool_review_settings"
        ? structuredClone(initial)
        : {
            ...initial,
            settings: {
              ...initial.settings,
              timeout_ms: args.patch.timeout_ms,
            },
          },
    );
    await render();
    const timeout = container.querySelector<HTMLInputElement>(
      "#tool-review-timeout",
    )!;
    expect(timeout.value).toBe("5000");
    expect(timeout.min).toBe("1");
    expect(timeout.max).toBe("120000");
    await input("#tool-review-timeout", "120000");
    await click("保存");
    expect(invoke).toHaveBeenLastCalledWith(
      "update_tool_review_settings",
      expect.objectContaining({
        patch: expect.objectContaining({ timeout_ms: 120000 }),
      }),
    );
    expect(container.textContent).toContain("已保存");
  });
  it("requires in-app consent before enabling and clears a saved write-only key", async () => {
    invoke.mockImplementation(async (command, args) => {
      if (command === "get_tool_review_settings")
        return structuredClone(initial);
      const {
        api_key: _key,
        data_transfer_acknowledged: _ack,
        ...patch
      } = args.patch;
      return { ...initial, settings: { ...initial.settings, ...patch } };
    });
    const confirm = vi.fn(() => false);
    Reflect.set(window, "confirm", confirm);
    await render();
    await input("#tool-review-key", "secret-key");
    await click("启用工具审查");
    await click("保存");
    expect(document.querySelector('[role="alertdialog"]')).not.toBeNull();
    expect(
      invoke.mock.calls.filter(
        ([command]) => command === "update_tool_review_settings",
      ),
    ).toHaveLength(0);
    await click("取消");
    await settleClosedLayer();
    expect(
      invoke.mock.calls.filter(
        ([command]) => command === "update_tool_review_settings",
      ),
    ).toHaveLength(0);
    await click("保存");
    await click("同意外发并保存");
    await settleClosedLayer();
    expect(invoke).toHaveBeenLastCalledWith(
      "update_tool_review_settings",
      expect.objectContaining({
        etag: initial.etag,
        patch: expect.objectContaining({
          enabled: true,
          api_key: "secret-key",
          data_transfer_acknowledged: true,
        }),
      }),
    );
    expect(
      container.querySelector<HTMLInputElement>("#tool-review-key")!.value,
    ).toBe("");
    expect(container.textContent).toContain("已配置");
    expect(confirm).not.toHaveBeenCalled();
    Reflect.deleteProperty(window, "confirm");
  });
  it("keeps the draft and key across a language switch; refresh clears them", async () => {
    await render();
    await input("#tool-review-key", "unsaved-key");
    await input("#tool-review-endpoint", "not a url");
    await click("保存");
    expect(container.textContent).toContain("配置无效");
    await act(async () => applyLocale("en"));
    const value = (id: string) =>
      container.querySelector<HTMLInputElement>(id)!.value;
    expect(value("#tool-review-key")).toBe("unsaved-key");
    expect(value("#tool-review-endpoint")).toBe("not a url");
    expect(container.textContent).toContain("Invalid configuration");
    expect(container.textContent).not.toContain("配置无效");
    const loads = () =>
      invoke.mock.calls.filter(
        ([command]) => command === "get_tool_review_settings",
      ).length;
    expect(loads()).toBe(1);
    await click("Refresh");
    expect(loads()).toBe(2);
    expect(value("#tool-review-key")).toBe("");
    expect(value("#tool-review-endpoint")).toBe(initial.settings.endpoint);
    expect(container.textContent).not.toContain("Invalid configuration");
  });
  it("opens missing credentials, retains manual collapse and draft, then reopens on key validation", async () => {
    invoke.mockResolvedValue({
      ...initial,
      settings: { ...initial.settings, credential_configured: false },
    });
    await render();
    const connection = container
      .querySelector<HTMLInputElement>("#tool-review-key")!
      .closest("details")!;
    expect(connection.open).toBe(true);
    await input("#tool-review-endpoint", "https://draft.example.com/check");
    await act(async () => connection.querySelector("summary")!.click());
    expect(connection.open).toBe(false);
    await click("启用工具审查");
    expect(connection.open).toBe(false);
    await act(async () => applyLocale("en"));
    expect(connection.open).toBe(false);
    await act(async () => applyLocale("zh-CN"));
    expect(
      container.querySelector<HTMLInputElement>("#tool-review-endpoint")!.value,
    ).toBe("https://draft.example.com/check");
    await click("保存");
    expect(connection.open).toBe(true);
    expect(document.querySelector('[role="alertdialog"]')).toBeNull();
    expect(container.textContent).toContain(i18n.t("toolReview.keyRequired"));
    expect(
      invoke.mock.calls.filter(
        ([command]) => command === "update_tool_review_settings",
      ),
    ).toHaveLength(0);
  });
  it("asks again for a new destination while enabled, but does not expose raw save errors", async () => {
    invoke.mockResolvedValue({
      ...initial,
      settings: { ...initial.settings, enabled: true },
    });
    await render();
    await input("#tool-review-endpoint", "https://review.example.com/check");
    await click("保存");
    expect(
      document.querySelector('[role="alertdialog"]')?.textContent,
    ).toContain("https://review.example.com/check");
    invoke.mockRejectedValue(
      new Error("upstream leaked secret-key and private prompt"),
    );
    await click("同意外发并保存");
    await settleClosedLayer();
    expect(container.textContent).toContain("保存失败");
    expect(document.body.textContent).not.toContain("leaked secret-key");
  });
  it("inserts both prompt variables and retains the draft after a revision conflict", async () => {
    await render();
    await click("插入用户请求");
    await click("插入工具调用");
    const prompt = container.querySelector<HTMLTextAreaElement>(
      "#tool-review-prompt",
    )!;
    expect(prompt.value).toContain("{{user_request}}");
    expect(prompt.value).toContain("{{tool_call}}");
    invoke.mockRejectedValue(new Error("tool_review_settings_conflict"));
    await click("保存");
    expect(container.textContent).toContain("配置已被其他操作修改");
    expect(prompt.value).toContain("{{user_request}}");
  });
});
