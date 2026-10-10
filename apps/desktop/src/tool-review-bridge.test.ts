// @vitest-environment happy-dom
import { afterEach, describe, expect, it, vi } from "vitest";
const invoke = vi.hoisted(() => vi.fn());
vi.mock("@tauri-apps/api/core", () => ({ invoke }));
import {
  getToolReviewSettings,
  updateToolReviewSettings,
  listRequestRecords,
  listRequestSessions,
} from "./bridge";

const record = {
  settings: {
    enabled: false,
    endpoint: "https://api.typesafe.ai/v1/systemone",
    model: "jev-latest",
    prompt: "{{user_request}}\n{{tool_call}}",
    mode: "mark",
    timeout_ms: 5000,
    revision: "revision-1",
    credential_configured: true,
  },
  etag: '"sha256:' + "a".repeat(64) + '"',
};
afterEach(() => {
  invoke.mockReset();
  Reflect.deleteProperty(window, "__TAURI_INTERNALS__");
});
describe("tool review IPC", () => {
  it("forwards the review filter only on request queries", async () => {
    Reflect.set(window, "__TAURI_INTERNALS__", {});
    invoke.mockResolvedValue({ items: [], next_cursor: null });
    await listRequestRecords({
      tool_review_status: "flagged",
      limit: 50,
      cursor: "next",
    });
    expect(invoke).toHaveBeenLastCalledWith("list_request_records", {
      query: { tool_review_status: "flagged", limit: 50, cursor: "next" },
    });
    await listRequestRecords();
    expect(invoke).toHaveBeenLastCalledWith("list_request_records", {
      query: {},
    });
    await listRequestSessions({ limit: 50 });
    expect(invoke).toHaveBeenLastCalledWith("list_request_sessions", {
      query: { limit: 50 },
    });
  });
  it("reads real native settings and carries If-Match plus acknowledgement on save", async () => {
    Reflect.set(window, "__TAURI_INTERNALS__", {});
    invoke.mockResolvedValue(record);
    expect(await getToolReviewSettings()).toEqual(record);
    expect(invoke).toHaveBeenLastCalledWith("get_tool_review_settings");
    await updateToolReviewSettings(record.etag, {
      enabled: true,
      api_key: "write-only",
      data_transfer_acknowledged: true,
    });
    expect(invoke).toHaveBeenLastCalledWith("update_tool_review_settings", {
      etag: record.etag,
      patch: {
        enabled: true,
        api_key: "write-only",
        data_transfer_acknowledged: true,
      },
    });
    invoke.mockClear();
    await expect(
      updateToolReviewSettings('W/"old"', { enabled: true }),
    ).rejects.toThrow();
    expect(invoke).not.toHaveBeenCalled();
  });
  it("rejects secret-bearing responses and never pretends a browser saved native settings", async () => {
    await expect(getToolReviewSettings()).rejects.toThrow();
    Reflect.set(window, "__TAURI_INTERNALS__", {});
    invoke.mockResolvedValue({
      ...record,
      settings: { ...record.settings, api_key: "secret" },
    });
    await expect(getToolReviewSettings()).rejects.toThrow();
  });
});
