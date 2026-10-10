import { invoke } from "@tauri-apps/api/core";
import { isTrajectoryInspectorWindow } from "./trajectory-inspector-window";
import { emitTo } from "@tauri-apps/api/event";
import type { RequestRecord } from "./request-record-model";

export const TOOL_REVIEW_NAVIGATE_EVENT = "trajectory-inspector:locate-tool";

/** An ephemeral intent, never a body, provider call id, or persisted setting. */
export interface ToolReviewJump {
  version: 1;
  target: "main";
  sessionId: string;
  requestId: string;
  reviewIndex: number;
  nonce: string;
}

const safeId = (value: unknown): value is string =>
  typeof value === "string" &&
  /^[a-zA-Z0-9][a-zA-Z0-9_.:-]{0,191}$/.test(value);

export function parseToolReviewJump(value: unknown): ToolReviewJump | null {
  if (!value || typeof value !== "object" || Array.isArray(value)) return null;
  const data = value as Record<string, unknown>;
  if (
    Object.keys(data).sort().join(",") !==
      "nonce,requestId,reviewIndex,sessionId,target,version" ||
    data.version !== 1 ||
    data.target !== "main" ||
    !safeId(data.sessionId) ||
    !safeId(data.requestId) ||
    !safeId(data.nonce) ||
    !Number.isSafeInteger(data.reviewIndex) ||
    Number(data.reviewIndex) < 0 ||
    Number(data.reviewIndex) >= 256
  )
    return null;
  return data as unknown as ToolReviewJump;
}

export function canLocateReviewedTool(
  record: RequestRecord,
  index: number,
): boolean {
  const calls = record.tool_review?.calls ?? [];
  return (
    safeId(record.id) &&
    safeId(record.session_id) &&
    Number.isSafeInteger(index) &&
    index >= 0 &&
    index < 256 &&
    calls.filter((call) => call.index === index).length === 1
  );
}

export async function navigateToReviewedTool(
  record: RequestRecord,
  index: number,
): Promise<void> {
  if (!canLocateReviewedTool(record, index))
    throw new Error("Tool location unavailable");
  const jump: ToolReviewJump = {
    version: 1,
    target: "main",
    sessionId: record.session_id!,
    requestId: record.id,
    reviewIndex: index,
    nonce: crypto.randomUUID(),
  };
  if (isTrajectoryInspectorWindow()) {
    // Main navigation destroys unpinned inspectors. Finish the inspector-only
    // host command first, while this window still exists.
    await invoke<void>("focus_main_window");
    await emitTo("main", TOOL_REVIEW_NAVIGATE_EVENT, jump);
  } else {
    window.dispatchEvent(
      new CustomEvent(TOOL_REVIEW_NAVIGATE_EVENT, { detail: jump }),
    );
  }
}
