// @vitest-environment happy-dom
import { act } from "react";
import { createRoot, type Root } from "react-dom/client";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
const invoke = vi.hoisted(() => vi.fn());
vi.mock("@tauri-apps/api/core", () => ({ invoke }));
import { ToolReviewSettings } from "./ToolReviewSettings";
import { settleClosedLayer } from "./lib/test-layer-settle";
import { applyLocale } from "./i18n";

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
async function render() {
  await act(async () =>
    root.render(<ToolReviewSettings coreSessionKey="test" isReady />),
  );
}
beforeEach(() => {
  Reflect.set(globalThis, "IS_REACT_ACT_ENVIRONMENT", true);
  Reflect.set(window, "__TAURI_INTERNALS__", {});
  invoke.mockReset();
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
      (item) => item.textContent === "插入 tool_call",
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
  it("asks for a key before the outbound consent when none is stored", async () => {
    invoke.mockResolvedValue({
      ...initial,
      settings: { ...initial.settings, credential_configured: false },
    });
    await render();
    await click("启用工具审查");
    await click("保存");
    expect(document.querySelector('[role="alertdialog"]')).toBeNull();
    expect(container.textContent).toContain("启用前请先填写 Jev API key");
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
    await click("插入 user_request");
    await click("插入 tool_call");
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
