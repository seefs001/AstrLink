import type { ToolReviewMode } from "./tool-review-settings-model";

export const toolReviewStatuses = [
  "pending",
  "passed",
  "flagged",
  "blocked",
  "failed",
  "interrupted",
  "cancelled",
  "not_applicable",
] as const;
export type ToolReviewStatus = (typeof toolReviewStatuses)[number];
export interface ToolReviewCall {
  index: number;
  name: string;
  verdict: "pass" | "flag" | "error";
  pass_probability?: number;
  flag_probability?: number;
  action: "delivered" | "withheld" | "pending";
  latency_ms: number;
  model: string;
  error_code?: string;
}
export interface ToolReviewSummary {
  status: ToolReviewStatus;
  mode: ToolReviewMode;
  model: string;
  config_revision: string;
  started_at: string;
  completed_at?: string;
  latency_ms: number;
  calls: ToolReviewCall[];
  error_code?: string;
}
function invalid(): never {
  throw new Error("Invalid tool review metadata");
}
function object(value: unknown, keys: string[]): Record<string, unknown> {
  if (!value || typeof value !== "object" || Array.isArray(value)) invalid();
  if (Object.keys(value).some((key) => !keys.includes(key))) invalid();
  return value as Record<string, unknown>;
}
function choice<T extends string>(value: unknown, choices: readonly T[]): T {
  if (typeof value !== "string" || !choices.includes(value as T)) invalid();
  return value as T;
}
function integer(value: unknown): number {
  if (typeof value !== "number" || !Number.isSafeInteger(value) || value < 0)
    invalid();
  return value;
}
// Mirrors core validateBoundedText: limits count code points and tab is allowed.
function text(value: unknown, max: number, empty = true): string {
  if (typeof value !== "string" || (!empty && !value.trim())) invalid();
  const chars = Array.from(value);
  if (
    chars.length > max ||
    chars.some((char) => char.charCodeAt(0) < 32 && char !== "\t")
  )
    invalid();
  return value;
}
function timestamp(value: unknown): string {
  const result = text(value, 64, false);
  if (
    !/^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:Z|[+-]\d{2}:\d{2})$/.test(
      result,
    ) ||
    !Number.isFinite(Date.parse(result))
  )
    invalid();
  return result;
}
function errorCode(value: unknown): { error_code?: string } {
  if (value === undefined) return {};
  if (
    typeof value !== "string" ||
    (value !== "" && !/^[a-z][a-z0-9_]{0,63}$/.test(value))
  )
    invalid();
  return { error_code: value };
}
function probability(value: unknown): number {
  if (
    typeof value !== "number" ||
    !Number.isFinite(value) ||
    value < 0 ||
    value > 1
  )
    invalid();
  return value;
}
export function parseToolReviewSummary(value: unknown): ToolReviewSummary {
  const summary = object(value, [
    "status",
    "mode",
    "model",
    "config_revision",
    "started_at",
    "completed_at",
    "latency_ms",
    "calls",
    "error_code",
  ]);
  const calls = summary.calls === null ? [] : summary.calls;
  if (!Array.isArray(calls) || calls.length > 64) invalid();
  const seen = new Set<number>();
  const startedAt = timestamp(summary.started_at);
  const completedAt =
    summary.completed_at === undefined
      ? undefined
      : timestamp(summary.completed_at);
  if (
    completedAt !== undefined &&
    Date.parse(completedAt) < Date.parse(startedAt)
  )
    invalid();
  return {
    status: choice(summary.status, toolReviewStatuses),
    mode: choice(summary.mode, ["mark", "reject"]),
    model: text(summary.model, 256, false),
    config_revision: text(summary.config_revision, 128),
    started_at: startedAt,
    ...(completedAt === undefined ? {} : { completed_at: completedAt }),
    latency_ms: integer(summary.latency_ms),
    ...errorCode(summary.error_code),
    calls: calls.map((value) => {
      const call = object(value, [
        "index",
        "name",
        "verdict",
        "pass_probability",
        "flag_probability",
        "action",
        "latency_ms",
        "model",
        "error_code",
      ]);
      const index = integer(call.index);
      if (seen.has(index)) invalid();
      seen.add(index);
      return {
        index,
        name: text(call.name, 256),
        verdict: choice(call.verdict, ["pass", "flag", "error"]),
        action: choice(call.action, ["delivered", "withheld", "pending"]),
        latency_ms: integer(call.latency_ms),
        model: text(call.model, 256),
        ...(call.pass_probability === undefined
          ? {}
          : { pass_probability: probability(call.pass_probability) }),
        ...(call.flag_probability === undefined
          ? {}
          : { flag_probability: probability(call.flag_probability) }),
        ...errorCode(call.error_code),
      };
    }),
  };
}
