import { useEffect, useLayoutEffect, useRef, useState } from "react";
import { listRequestRecords, getRequestRecord } from "./bridge";
import { DataRow } from "./components/DataRow";
import { EmptyState } from "./components/EmptyState";
import { FilterSelect } from "./components/FilterSelect";
import { HelpPopover } from "./components/HelpPopover";
import { ActionGroup } from "./components/ActionGroup";
import { IconButton } from "./components/IconButton";
import { HelpDisclosure } from "./components/HelpDisclosure";
import { FormMessage } from "./components/FormMessage";
import { ArrowLeft, ChevronRight, RefreshCw } from "./components/icons";
import { Button } from "./components/ui/button";
import { useT } from "./i18n";
import { statusLabel, type RequestRecord } from "./request-record-model";
import { ToolReviewBadge, ToolReviewDetails } from "./ToolReviewDetails";
import {
  hasVisibleToolReview,
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
  const scroller = useRef<HTMLDivElement>(null);
  const listScroll = useRef(0);
  const lastSelected = useRef<string | null>(null);
  useLayoutEffect(() => {
    if (scroller.current)
      scroller.current.scrollTop = selected ? 0 : listScroll.current;
    if (!selected && lastSelected.current) {
      const row = Array.from(
        scroller.current?.querySelectorAll<HTMLButtonElement>(
          "[data-record-id]",
        ) ?? [],
      ).find((node) => node.dataset.recordId === lastSelected.current);
      row?.focus({ preventScroll: true });
    }
  }, [selected?.id]);

  useEffect(() => {
    listScroll.current = 0;
    lastSelected.current = null;
    if (scroller.current) scroller.current.scrollTop = 0;
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
            tool_review_only: true,
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
        setRecords([
          ...new Map(
            items
              .filter((item) => hasVisibleToolReview(item.tool_review))
              .map((item) => [item.id, item]),
          ).values(),
        ]);
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
            <ArrowLeft aria-hidden="true" />
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
              ...toolReviewStatuses
                .filter((value) => value !== "not_applicable")
                .map((value) => ({
                  value,
                  label: t(`toolReview.status.${value}`),
                })),
            ]}
          />
        )}
        <HelpPopover label={t("toolReview.requestsTab")}>
          {t("toolReview.requestsHint")}
        </HelpPopover>
        <ActionGroup>
          <IconButton
            label={t("common.refresh")}
            variant="outline"
            disabled={!isReady || loading}
            onClick={() => {
              setPages(1);
              setRefresh((value) => value + 1);
            }}
          >
            <RefreshCw aria-hidden="true" />
          </IconButton>
        </ActionGroup>
      </div>
      {error ? (
        <FormMessage tone="error" className="shrink-0 my-2">
          {t("toolReview.listFailed")}
        </FormMessage>
      ) : null}
      <div
        ref={scroller}
        className="min-h-0 min-w-0 flex-1 overflow-y-auto overscroll-contain"
        data-tab-scroller
        data-testid="tool-review-records-primary"
        aria-busy={loading}
      >
        {selected ? (
          <div className="space-y-3 py-3">
            {selected.tool_review ? (
              <ToolReviewDetails summary={selected.tool_review} />
            ) : (
              <p className="text-sm text-muted-foreground">
                {t("toolReview.noCalls")}
              </p>
            )}
            <HelpDisclosure title={t("toolReview.requestDetails")}>
              <span className="break-all font-mono">{selected.id}</span>
              <span>
                {t("toolReview.recordOutcome")}: {statusLabel(selected.status)}
              </span>
            </HelpDisclosure>
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
                  className="w-full flex-wrap text-left hover:bg-accent data-[state=selected]:bg-accent focus-visible:outline-2 focus-visible:outline-ring"
                >
                  <button
                    type="button"
                    data-testid="tool-review-record-row"
                    data-record-id={record.id}
                    data-state={
                      lastSelected.current === record.id
                        ? "selected"
                        : undefined
                    }
                    onClick={() => {
                      listScroll.current = scroller.current?.scrollTop ?? 0;
                      lastSelected.current = record.id;
                      setSelected(record);
                    }}
                  >
                    <span className="min-w-0 flex-1">
                      <span className="block truncate text-sm font-medium">
                        {record.requested_model ?? record.input_protocol}
                      </span>
                      <span
                        className="text-xs text-muted-foreground"
                        title={record.started_at}
                      >
                        {t("toolReview.callCount", {
                          count: record.tool_review?.calls.length ?? 0,
                        })}{" "}
                        ·{" "}
                        {new Date(record.started_at).toLocaleTimeString([], {
                          hour: "2-digit",
                          minute: "2-digit",
                          second: "2-digit",
                          hour12: false,
                        })}
                      </span>
                    </span>
                    {record.tool_review ? (
                      <ToolReviewBadge summary={record.tool_review} />
                    ) : null}
                    <ChevronRight
                      className="size-3.5 shrink-0 text-muted-foreground"
                      aria-hidden="true"
                    />
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
