import { describe, expect, it } from "vitest";
import {
  parseToolReviewSettingsRecord,
  validateToolReviewSettingsPatch,
} from "./tool-review-settings-model";

const settings = {
  enabled: false,
  endpoint: "https://api.typesafe.ai/v1/systemone",
  model: "jev-latest",
  prompt: "Request: {{user_request}}\nTool: {{tool_call}}",
  mode: "mark",
  timeout_ms: 5000,
  revision: "revision-1",
  credential_configured: true,
};
const etag = '"sha256:' + "a".repeat(64) + '"';

describe("tool review operator settings", () => {
  it("keeps the revision and strong ETag without accepting secrets or unknown fields", () => {
    expect(parseToolReviewSettingsRecord({ settings, etag })).toEqual({
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
    for (const extra of [{ api_key: "secret" }, { prompt_debug: "private" }]) {
      expect(() =>
        parseToolReviewSettingsRecord({
          settings: { ...settings, ...extra },
          etag,
        }),
      ).toThrow();
    }
    expect(() =>
      parseToolReviewSettingsRecord({ settings, etag: 'W/"old"' }),
    ).toThrow();
  });
  it("defaults legacy backend to Jev and strictly accepts only the two frozen backends", () => {
    expect(
      parseToolReviewSettingsRecord({ settings, etag }).settings.backend,
    ).toBe("jev");
    for (const backend of ["jev", "responses"]) {
      expect(
        parseToolReviewSettingsRecord({
          settings: { ...settings, backend },
          etag,
        }).settings.backend,
      ).toBe(backend);
      expect(() => validateToolReviewSettingsPatch({ backend })).not.toThrow();
    }
    for (const backend of [null, "", "openai", "Responses", true, 0, {}, []]) {
      expect(() =>
        parseToolReviewSettingsRecord({
          settings: { ...settings, backend },
          etag,
        }),
      ).toThrow("Invalid tool review settings");
      expect(() => validateToolReviewSettingsPatch({ backend })).toThrow(
        "Invalid tool review settings",
      );
    }
  });
  it("defaults legacy review method to individual and strictly accepts boolean batch settings", () => {
    expect(
      parseToolReviewSettingsRecord({ settings, etag }).settings.batch_calls,
    ).toBe(false);
    for (const batch_calls of [false, true]) {
      expect(
        parseToolReviewSettingsRecord({
          settings: { ...settings, batch_calls },
          etag,
        }).settings.batch_calls,
      ).toBe(batch_calls);
      expect(() =>
        validateToolReviewSettingsPatch({ batch_calls }),
      ).not.toThrow();
    }
    for (const batch_calls of [null, 0, 1, "false", "true", [], {}]) {
      expect(() =>
        parseToolReviewSettingsRecord({
          settings: { ...settings, batch_calls },
          etag,
        }),
      ).toThrow("Invalid tool review settings");
      expect(() => validateToolReviewSettingsPatch({ batch_calls })).toThrow(
        "Invalid tool review settings",
      );
    }
  });
  it("defaults only a missing threshold and validates inclusive boundaries for reads and writes", () => {
    for (const reject_threshold of [0, 0.5, 1]) {
      expect(
        parseToolReviewSettingsRecord({
          settings: { ...settings, reject_threshold },
          etag,
        }).settings.reject_threshold,
      ).toBe(reject_threshold);
      expect(() =>
        validateToolReviewSettingsPatch({ reject_threshold }),
      ).not.toThrow();
    }
    for (const reject_threshold of [null, "0.5", NaN, Infinity, -0.01, 1.01]) {
      expect(() =>
        parseToolReviewSettingsRecord({
          settings: { ...settings, reject_threshold },
          etag,
        }),
      ).toThrow();
      expect(() =>
        validateToolReviewSettingsPatch({ reject_threshold }),
      ).toThrow();
    }
  });
  it("accepts ask mode and defaults only missing approval settings", () => {
    const parsed = parseToolReviewSettingsRecord({
      settings: { ...settings, mode: "ask" },
      etag,
    });
    expect(parsed.settings.mode).toBe("ask");
    expect(parsed.settings.ask_on_review_error).toBe(false);
    expect(parsed.settings.approval_timeout_ms).toBe(120000);
    expect(() =>
      validateToolReviewSettingsPatch({ mode: "ask" }),
    ).not.toThrow();
    for (const ask_on_review_error of [true, false]) {
      expect(
        parseToolReviewSettingsRecord({
          settings: { ...settings, ask_on_review_error },
          etag,
        }).settings.ask_on_review_error,
      ).toBe(ask_on_review_error);
      expect(() =>
        validateToolReviewSettingsPatch({ ask_on_review_error }),
      ).not.toThrow();
    }
    for (const approval_timeout_ms of [1000, 120000, 600000]) {
      expect(
        parseToolReviewSettingsRecord({
          settings: { ...settings, approval_timeout_ms },
          etag,
        }).settings.approval_timeout_ms,
      ).toBe(approval_timeout_ms);
      expect(() =>
        validateToolReviewSettingsPatch({ approval_timeout_ms }),
      ).not.toThrow();
    }
    for (const patch of [
      ...[null, "false", 0].map((ask_on_review_error) => ({
        ask_on_review_error,
      })),
      ...[null, "120000", 999, 600001, 1000.5, NaN, Infinity].map(
        (approval_timeout_ms) => ({ approval_timeout_ms }),
      ),
    ]) {
      expect(() =>
        parseToolReviewSettingsRecord({
          settings: { ...settings, ...patch },
          etag,
        }),
      ).toThrow("Invalid tool review settings");
      expect(() => validateToolReviewSettingsPatch(patch)).toThrow(
        "Invalid tool review settings",
      );
    }
  });
  it("bounds model names by characters like core, allowing tab", () => {
    for (const model of ["模".repeat(256), "jev\tlatest"]) {
      expect(
        parseToolReviewSettingsRecord({
          settings: { ...settings, model },
          etag,
        }).settings.model,
      ).toBe(model);
      expect(() => validateToolReviewSettingsPatch({ model })).not.toThrow();
    }
    for (const model of ["模".repeat(257), "jev\nlatest", " "]) {
      expect(() => validateToolReviewSettingsPatch({ model })).toThrow();
    }
  });
  it("rejects unsafe endpoint, invalid choices and missing prompt variables", () => {
    for (const change of [
      { endpoint: "https://user:secret@example.com/review" },
      { endpoint: "https://example.com/review?key=secret" },
      { mode: "threshold" },
      { timeout_ms: 120001 },
      { prompt: "{{tool_call}}" },
      { credential_configured: "yes" },
      { enabled: true, credential_configured: false },
    ]) {
      expect(() =>
        parseToolReviewSettingsRecord({
          settings: { ...settings, ...change },
          etag,
        }),
      ).toThrow();
    }
  });
  it("accepts a 1–120000 ms timeout, keeping 5000 as a valid default rather than a ceiling", () => {
    for (const timeout_ms of [1, 5000, 5001, 120000]) {
      expect(
        parseToolReviewSettingsRecord({
          settings: { ...settings, timeout_ms },
          etag,
        }).settings.timeout_ms,
      ).toBe(timeout_ms);
      expect(() =>
        validateToolReviewSettingsPatch({ timeout_ms }),
      ).not.toThrow();
    }
    for (const timeout_ms of [0, 120001, 1.5, Infinity, "5000"]) {
      expect(() =>
        parseToolReviewSettingsRecord({
          settings: { ...settings, timeout_ms },
          etag,
        }),
      ).toThrow();
      expect(() => validateToolReviewSettingsPatch({ timeout_ms })).toThrow();
    }
  });
  it("allows a write-only credential patch and explicit acknowledgement, not arbitrary data", () => {
    expect(() =>
      validateToolReviewSettingsPatch({
        api_key: "new-key",
        enabled: true,
        data_transfer_acknowledged: true,
      }),
    ).not.toThrow();
    expect(() =>
      validateToolReviewSettingsPatch({ api_key: "" }),
    ).not.toThrow();
    expect(() =>
      validateToolReviewSettingsPatch({ revision: "forged" }),
    ).toThrow();
    expect(() =>
      validateToolReviewSettingsPatch({ api_key: "secret\nheader" }),
    ).toThrow();
  });
});
