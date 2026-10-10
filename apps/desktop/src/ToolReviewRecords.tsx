import { useEffect, useRef, useState } from "react";
import { listRequestRecords, getRequestRecord } from "./bridge";
import { DataRow } from "./components/DataRow";
import { EmptyState } from "./components/EmptyState";
import { FilterSelect } from "./components/FilterSelect";
import { HelpPopover } from "./components/HelpPopover";
import { StatusBadge } from "./components/StatusBadge";
import { Button } from "./components/ui/button";
import { useT } from "./i18n";
import {
  statusLabel,
  statusTone,
  type RequestRecord,
} from "./request-record-model";
import { ToolReviewBadge, ToolReviewDetails } from "./ToolReviewDetails";
import {
  toolReviewStatuses,
  type ToolReviewStatus,
} from "./tool-review-record-model";

/** Requests, not sessions: the server applies review filters before pagination. */
export function ToolReviewRecords({
  coreSessionKey,
  isReady,
  localAccessTokenIds = [],
}: {
  coreSessionKey: string | null;
  isReady: boolean;
  localAccessTokenIds?: string[];
}) {
  const t = useT();
  const [filter, setFilter] = useState<ToolReviewStatus | "">("");
  const [records, setRecords] = useState<RequestRecord[]>([]);
  const [selected, setSelected] = useState<RequestRecord | null>(null);
  const selectedRef = useRef(selected);
  selectedRef.current = selected;
  const [pages, setPages] = useState(1);
  const [cursor, setCursor] = useState<string | null>(null);
  const [refresh, setRefresh] = useState(0);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState(false);
  const tokenKey = JSON.stringify(localAccessTokenIds);

  useEffect(() => {
    setRecords([]);
    setSelected(null);
    setCursor(null);
    setPages(1);
  }, [coreSessionKey, isReady, filter, tokenKey]);
  useEffect(() => {
    if (!isReady || coreSessionKey === null) return;
    let cancelled = false;
    let inFlight = false;
    let timer: ReturnType<typeof setTimeout> | undefined;
    setLoading(true);
    // A hidden window sends no list or detail requests; becoming visible
    // refreshes at once unless a load is already running.
    const tick = () => {
      timer = undefined;
      if (document.visibilityState !== "hidden") void load();
    };
    const load = async () => {
      inFlight = true;
      try {
        const items: RequestRecord[] = [];
        let next: string | null = null;
        // Re-read the visible page chain so a pending review leaving the filter
        // is removed, rather than retained forever by an append-only merge.
        for (let index = 0; index < pages; index++) {
          const page = await listRequestRecords({
            limit: 50,
            ...(filter ? { tool_review_status: filter } : {}),
            ...(next ? { cursor: next } : {}),
            local_access_token_ids: JSON.parse(tokenKey) as string[],
          });
          if (cancelled) return;
          items.push(...page.items);
          next = page.next_cursor;
          if (!next) break;
        }
        const current = selectedRef.current;
        let updated = current
          ? items.find((item) => item.id === current.id)
          : undefined;
        if (
          current &&
          !updated &&
          (current.tool_review?.status === "pending" ||
            current.status === "pending")
        ) {
          updated = await getRequestRecord(current.id);
        }
        if (cancelled) return;
        setRecords([...new Map(items.map((item) => [item.id, item])).values()]);
        setCursor(next);
        setError(false);
        if (updated)
          setSelected((value) => (value?.id === updated.id ? updated : value));
      } catch {
        if (!cancelled) setError(true);
      } finally {
        if (!cancelled) {
          inFlight = false;
          setLoading(false);
          timer = setTimeout(tick, 2000);
        }
      }
    };
    const onVisibilityChange = () => {
      if (document.visibilityState === "hidden" || inFlight) return;
      clearTimeout(timer);
      tick();
    };
    document.addEventListener("visibilitychange", onVisibilityChange);
    tick();
    return () => {
      cancelled = true;
      clearTimeout(timer);
      document.removeEventListener("visibilitychange", onVisibilityChange);
    };
  }, [coreSessionKey, isReady, filter, pages, refresh, tokenKey]);

  return (
    <div
      className="flex h-full min-h-0 min-w-0 flex-col overflow-hidden"
      data-testid="tool-review-records"
    >
      <div className="flex shrink-0 flex-wrap items-center gap-2 border-b py-2">
        {selected ? (
          <Button size="sm" variant="outline" onClick={() => setSelected(null)}>
            {t("toolReview.backToList")}
          </Button>
        ) : (
          <FilterSelect
            ariaLabel={t("toolReview.filter")}
            label={t("toolReview.filter")}
            value={filter}
            disabled={!isReady}
            onChange={(value) => setFilter(value as ToolReviewStatus | "")}
            options={[
              { label: t("common.all"), value: "" },
              ...toolReviewStatuses.map((value) => ({
                value,
                label: t(`toolReview.status.${value}`),
              })),
            ]}
          />
        )}
        <HelpPopover label={t("toolReview.requestsTab")}>
          {t("toolReview.requestsHint")}
        </HelpPopover>
        <Button
          className="ml-auto"
          size="sm"
          variant="outline"
          disabled={!isReady || loading}
          onClick={() => {
            setPages(1);
            setRefresh((value) => value + 1);
          }}
        >
          {t("toolReview.refresh")}
        </Button>
      </div>
      {error ? (
        <p
          role="alert"
          className="shrink-0 py-2 text-sm text-danger-foreground"
        >
          {t("toolReview.listFailed")}
        </p>
      ) : null}
      <div
        className="min-h-0 flex-1 overflow-y-auto overscroll-contain"
        data-testid="tool-review-records-primary"
        aria-busy={loading}
      >
        {selected ? (
          <div className="space-y-3 py-3">
            <div className="flex flex-wrap items-center gap-2 text-sm">
              <span className="break-all font-mono">{selected.id}</span>
              <StatusBadge tone={statusTone(selected.status)}>
                {t("toolReview.recordOutcome")}: {statusLabel(selected.status)}
              </StatusBadge>
            </div>
            {selected.tool_review ? (
              <ToolReviewDetails summary={selected.tool_review} />
            ) : (
              <p className="text-sm text-muted-foreground">
                {t("toolReview.noCalls")}
              </p>
            )}
          </div>
        ) : (
          <>
            {!isReady ? (
              <EmptyState title={t("toolReview.unavailable")} />
            ) : records.length === 0 && !loading ? (
              <EmptyState title={t("toolReview.listEmpty")} />
            ) : (
              records.map((record) => (
                <DataRow
                  asChild
                  key={record.id}
                  className="w-full flex-wrap text-left hover:bg-accent"
                >
                  <button
                    type="button"
                    data-testid="tool-review-record-row"
                    onClick={() => setSelected(record)}
                  >
                    <span className="min-w-0 flex-1">
                      <span className="block truncate text-sm font-medium">
                        {record.requested_model ?? record.input_protocol}
                      </span>
                      <span className="block break-all font-mono text-xs text-muted-foreground">
                        {record.id}
                      </span>
                      <span className="text-xs text-muted-foreground">
                        {record.started_at}
                      </span>
                    </span>
                    <StatusBadge tone={statusTone(record.status)}>
                      {statusLabel(record.status)}
                    </StatusBadge>
                    {record.tool_review ? (
                      <ToolReviewBadge summary={record.tool_review} />
                    ) : null}
                  </button>
                </DataRow>
              ))
            )}
            {cursor ? (
              <Button
                className="mx-auto my-3 flex"
                variant="outline"
                disabled={loading}
                onClick={() => setPages((value) => value + 1)}
              >
                {t("toolReview.loadEarlier")}
              </Button>
            ) : null}
          </>
        )}
      </div>
    </div>
  );
}
