import { Button } from "./components/ui/button";
import { ArrowUpRight } from "./components/icons";
import type { RequestRecord } from "./request-record-model";
import {
  canLocateReviewedTool,
  navigateToReviewedTool,
} from "./tool-review-navigation";
import { notify } from "./notify";
import { DataField, DataRow } from "./components/DataRow";
import { HelpDisclosure } from "./components/HelpDisclosure";
import { Panel, PanelBody, PanelHeader } from "./components/Panel";
import { StatusBadge } from "./components/StatusBadge";
import type { StatusTone } from "./components/StatusDot";
import { useT } from "./i18n";
import {
  hasVisibleToolReview,
  type ToolReviewSummary,
  type ToolReviewStatus,
  type ToolReviewApprovalState,
} from "./tool-review-record-model";

const tones: Record<ToolReviewStatus, StatusTone> = {
  pending: "pending",
  passed: "positive",
  flagged: "pending",
  blocked: "blocked",
  failed: "negative",
  interrupted: "neutral",
  cancelled: "neutral",
  not_applicable: "neutral",
};
const approvalTones: Record<ToolReviewApprovalState, StatusTone> = {
  pending: "pending",
  approved: "positive",
  denied: "blocked",
  expired: "neutral",
  cancelled: "neutral",
  interrupted: "neutral",
};
const errorMessages: Record<string, string> = {
  timeout: "timeout",
  collection_timeout: "timeout",
  queue_full: "busy",
  rate_limited: "busy",
  credential_missing: "credentials",
  unauthorized: "credentials",
  invalid_config: "configuration",
  transport_error: "unavailable",
  upstream_error: "unavailable",
  upstream_failed: "unavailable",
  invalid_response: "response",
  reviewer_invalid_json: "reviewerJson",
  reviewer_invalid_review: "reviewerReview",
  reviewer_invalid_reason: "reviewerReason",
  reviewer_invalid_model: "reviewerModel",
  incomplete_response: "response",
  encoded_response: "response",
  unsupported_response_format: "response",
  invalid_payload: "response",
  invalid_candidate_count: "response",
  invalid_candidates: "response",
  unexpected_tool_snapshot: "response",
  input_limit: "limit",
  buffer_limit: "limit",
  call_limit: "limit",
  invalid_input: "input",
  invalid_call: "input",
  invalid_arguments: "input",
  conflicting_call: "conflictingCall",
  incomplete_call: "input",
  unsupported_tool: "input",
  client_delivery_failed: "delivery",
  cancelled: "cancelled",
  interrupted: "interrupted",
};
function ReviewError({ code }: { code: string }) {
  const t = useT();
  // Unknown codes remain bounded identifiers, never arbitrary upstream text.
  const safe = /^[a-z][a-z0-9_]{0,63}$/.test(code);
  return (
    <span className="break-words text-danger-foreground">
      {t(
        `toolReview.errors.${safe && Object.hasOwn(errorMessages, code) ? errorMessages[code] : "unknown"}`,
      )}
      {safe ? (
        <code className="ml-1 break-all text-muted-foreground">({code})</code>
      ) : null}
    </span>
  );
}
const percent = (value: number) => `${Number((value * 100).toFixed(8))}%`;

export function ToolReviewBadge({ summary }: { summary: ToolReviewSummary }) {
  const t = useT();
  if (!hasVisibleToolReview(summary)) return null;
  const status = summary.approval?.review_status ?? summary.status;
  return (
    <StatusBadge tone={tones[status]} data-testid="tool-review-badge">
      {t(
        status === "flagged" && summary.mode === "ask"
          ? "toolReview.verdict.flag"
          : `toolReview.status.${status}`,
      )}
    </StatusBadge>
  );
}

export function ToolReviewDetails({
  summary,
  embedded = false,
  record,
}: {
  record?: RequestRecord;
  summary: ToolReviewSummary;
  embedded?: boolean;
}) {
  const t = useT();
  if (!hasVisibleToolReview(summary)) return null;
  const pending = summary.status === "pending" && !summary.approval;
  return (
    <Panel
      className={
        embedded ? "shrink-0 border-0 bg-transparent" : "mb-3 shrink-0"
      }
      data-testid="tool-review-details"
    >
      <PanelHeader
        size="sm"
        className={embedded ? "border-0 px-0 pt-0" : undefined}
      >
        <div className="flex flex-wrap items-center gap-2">
          {!embedded ? (
            <span className="text-sm font-medium">{t("toolReview.title")}</span>
          ) : null}
          {summary.approval ? (
            <span className="text-xs text-muted-foreground">
              {t("toolReview.reviewStatus")}:
            </span>
          ) : null}
          <ToolReviewBadge summary={summary} />
          {summary.mode === "ask" ||
          (summary.status !== "flagged" && summary.status !== "blocked") ? (
            <span className="text-xs text-muted-foreground">
              {t(
                summary.mode === "mark"
                  ? "toolReview.mark"
                  : summary.mode === "ask"
                    ? "toolReview.ask"
                    : "toolReview.reject",
              )}
            </span>
          ) : null}
        </div>
      </PanelHeader>
      <PanelBody
        className={
          embedded
            ? "space-y-3 overflow-visible p-0"
            : "space-y-3 overflow-visible p-3"
        }
      >
        {summary.approval ? (
          <div className="grid min-w-0 gap-2 sm:grid-cols-2">
            <DataField
              label={t("toolReview.humanDecision")}
              value={
                <StatusBadge
                  tone={approvalTones[summary.approval.state]}
                  data-testid="tool-review-human-decision"
                >
                  {t(`toolReview.approvalStates.${summary.approval.state}`)}
                </StatusBadge>
              }
            />
            <DataField
              label={t("toolReview.approvals.expires")}
              value={summary.approval.expires_at}
            />
          </div>
        ) : null}
        <div className="flex flex-wrap items-center gap-x-4 gap-y-1 text-xs">
          <span data-testid="tool-review-backend-record">
            {t("toolReview.backend")}:{" "}
            {summary.backend === undefined
              ? t("toolReview.notRecorded")
              : t(`toolReview.backends.${summary.backend}`)}
          </span>
          <span data-testid="tool-review-method">
            {t("toolReview.reviewMethod")}:{" "}
            {summary.batch_calls === undefined
              ? t("toolReview.notRecorded")
              : t(
                  summary.batch_calls
                    ? "toolReview.batchCalls"
                    : "toolReview.individualCalls",
                )}
          </span>
          <span>
            {t("toolReview.threshold")}:{" "}
            {summary.reject_threshold === undefined
              ? t("toolReview.notRecorded")
              : percent(summary.reject_threshold)}
          </span>
          <span className="text-muted-foreground">
            {t("toolReview.callCount", { count: summary.calls.length })}
          </span>
        </div>
        {summary.error_code ? (
          <p className="text-xs">
            <ReviewError code={summary.error_code} />
          </p>
        ) : null}
        {summary.calls.length === 0 ? (
          <p className="text-xs text-muted-foreground">
            {t("toolReview.noCalls")}
          </p>
        ) : (
          <div>
            {summary.calls.map((call) => (
              <DataRow className="flex-wrap gap-y-2 px-0 py-3" key={call.index}>
                <span className="min-w-0 flex-1 break-all font-mono text-sm">
                  #{call.index} {call.name || t("toolReview.unavailableValue")}
                </span>
                {record ? (
                  canLocateReviewedTool(record, call.index) ? (
                    <Button
                      size="xs"
                      variant="outline"
                      type="button"
                      data-testid="tool-review-locate"
                      data-review-index={call.index}
                      onClick={() => {
                        void navigateToReviewedTool(record, call.index).catch(
                          () =>
                            notify.error(t("toolReview.location.unreliable")),
                        );
                      }}
                    >
                      <ArrowUpRight aria-hidden="true" />
                      {t("toolReview.location.action")}
                    </Button>
                  ) : (
                    <span
                      className="text-xs text-muted-foreground"
                      data-testid="tool-review-location-unavailable"
                    >
                      {t("toolReview.location.unreliable")}
                    </span>
                  )
                ) : null}
                <StatusBadge
                  tone={
                    pending
                      ? "pending"
                      : call.verdict === "pass"
                        ? "positive"
                        : call.verdict === "flag"
                          ? "blocked"
                          : "negative"
                  }
                >
                  {t(
                    pending
                      ? "toolReview.status.pending"
                      : `toolReview.verdict.${call.verdict}`,
                  )}
                </StatusBadge>
                <div className="grid w-full min-w-0 gap-2 sm:grid-cols-2">
                  <DataField
                    label={t("toolReview.reason")}
                    value={
                      call.error_code ? (
                        <ReviewError code={call.error_code} />
                      ) : call.reason_code ? (
                        t(`toolReview.reasons.${call.reason_code}`)
                      ) : (
                        t("toolReview.reasonMissing")
                      )
                    }
                  />
                  <DataField
                    label={t("toolReview.delivery")}
                    value={t(
                      call.action === "withheld" &&
                        summary.mode === "reject" &&
                        call.verdict === "pass" &&
                        !pending
                        ? "toolReview.batchWithheld"
                        : call.action === "delivered" &&
                            call.verdict === "flag" &&
                            summary.mode === "mark" &&
                            !pending
                          ? "toolReview.markDelivered"
                          : call.action === "pending" &&
                              !pending &&
                              summary.approval?.state !== "pending"
                            ? "toolReview.deliveryUnconfirmed"
                            : `toolReview.action.${call.action}`,
                    )}
                  />
                  <DataField
                    label={t(
                      summary.backend === "responses"
                        ? "toolReview.riskScore"
                        : "toolReview.riskProbability",
                    )}
                    value={
                      call.flag_probability === undefined
                        ? t("toolReview.notRecorded")
                        : percent(call.flag_probability)
                    }
                  />
                  <DataField
                    label={t("toolReview.decisionBasis")}
                    value={
                      pending
                        ? t("toolReview.status.pending")
                        : call.verdict === "error"
                          ? t("toolReview.noDecision")
                          : summary.reject_threshold === undefined ||
                              call.flag_probability === undefined
                            ? t("toolReview.basisMissing")
                            : t(
                                call.flag_probability >=
                                  summary.reject_threshold
                                  ? "toolReview.atThreshold"
                                  : "toolReview.belowThreshold",
                                {
                                  risk: percent(call.flag_probability),
                                  threshold: percent(summary.reject_threshold),
                                },
                              )
                    }
                  />
                </div>
              </DataRow>
            ))}
          </div>
        )}
        <HelpDisclosure title={t("toolReview.technicalDetails")}>
          <p>{t("toolReview.reasonHint")}</p>
          {summary.backend === "responses" ? (
            <p>{t("toolReview.responsesScoreHint")}</p>
          ) : null}
          <div className="grid min-w-0 gap-2 sm:grid-cols-2">
            <DataField
              label={t("toolReview.model")}
              value={<span className="break-all">{summary.model}</span>}
            />
            <DataField
              label={t("toolReview.revision")}
              value={
                <span className="break-all font-mono">
                  {summary.config_revision || t("toolReview.unavailableValue")}
                </span>
              }
            />
            <DataField
              label={t("toolReview.startedAt")}
              value={summary.started_at}
            />
            <DataField
              label={t("toolReview.completedAt")}
              value={summary.completed_at ?? t("toolReview.notRecorded")}
            />
          </div>
          <p>
            {t("toolReview.duration")}: {summary.latency_ms} ms
          </p>
          {summary.calls.map((call) => (
            <div
              key={call.index}
              className="flex min-w-0 flex-wrap gap-x-3 gap-y-1 break-all"
            >
              <span>
                #{call.index} {call.model || t("toolReview.unavailableValue")} ·{" "}
                {call.latency_ms} ms
              </span>
              <span>
                {t("toolReview.modelVerdict")}:{" "}
                {call.model_verdict
                  ? t(`toolReview.verdict.${call.model_verdict}`)
                  : t("toolReview.notRecorded")}
              </span>
              {call.pass_probability !== undefined ? (
                <span>
                  {t(
                    summary.backend === "responses"
                      ? "toolReview.safeScore"
                      : "toolReview.safeProbability",
                  )}
                  : {percent(call.pass_probability)}
                </span>
              ) : null}
            </div>
          ))}
        </HelpDisclosure>
      </PanelBody>
    </Panel>
  );
}
