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
      tool_review_only: true,
      limit: 50,
      cursor: "next",
    });
    expect(invoke).toHaveBeenLastCalledWith("list_request_records", {
      query: {
        tool_review_status: "flagged",
        tool_review_only: true,
        limit: 50,
        cursor: "next",
      },
    });
    await listRequestRecords();
    expect(invoke).toHaveBeenLastCalledWith("list_request_records", {
      query: {},
    });
    const sessionQuery = {
      limit: 50,
      tool_review_only: true,
      tool_review_status: "flagged",
    };
    await listRequestSessions(sessionQuery);
    expect(invoke).toHaveBeenLastCalledWith("list_request_sessions", {
      query: { limit: 50 },
    });
  });
  it("reads real native settings and carries If-Match plus acknowledgement on save", async () => {
    Reflect.set(window, "__TAURI_INTERNALS__", {});
    invoke.mockResolvedValue(record);
    expect(await getToolReviewSettings()).toEqual({
      ...record,
      settings: {
        ...record.settings,
        reject_threshold: 0.5,
        ask_on_review_error: false,
        approval_timeout_ms: 120000,
        batch_calls: false,
        backend: "jev",
      },
    });
    expect(invoke).toHaveBeenLastCalledWith("get_tool_review_settings");
    await updateToolReviewSettings(record.etag, {
      enabled: true,
      backend: "responses",
      api_key: "write-only",
      batch_calls: true,
      reject_threshold: 0.75,
      data_transfer_acknowledged: true,
    });
    expect(invoke).toHaveBeenLastCalledWith("update_tool_review_settings", {
      etag: record.etag,
      patch: {
        enabled: true,
        backend: "responses",
        api_key: "write-only",
        batch_calls: true,
        reject_threshold: 0.75,
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
