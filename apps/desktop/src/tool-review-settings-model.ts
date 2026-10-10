export type ToolReviewMode = "mark" | "reject";

export interface ToolReviewSettings {
  enabled: boolean;
  endpoint: string;
  model: string;
  prompt: string;
  mode: ToolReviewMode;
  timeout_ms: number;
  revision: string;
  credential_configured: boolean;
}

export interface ToolReviewSettingsRecord {
  settings: ToolReviewSettings;
  etag: string;
}

export interface ToolReviewSettingsPatch {
  enabled?: boolean;
  endpoint?: string;
  model?: string;
  prompt?: string;
  mode?: ToolReviewMode;
  timeout_ms?: number;
  api_key?: string;
  data_transfer_acknowledged?: boolean;
}

function invalid(): never {
  // Never include the received value: configuration can contain a credential.
  throw new Error("Invalid tool review settings");
}

function object(
  value: unknown,
  keys: readonly string[],
): Record<string, unknown> {
  if (typeof value !== "object" || value === null || Array.isArray(value))
    invalid();
  const result = value as Record<string, unknown>;
  if (Object.keys(result).some((key) => !keys.includes(key))) invalid();
  return result;
}

function text(value: unknown, max: number, empty = false): string {
  if (
    typeof value !== "string" ||
    (!empty && !value.trim()) ||
    new TextEncoder().encode(value).length > max
  )
    invalid();
  return value;
}

function boolean(value: unknown): boolean {
  if (typeof value !== "boolean") invalid();
  return value;
}

function endpoint(value: unknown): string {
  const result = text(value, 4096);
  let url: URL;
  try {
    url = new URL(result);
  } catch {
    return invalid();
  }
  if (
    !["http:", "https:"].includes(url.protocol) ||
    !url.hostname ||
    url.username ||
    url.password ||
    url.search ||
    url.hash ||
    result !== result.trim()
  )
    invalid();
  return result;
}

function prompt(value: unknown): string {
  const result = text(value, 128 << 10);
  if (!result.includes("{{user_request}}") || !result.includes("{{tool_call}}"))
    invalid();
  return result;
}

function singleLine(value: unknown, max: number, empty = false): string {
  const result = text(value, max, empty);
  if (
    Array.from(result).some(
      (char) => char.charCodeAt(0) < 32 || char.charCodeAt(0) === 127,
    )
  )
    invalid();
  return result;
}

// Mirrors core validateBoundedText: limits count code points and tab is allowed.
function boundedText(value: unknown, max: number): string {
  if (typeof value !== "string" || !value.trim()) invalid();
  const chars = Array.from(value);
  if (
    chars.length > max ||
    chars.some((char) => char.charCodeAt(0) < 32 && char !== "\t")
  )
    invalid();
  return value;
}

function mode(value: unknown): ToolReviewMode {
  if (value !== "mark" && value !== "reject") invalid();
  return value;
}

function timeout(value: unknown): number {
  if (
    typeof value !== "number" ||
    !Number.isInteger(value) ||
    value < 1 ||
    value > 120000
  )
    invalid();
  return value;
}

export function validateToolReviewETag(
  value: unknown,
): asserts value is string {
  if (typeof value !== "string" || !/^"sha256:[a-f0-9]{64}"$/.test(value))
    invalid();
}

export function parseToolReviewSettingsRecord(
  value: unknown,
): ToolReviewSettingsRecord {
  const record = object(value, ["settings", "etag"]);
  validateToolReviewETag(record.etag);
  const input = object(record.settings, [
    "enabled",
    "endpoint",
    "model",
    "prompt",
    "mode",
    "timeout_ms",
    "revision",
    "credential_configured",
  ]);
  if (input.enabled === true && input.credential_configured !== true) invalid();
  return {
    etag: record.etag,
    settings: {
      enabled: boolean(input.enabled),
      endpoint: endpoint(input.endpoint),
      model: boundedText(input.model, 256),
      prompt: prompt(input.prompt),
      mode: mode(input.mode),
      timeout_ms: timeout(input.timeout_ms),
      revision: boundedText(input.revision, 128),
      credential_configured: boolean(input.credential_configured),
    },
  };
}

export function validateToolReviewSettingsPatch(
  value: unknown,
): asserts value is ToolReviewSettingsPatch {
  const input = object(value, [
    "enabled",
    "endpoint",
    "model",
    "prompt",
    "mode",
    "timeout_ms",
    "api_key",
    "data_transfer_acknowledged",
  ]);
  if (!Object.keys(input).length) invalid();
  for (const [key, item] of Object.entries(input)) {
    switch (key) {
      case "enabled":
      case "data_transfer_acknowledged":
        boolean(item);
        break;
      case "endpoint":
        endpoint(item);
        break;
      case "model":
        boundedText(item, 256);
        break;
      case "prompt":
        prompt(item);
        break;
      case "mode":
        mode(item);
        break;
      case "timeout_ms":
        timeout(item);
        break;
      case "api_key": {
        const key = singleLine(item, 16384, true);
        if (key !== key.trim()) invalid();
        break;
      }
    }
  }
}
