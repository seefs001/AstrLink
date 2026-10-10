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
      settings,
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
