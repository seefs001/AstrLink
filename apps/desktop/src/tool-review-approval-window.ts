import { getCurrentWindow } from "@tauri-apps/api/window";
import { isWebEdition } from "./edition";
import { openToolReviewApprovalNativeWindow } from "./bridge";

export const TOOL_REVIEW_APPROVAL_LABEL = "tool-review-approval";
export function isToolReviewApprovalWindow(): boolean {
  if (isWebEdition)
    return new URLSearchParams(window.location.search).has(
      "tool-review-approval",
    );
  try {
    return getCurrentWindow().label === TOOL_REVIEW_APPROVAL_LABEL;
  } catch {
    return false;
  }
}
export async function openToolReviewApprovalWindow(): Promise<void> {
  if (!isWebEdition) return openToolReviewApprovalNativeWindow();
  const url = new URL(window.location.href);
  url.search = "?tool-review-approval";
  url.hash = "";
  // Same-origin console authentication, no popup blocker or fake native success.
  window.location.assign(url.href);
}
