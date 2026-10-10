// @vitest-environment happy-dom
import { act } from "react";
import { createRoot, type Root } from "react-dom/client";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
vi.mock("./edition", () => ({ isWebEdition: true }));
import {
  getToolReviewSettings,
  updateToolReviewSettings,
  listRequestRecords,
  listRequestSessions,
} from "./bridge";
import { CONSOLE_SIGNED_OUT, webInvoke } from "./web-transport";
import { SafetyPolicy } from "./SafetyPolicy";
import { settleClosedLayer } from "./lib/test-layer-settle";

const settings = {
  enabled: false,
  endpoint: "https://api.typesafe.ai/v1/systemone",
  model: "jev-latest",
  prompt: "{{user_request}}\n{{tool_call}}",
  mode: "mark",
  timeout_ms: 5000,
  revision: "r1",
  credential_configured: true,
};
const etag = '"sha256:' + "a".repeat(64) + '"';
const fetchMock = vi.fn();
let container: HTMLDivElement | undefined;
let root: Root | undefined;
function response(
  value: unknown,
  status = 200,
  headers: Record<string, string> = {},
) {
  return new Response(JSON.stringify(value), { status, headers });
}
beforeEach(() => {
  Reflect.set(globalThis, "IS_REACT_ACT_ENVIRONMENT", true);
  Reflect.deleteProperty(window, "__TAURI_INTERNALS__");
  fetchMock.mockReset();
  vi.stubGlobal("fetch", fetchMock);
});
afterEach(async () => {
  if (root) await act(async () => root!.unmount());
  root = undefined;
  container?.remove();
  container = undefined;
  vi.unstubAllGlobals();
});

describe("tool review in the Web edition", () => {
  it("wraps operator settings with the strong ETag without needing a native bridge", async () => {
    fetchMock.mockResolvedValueOnce(response(settings, 200, { ETag: etag }));
    expect(await getToolReviewSettings()).toEqual({
      settings: {
        ...settings,
        reject_threshold: 0.5,
        ask_on_review_error: false,
        approval_timeout_ms: 120000,
        batch_calls: false,
        backend: "jev",
      },
      etag,
    });
    expect(fetchMock).toHaveBeenCalledWith(
      "/control/v1/tool-review-settings",
      expect.objectContaining({
        method: "GET",
        credentials: "same-origin",
        redirect: "error",
        cache: "no-store",
      }),
    );
  });
  it("carries Responses backend, exact endpoint and replacement key through operator HTTP without echoing the key", async () => {
    const endpoint = "https://response.example/custom/exact/responses";
    const result = {
      ...settings,
      backend: "responses",
      endpoint,
      model: "schema-fixture-model",
      credential_configured: true,
    };
    fetchMock.mockResolvedValueOnce(response(result, 200, { ETag: etag }));
    const patch = {
      backend: "responses" as const,
      endpoint,
      model: "schema-fixture-model",
      api_key: "synthetic-new-responses-key",
      data_transfer_acknowledged: true,
    };
    const saved = await updateToolReviewSettings(etag, patch);
    expect(saved.settings.backend).toBe("responses");
    expect(saved.settings.endpoint).toBe(endpoint);
    expect(saved.settings).not.toHaveProperty("api_key");
    expect(fetchMock).toHaveBeenLastCalledWith(
      "/control/v1/tool-review-settings",
      expect.objectContaining({ method: "PATCH", body: JSON.stringify(patch) }),
    );
    fetchMock.mockClear();
    await expect(
      updateToolReviewSettings(etag, {
        backend: "openai",
      } as unknown as Parameters<typeof updateToolReviewSettings>[1]),
    ).rejects.toThrow("Invalid tool review settings");
    expect(fetchMock).not.toHaveBeenCalled();
  });
  it("reads and writes explicit batch mode through the real operator settings route", async () => {
    fetchMock.mockResolvedValueOnce(
      response({ ...settings, batch_calls: true }, 200, { ETag: etag }),
    );
    expect((await getToolReviewSettings()).settings.batch_calls).toBe(true);
    fetchMock.mockResolvedValueOnce(
      response({ ...settings, batch_calls: false }, 200, { ETag: etag }),
    );
    expect(
      (await updateToolReviewSettings(etag, { batch_calls: false })).settings
        .batch_calls,
    ).toBe(false);
    expect(fetchMock).toHaveBeenLastCalledWith(
      "/control/v1/tool-review-settings",
      expect.objectContaining({
        method: "PATCH",
        body: JSON.stringify({ batch_calls: false }),
      }),
    );
    fetchMock.mockClear();
    await expect(
      updateToolReviewSettings(etag, {
        batch_calls: "true",
      } as unknown as Parameters<typeof updateToolReviewSettings>[1]),
    ).rejects.toThrow("Invalid tool review settings");
    expect(fetchMock).not.toHaveBeenCalled();
  });
  it("saves from SafetyPolicy with in-app consent, If-Match and no returned credential", async () => {
    fetchMock.mockImplementation(async (path: string, options: RequestInit) => {
      if (path !== "/control/v1/tool-review-settings")
        return response({ error: { code: "unavailable" } }, 503);
      return response(
        { ...settings, enabled: options.method === "PATCH" },
        200,
        { ETag: etag },
      );
    });
    container = document.createElement("div");
    document.body.append(container);
    root = createRoot(container);
    await act(async () =>
      root!.render(<SafetyPolicy coreSessionKey="web-test" isReady />),
    );
    const tab = Array.from(
      container.querySelectorAll<HTMLButtonElement>('[role="tab"]'),
    ).find((item) => item.textContent === "工具审查")!;
    await act(async () =>
      tab.dispatchEvent(
        new MouseEvent("mousedown", { bubbles: true, button: 0 }),
      ),
    );
    const key = container.querySelector<HTMLInputElement>("#tool-review-key")!;
    await act(async () => {
      Object.getOwnPropertyDescriptor(
        HTMLInputElement.prototype,
        "value",
      )!.set!.call(key, "write-only-test-key");
      key.dispatchEvent(new Event("input", { bubbles: true }));
    });
    await act(async () =>
      container!
        .querySelector<HTMLButtonElement>("#tool-review-enabled")!
        .click(),
    );
    const save = Array.from(
      container.querySelectorAll<HTMLButtonElement>("button"),
    ).find((item) => item.textContent === "保存")!;
    await act(async () => save.click());
    expect(
      fetchMock.mock.calls.some(([, options]) => options.method === "PATCH"),
    ).toBe(false);
    const dialog = document.querySelector('[role="alertdialog"]')!;
    const consent = Array.from(
      dialog.querySelectorAll<HTMLButtonElement>("button"),
    ).find((item) => item.textContent === "同意外发并保存")!;
    await act(async () => consent.click());
    await settleClosedLayer();
    const saves = fetchMock.mock.calls.filter(
      ([, options]) => options.method === "PATCH",
    );
    expect(saves).toHaveLength(1);
    expect(saves[0][0]).toBe("/control/v1/tool-review-settings");
    expect(saves[0][1]).toMatchObject({
      credentials: "same-origin",
      redirect: "error",
      headers: {
        "Content-Type": "application/merge-patch+json",
        "If-Match": etag,
        "X-AstrLink-Console": "1",
      },
    });
    expect(JSON.parse(saves[0][1].body)).toMatchObject({
      enabled: true,
      api_key: "write-only-test-key",
      reject_threshold: 0.5,
      data_transfer_acknowledged: true,
    });
    expect(key.value).toBe("");
    expect(container.textContent).toContain("已保存");
  });
  it.each([
    [409, "tool_review_settings_conflict"],
    [412, "tool_review_settings_conflict"],
    [422, "tool_review_settings_rejected"],
    [500, "tool_review_settings_rejected"],
  ])(
    "maps HTTP %s to a safe stable error without retrying",
    async (status, code) => {
      fetchMock.mockResolvedValueOnce(
        response(
          {
            error: {
              code: "secret-key private prompt",
              message: "raw upstream error",
            },
          },
          Number(status),
        ),
      );
      await expect(
        updateToolReviewSettings(etag, { timeout_ms: 120000 }),
      ).rejects.toThrow(String(code));
      expect(fetchMock).toHaveBeenCalledOnce();
    },
  );
  it("does not leak a transport error or accept a secret-bearing response", async () => {
    fetchMock.mockRejectedValueOnce(
      new Error("private prompt in network error"),
    );
    await expect(
      updateToolReviewSettings(etag, { enabled: false }),
    ).rejects.toThrow("tool_review_settings_unavailable");
    expect(fetchMock).toHaveBeenCalledOnce();
    fetchMock.mockResolvedValueOnce(
      response({ ...settings, api_key: "private-key" }, 200, { ETag: etag }),
    );
    await expect(getToolReviewSettings()).rejects.toThrow(
      "tool_review_settings_unavailable",
    );
    fetchMock.mockResolvedValueOnce(response(settings));
    await expect(getToolReviewSettings()).rejects.toThrow(
      "tool_review_settings_unavailable",
    );
  });
  it("validates direct Web writes before fetch and still signals authentication expiry", async () => {
    await expect(
      webInvoke("update_tool_review_settings", { patch: { enabled: true } }),
    ).rejects.toThrow();
    await expect(
      webInvoke("update_tool_review_settings", {
        etag,
        patch: { secret_debug: "private" },
      }),
    ).rejects.toThrow();
    expect(fetchMock).not.toHaveBeenCalled();
    const signedOut = vi.fn();
    window.addEventListener(CONSOLE_SIGNED_OUT, signedOut);
    try {
      fetchMock.mockResolvedValueOnce(
        response({ error: { code: "login_required" } }, 401),
      );
      await expect(getToolReviewSettings()).rejects.toThrow(
        "tool_review_settings_rejected",
      );
      expect(signedOut).toHaveBeenCalledOnce();
    } finally {
      window.removeEventListener(CONSOLE_SIGNED_OUT, signedOut);
    }
  });
  it("keeps review status, repeated token filters and the encoded cursor on the HTTP request", async () => {
    fetchMock.mockResolvedValueOnce(response({ items: [], next_cursor: null }));
    await listRequestRecords({
      tool_review_status: "flagged",
      tool_review_only: true,
      limit: 50,
      cursor: "x+y",
      local_access_token_ids: ["token_a", "token_b"],
    });
    const url = new URL(fetchMock.mock.calls[0][0], "https://console.example");
    expect(url.pathname).toBe("/control/v1/requests");
    expect(url.searchParams.get("tool_review_status")).toBe("flagged");
    expect(url.searchParams.get("cursor")).toBe("x+y");
    expect(url.searchParams.getAll("local_access_token_id")).toEqual([
      "token_a",
      "token_b",
    ]);
    expect(url.searchParams.get("limit")).toBe("50");
    expect(url.searchParams.get("tool_review_only")).toBe("true");
    fetchMock.mockResolvedValueOnce(response({ items: [], next_cursor: null }));
    const sessionQuery = { limit: 50, tool_review_only: true };
    await listRequestSessions(sessionQuery);
    const sessionURL = new URL(
      fetchMock.mock.calls[1][0],
      "https://console.example",
    );
    expect(sessionURL.pathname).toBe("/control/v1/request-sessions");
    expect(sessionURL.searchParams.has("tool_review_only")).toBe(false);
  });
});
