import {
  toolReviewBackends,
  type ToolReviewBackend,
  type ToolReviewMode,
} from "./tool-review-settings-model";

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
export const toolReviewReasons = [
  "authorized",
  "unclear_authorization",
  "unauthorized_action",
  "destructive_action",
  "data_exfiltration",
  "privilege_escalation",
  "untrusted_execution",
  "prompt_injection",
  "unrelated_action",
  "policy_violation",
] as const;
export type ToolReviewReason = (typeof toolReviewReasons)[number];
export interface ToolReviewCall {
  index: number;
  name: string;
  verdict: "pass" | "flag" | "error";
  model_verdict?: "pass" | "flag";
  reason_code?: ToolReviewReason;
  pass_probability?: number;
  flag_probability?: number;
  action: "delivered" | "withheld" | "pending";
  latency_ms: number;
  model: string;
  error_code?: string;
}
export const toolReviewApprovalStates = [
  "pending",
  "approved",
  "denied",
  "expired",
  "cancelled",
  "interrupted",
] as const;
export type ToolReviewApprovalState = (typeof toolReviewApprovalStates)[number];
export interface ToolReviewApproval {
  id: string;
  state: ToolReviewApprovalState;
  review_status: "flagged" | "failed";
  requested_at: string;
  expires_at: string;
  decided_at?: string;
}
export interface ToolReviewSummary {
  backend?: ToolReviewBackend;
  status: ToolReviewStatus;
  mode: ToolReviewMode;
  batch_calls?: boolean;
  model: string;
  config_revision: string;
  reject_threshold?: number;
  started_at: string;
  completed_at?: string;
  latency_ms: number;
  calls: ToolReviewCall[];
  approval?: ToolReviewApproval;
  error_code?: string;
}
/** Historical placeholders do not represent an attempted tool review. */
export function hasVisibleToolReview(
  summary: ToolReviewSummary | null | undefined,
): summary is ToolReviewSummary {
  return (
    !!summary &&
    summary.status !== "not_applicable" &&
    (summary.status !== "pending" ||
      summary.calls.length > 0 ||
      summary.approval !== undefined)
  );
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
  const [year, month, day, hour, minute, second] = result
    .slice(0, 19)
    .split(/[-T:]/)
    .map(Number);
  const leapYear = year % 4 === 0 && (year % 100 !== 0 || year % 400 === 0);
  const daysInMonth = [
    31,
    leapYear ? 29 : 28,
    31,
    30,
    31,
    30,
    31,
    31,
    30,
    31,
    30,
    31,
  ];
  if (
    year < 1 ||
    month < 1 ||
    month > 12 ||
    day < 1 ||
    day > daysInMonth[month - 1] ||
    hour > 23 ||
    minute > 59 ||
    second > 59
  )
    invalid();
  return result;
}
// Preserve RFC3339 nanoseconds when comparing Go timestamps. Date.parse alone
// truncates sub-millisecond precision and can accept a decision before a request.
function instant(value: string): bigint {
  const fraction = /\.(\d+)/.exec(value)?.[1] ?? "";
  return (
    BigInt(Date.parse(value.replace(/\.\d+/, ""))) * 1000000n +
    BigInt(fraction.padEnd(9, "0").slice(0, 9))
  );
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
export function parseToolReviewApproval(value: unknown): ToolReviewApproval {
  const approval = object(value, [
    "id",
    "state",
    "review_status",
    "requested_at",
    "expires_at",
    "decided_at",
  ]);
  if (
    typeof approval.id !== "string" ||
    !/^[a-zA-Z0-9_-]{1,128}$/.test(approval.id)
  )
    invalid();
  const state = choice(approval.state, toolReviewApprovalStates);
  const requestedAt = timestamp(approval.requested_at);
  const expiresAt = timestamp(approval.expires_at);
  const decidedAt =
    approval.decided_at === undefined
      ? undefined
      : timestamp(approval.decided_at);
  if (
    instant(requestedAt) === instant("0001-01-01T00:00:00Z") ||
    instant(expiresAt) <= instant(requestedAt) ||
    (state === "pending"
      ? decidedAt !== undefined
      : decidedAt === undefined || instant(decidedAt) < instant(requestedAt))
  )
    invalid();
  return {
    id: approval.id,
    state,
    review_status: choice(approval.review_status, ["flagged", "failed"]),
    requested_at: requestedAt,
    expires_at: expiresAt,
    ...(decidedAt === undefined ? {} : { decided_at: decidedAt }),
  };
}

export function parseToolReviewSummary(value: unknown): ToolReviewSummary {
  const summary = object(value, [
    "backend",
    "status",
    "mode",
    "model",
    "config_revision",
    "batch_calls",
    "reject_threshold",
    "started_at",
    "completed_at",
    "latency_ms",
    "calls",
    "approval",
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
  if (completedAt !== undefined && instant(completedAt) < instant(startedAt))
    invalid();
  const mode = choice(summary.mode, ["mark", "reject", "ask"] as const);
  const approval =
    summary.approval === undefined
      ? undefined
      : parseToolReviewApproval(summary.approval);
  if (approval !== undefined && mode !== "ask") invalid();
  if (
    summary.batch_calls !== undefined &&
    typeof summary.batch_calls !== "boolean"
  )
    invalid();
  return {
    status: choice(summary.status, toolReviewStatuses),
    ...(summary.backend === undefined
      ? {}
      : { backend: choice(summary.backend, toolReviewBackends) }),
    mode,
    ...(summary.batch_calls === undefined
      ? {}
      : { batch_calls: summary.batch_calls }),
    ...(approval === undefined ? {} : { approval }),
    model: text(summary.model, 256, false),
    config_revision: text(summary.config_revision, 128),
    ...(summary.reject_threshold === undefined
      ? {}
      : { reject_threshold: probability(summary.reject_threshold) }),
    started_at: startedAt,
    ...(completedAt === undefined ? {} : { completed_at: completedAt }),
    latency_ms: integer(summary.latency_ms),
    ...errorCode(summary.error_code),
    calls: calls.map((value) => {
      const call = object(value, [
        "index",
        "name",
        "verdict",
        "model_verdict",
        "reason_code",
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
        ...(call.model_verdict === undefined
          ? {}
          : {
              model_verdict: choice(call.model_verdict, [
                "pass",
                "flag",
              ] as const),
            }),
        ...(call.reason_code === undefined
          ? {}
          : { reason_code: choice(call.reason_code, toolReviewReasons) }),
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
