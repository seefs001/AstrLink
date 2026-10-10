import { DataField, DataRow } from "./components/DataRow";
import { Panel, PanelBody, PanelHeader } from "./components/Panel";
import { StatusBadge } from "./components/StatusBadge";
import type { StatusTone } from "./components/StatusDot";
import { useT } from "./i18n";
import type {
  ToolReviewSummary,
  ToolReviewStatus,
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
export function ToolReviewBadge({ summary }: { summary: ToolReviewSummary }) {
  const t = useT();
  return (
    <StatusBadge tone={tones[summary.status]} data-testid="tool-review-badge">
      {t(`toolReview.status.${summary.status}`)}
    </StatusBadge>
  );
}

export function ToolReviewDetails({ summary }: { summary: ToolReviewSummary }) {
  const t = useT();
  return (
    <Panel className="mb-3 shrink-0" data-testid="tool-review-details">
      <PanelHeader size="sm">
        <div className="flex flex-wrap items-center gap-2">
          <span className="text-sm font-medium">{t("toolReview.title")}</span>
          <ToolReviewBadge summary={summary} />
          <span className="text-xs text-muted-foreground">
            {t(
              summary.mode === "mark" ? "toolReview.mark" : "toolReview.reject",
            )}{" "}
            · {summary.latency_ms} ms
          </span>
        </div>
      </PanelHeader>
      <PanelBody className="space-y-3 overflow-visible p-3">
        <div className="grid gap-2 sm:grid-cols-2">
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
            value={summary.completed_at ?? t("toolReview.unavailableValue")}
          />
        </div>
        {summary.error_code ? (
          <p className="break-all text-xs text-danger-foreground">
            {t("toolReview.safeError")}: <code>{summary.error_code}</code>
          </p>
        ) : null}
        {summary.mode === "mark" ? (
          <p className="text-xs text-muted-foreground">
            {t("toolReview.markHint")}
          </p>
        ) : null}
        {summary.calls.length === 0 ? (
          <p className="text-xs text-muted-foreground">
            {t("toolReview.noCalls")}
          </p>
        ) : (
          <div>
            {summary.calls.map((call) => (
              <DataRow className="flex-wrap px-0 py-2" key={call.index}>
                <span className="break-all font-mono text-sm">
                  #{call.index} {call.name || t("toolReview.unavailableValue")}
                </span>
                <StatusBadge
                  tone={
                    call.verdict === "pass"
                      ? "positive"
                      : call.verdict === "flag"
                        ? "pending"
                        : "negative"
                  }
                >
                  {t(`toolReview.verdict.${call.verdict}`)}
                </StatusBadge>
                <span className="text-xs">
                  {t(
                    call.action === "withheld" && call.verdict === "pass"
                      ? "toolReview.batchWithheld"
                      : // A finished review whose delivery was never confirmed
                        // may have reached the client only in part.
                        call.action === "pending" &&
                          summary.status !== "pending"
                        ? "toolReview.deliveryUnconfirmed"
                        : `toolReview.action.${call.action}`,
                  )}
                </span>
                <span className="text-xs text-muted-foreground">
                  {call.latency_ms} ms ·{" "}
                  {call.model || t("toolReview.unavailableValue")}
                </span>
                <div className="flex w-full flex-wrap gap-x-3 text-xs text-muted-foreground">
                  {call.pass_probability !== undefined ? (
                    <span>
                      {t("toolReview.verdict.pass")}:{" "}
                      {(call.pass_probability * 100).toFixed(1)}%
                    </span>
                  ) : null}
                  {call.flag_probability !== undefined ? (
                    <span>
                      {t("toolReview.verdict.flag")}:{" "}
                      {(call.flag_probability * 100).toFixed(1)}%
                    </span>
                  ) : null}
                  {call.error_code ? (
                    <span>
                      {t("toolReview.safeError")}:{" "}
                      <code>{call.error_code}</code>
                    </span>
                  ) : null}
                </div>
              </DataRow>
            ))}
          </div>
        )}
      </PanelBody>
    </Panel>
  );
}
