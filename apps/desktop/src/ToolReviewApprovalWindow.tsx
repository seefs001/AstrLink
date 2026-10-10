import { useCallback, useEffect, useRef, useState } from "react";
import { Button } from "./components/ui/button";
import { Panel, PanelBody } from "./components/Panel";
import { EmptyState } from "./components/EmptyState";
import { FormMessage } from "./components/FormMessage";
import { HelpDisclosure } from "./components/HelpDisclosure";
import { StatusBadge } from "./components/StatusBadge";
import { RawSealingDialogs } from "./RawSealingControls";
import { ToolReviewDetails, ToolReviewBadge } from "./ToolReviewDetails";
import { useT } from "./i18n";
import {
  decideToolReviewApproval,
  getToolReviewApproval,
  getRawSealingStatus,
  listToolReviewApprovals,
  listenRawSealingChanged,
  listenToolReviewApprovalState,
} from "./bridge";
import {
  ApprovalTombstones,
  type PendingToolApproval,
  type ToolApprovalDetail,
} from "./tool-review-approval-model";
import type { RawSealingState } from "./raw-sealing-model";
import { isWebEdition } from "./edition";

export const TOOL_APPROVAL_POLL_MS = 2000;
type QueueStatus = "loading" | "ready" | "offline";

/** Live plaintext is scoped to the selected pending request and lock generation.
 * Nothing writes it to storage, events, notifications or logs. Closing decides nothing. */
export function ToolReviewApprovalWindow() {
  const t = useT();
  const [status, setStatus] = useState<QueueStatus>("loading");
  const [items, setItems] = useState<PendingToolApproval[]>([]);
  const [selected, setSelected] = useState<string | null>(null);
  const [detail, setDetail] = useState<ToolApprovalDetail | null>(null);
  const [sealing, setSealing] = useState<RawSealingState | null>(null);
  const [unlockOpen, setUnlockOpen] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [now, setNow] = useState(Date.now);
  const generation = useRef(0);
  const validDetail = useRef<string | null>(null);
  const refreshGeneration = useRef(0);
  const refreshFlight = useRef(false);
  const mounted = useRef(true);
  const busyRef = useRef(false);
  const selectedRef = useRef<string | null>(null);
  const terminal = useRef(new ApprovalTombstones());
  const live = useRef<{
    status: QueueStatus;
    item?: PendingToolApproval;
    unlocked: boolean;
    unlockExpiresAt?: string | null;
  }>({ status: "loading", unlocked: false });
  const clearDetail = useCallback(() => {
    generation.current++;
    validDetail.current = null;
    setDetail(null);
  }, []);
  const refresh = useCallback(async () => {
    if (refreshFlight.current || busyRef.current) return;
    refreshFlight.current = true;
    const current = ++refreshGeneration.current;
    let timeout: ReturnType<typeof setTimeout> | undefined;
    try {
      // A failed or stalled lock read is not evidence of an unlocked vault.
      const [pending, raw] = await Promise.race([
        Promise.all([listToolReviewApprovals(), getRawSealingStatus()]),
        new Promise<never>((_, reject) => {
          timeout = setTimeout(
            () => reject(new Error("tool_review_approval_unavailable")),
            5000,
          );
        }),
      ]);
      if (!mounted.current || current !== refreshGeneration.current) return;
      terminal.current.reconcile(pending);
      const next = pending.filter(
        (row) =>
          !terminal.current.has(row.id) &&
          Date.parse(row.expires_at) > Date.now(),
      );
      setItems(next);
      setSealing(raw);
      setStatus("ready");
      setError((previous) =>
        previous === "unavailable" || previous === "forbidden"
          ? null
          : previous,
      );
      if (
        !raw.unlocked ||
        raw.key_replaced ||
        (raw.unlock_expires_at !== null &&
          Date.parse(raw.unlock_expires_at) <= Date.now()) ||
        !next.some((row) => row.id === selectedRef.current)
      )
        clearDetail();
    } catch (cause) {
      if (!mounted.current || current !== refreshGeneration.current) return;
      const code = cause instanceof Error ? cause.message : "";
      setError((previous) =>
        previous === "failed"
          ? previous
          : code === "tool_review_approval_forbidden"
            ? "forbidden"
            : "unavailable",
      );
      live.current = { status: "offline", unlocked: false };
      setStatus("offline");
      setSealing(null);
      setUnlockOpen(false);
      clearDetail();
    } finally {
      clearTimeout(timeout);
      refreshFlight.current = false;
    }
  }, [clearDetail]);
  useEffect(() => {
    mounted.current = true;
    void refresh();
    const poll = setInterval(() => void refresh(), TOOL_APPROVAL_POLL_MS);
    const clock = setInterval(() => setNow(Date.now()), 250);
    let stops: Array<() => void> = [];
    let disposed = false;
    const register = async () => {
      const results = await Promise.allSettled([
        listenRawSealingChanged(() => {
          refreshGeneration.current++;
          live.current.unlocked = false;
          setSealing(null);
          clearDetail();
          void refresh();
        }),
        listenToolReviewApprovalState((next) => {
          if (next !== "ready") {
            refreshGeneration.current++;
            live.current = { status: next, unlocked: false };
            setStatus(next);
            setSealing(null);
            setUnlockOpen(false);
            clearDetail();
          } else void refresh();
        }),
      ]);
      for (const result of results) {
        if (result.status === "fulfilled") {
          if (disposed) result.value();
          else stops.push(result.value);
        } else console.error("tool_review_approval_listener_failed");
      }
    };
    void register();
    return () => {
      mounted.current = false;
      disposed = true;
      generation.current++;
      refreshGeneration.current++;
      clearInterval(poll);
      clearInterval(clock);
      stops.forEach((stop) => stop());
      stops = [];
    };
  }, [refresh, clearDetail]);
  const pending = items.filter(
    (row) => !terminal.current.has(row.id) && Date.parse(row.expires_at) > now,
  );
  const item = pending.find((row) => row.id === selected) ?? pending[0];
  const id = item?.id ?? null;
  const unlocked =
    !!sealing?.unlocked &&
    !sealing.key_replaced &&
    (sealing.unlock_expires_at === null ||
      Date.parse(sealing.unlock_expires_at) > now);
  live.current = {
    status,
    item,
    unlocked,
    unlockExpiresAt: sealing?.unlock_expires_at ?? null,
  };
  selectedRef.current = id;
  useEffect(() => {
    clearDetail();
    setUnlockOpen(false);
  }, [id, clearDetail]);
  useEffect(() => {
    if (!unlocked || status !== "ready" || !id) {
      clearDetail();
      return;
    }
    const current = ++generation.current;
    void getToolReviewApproval(id).then(
      (value) => {
        const facts = live.current;
        if (
          !mounted.current ||
          current !== generation.current ||
          !facts.unlocked ||
          (facts.unlockExpiresAt != null &&
            Date.parse(facts.unlockExpiresAt) <= Date.now()) ||
          facts.status !== "ready" ||
          facts.item?.id !== id ||
          Date.parse(facts.item.expires_at) <= Date.now()
        )
          return;
        if (
          facts.item.request_id !== value.request_id ||
          facts.item.summary.calls.length !== value.calls.length ||
          value.calls.some(
            (call) =>
              !facts.item?.summary.calls.some(
                (review) =>
                  review.index === call.index && review.name === call.name,
              ),
          )
        ) {
          clearDetail();
          setStatus("offline");
          setError("failed");
          return;
        }
        validDetail.current = id;
        setDetail(value);
      },
      (cause) => {
        if (!mounted.current || current !== generation.current) return;
        clearDetail();
        const code = cause instanceof Error ? cause.message : "";
        if (code === "raw_locked") {
          setSealing(null);
          setError("locked");
        } else if (code === "tool_review_approval_not_pending") {
          refreshGeneration.current++;
          terminal.current.add(id);
          setItems((previous) => previous.filter((row) => row.id !== id));
          setError("notPending");
        } else {
          setStatus("offline");
          setError("failed");
        }
      },
    );
    return () => {
      generation.current++;
    };
  }, [id, unlocked, status, clearDetail]);

  const decide = async (decision: "allow" | "deny") => {
    const facts = live.current;
    if (
      busyRef.current ||
      facts.status !== "ready" ||
      !facts.item ||
      Date.parse(facts.item.expires_at) <= Date.now() ||
      (decision === "allow" &&
        (!facts.unlocked ||
          (facts.unlockExpiresAt != null &&
            Date.parse(facts.unlockExpiresAt) <= Date.now()) ||
          validDetail.current !== facts.item.id ||
          detail?.id !== facts.item.id))
    )
      return;
    const target = facts.item.id;
    refreshGeneration.current++;
    busyRef.current = true;
    setBusy(true);
    setError(null);
    clearDetail();
    try {
      // No optimistic approval. Only a validated server terminal response settles.
      await decideToolReviewApproval(target, decision);
      terminal.current.add(target);
      if (mounted.current)
        setItems((previous) => previous.filter((row) => row.id !== target));
    } catch (cause) {
      if (!mounted.current) return;
      const code = cause instanceof Error ? cause.message : "";
      if (code === "tool_review_approval_not_pending") {
        terminal.current.add(target);
        setItems((previous) => previous.filter((row) => row.id !== target));
        setError("notPending");
      } else {
        setStatus("offline");
        setSealing(null);
        setError("failed");
      }
    } finally {
      // A pre-decision list cannot reconcile tombstones or restore a settled
      // request. The next successful snapshot must start after this outcome.
      refreshGeneration.current++;
      busyRef.current = false;
      if (mounted.current) {
        setBusy(false);
        void refresh();
      }
    }
  };
  const currentDetail =
    status === "ready" &&
    unlocked &&
    detail?.id === id &&
    detail.request_id === item?.request_id
      ? detail
      : null;
  return (
    <main
      className="flex h-dvh min-h-0 flex-col overflow-hidden pt-[var(--window-chrome-height)]"
      data-testid="tool-review-approval-window"
    >
      <header className="flex shrink-0 flex-wrap items-center justify-between gap-2 border-b px-3 py-2">
        <h1 className="text-sm font-semibold">
          {t("toolReview.approvals.title")} · {pending.length}
        </h1>
        <div className="flex gap-2">
          {isWebEdition ? (
            <Button
              size="xs"
              variant="outline"
              onClick={() => window.location.assign(window.location.pathname)}
            >
              {t("common.back")}
            </Button>
          ) : null}
          <Button size="xs" variant="outline" onClick={() => void refresh()}>
            {t("common.refresh")}
          </Button>
        </div>
      </header>
      <Panel className="flex min-h-0 flex-1 flex-col border-0 rounded-none">
        <PanelBody
          className="min-h-0 flex-1 overflow-y-auto p-3"
          data-testid="tool-review-approval-primary"
        >
          {status !== "ready" ? (
            <FormMessage tone="warning">
              {t(`toolReview.approvals.${status}`)}
            </FormMessage>
          ) : null}
          {error ? (
            <FormMessage tone="error">
              {t(`toolReview.approvals.${error}`)}
            </FormMessage>
          ) : null}
          {pending.length > 1 ? (
            <nav
              className="mb-3 flex flex-wrap gap-2"
              aria-label={t("toolReview.approvals.title")}
            >
              {pending.map((row) => (
                <Button
                  key={row.id}
                  size="xs"
                  variant={row.id === id ? "secondary" : "outline"}
                  onClick={() => {
                    clearDetail();
                    setError(null);
                    setSelected(row.id);
                  }}
                  disabled={busy}
                >
                  {row.request_id}
                </Button>
              ))}
            </nav>
          ) : null}
          {item ? (
            <>
              <div className="mb-3 flex flex-wrap justify-between gap-2 text-xs text-muted-foreground">
                <code className="break-all">{item.request_id}</code>
                <span>
                  {t("toolReview.approvals.expires")}:{" "}
                  {new Date(item.expires_at).toLocaleTimeString()}
                </span>
              </div>
              <div className="mb-3 flex flex-wrap items-center gap-2 text-xs">
                <span
                  data-testid="tool-approval-backend"
                  className="text-muted-foreground"
                >
                  {item.summary.backend === undefined
                    ? t("toolReview.notRecorded")
                    : t(`toolReview.backends.${item.summary.backend}`)}
                  {item.summary.backend === "responses"
                    ? ` · ${t("toolReview.riskScore")}`
                    : ""}
                </span>
                <span>{t("toolReview.reviewStatus")}:</span>
                <ToolReviewBadge summary={item.summary} />
                <span>{t("toolReview.humanDecision")}:</span>
                <StatusBadge tone="pending">
                  {t("toolReview.approvalStates.pending")}
                </StatusBadge>
              </div>
              {status === "ready" && !unlocked ? (
                <FormMessage tone="notice">
                  <p>{t("toolReview.approvals.locked")}</p>
                  <Button
                    size="sm"
                    variant="outline"
                    disabled={!sealing?.password_set || sealing.key_replaced}
                    onClick={() => setUnlockOpen(true)}
                  >
                    {t("toolReview.approvals.unlock")}
                  </Button>
                </FormMessage>
              ) : null}
              {currentDetail ? (
                <section
                  className="mt-3 space-y-3"
                  data-testid="tool-approval-plaintext"
                >
                  <h2 className="text-xs font-semibold">
                    {t("toolReview.approvals.userRequest")}
                  </h2>
                  <pre className="whitespace-pre-wrap break-words text-xs">
                    {currentDetail.user_request}
                  </pre>
                  {currentDetail.calls.map((call) => (
                    <div key={call.index}>
                      <h3 className="break-all font-mono text-xs">
                        #{call.index} {call.name} ·{" "}
                        {t("toolReview.approvals.arguments")}
                      </h3>
                      <pre className="mt-1 whitespace-pre-wrap break-words font-mono text-xs">
                        {call.arguments}
                      </pre>
                    </div>
                  ))}
                </section>
              ) : status === "ready" && unlocked ? (
                <p className="mt-3 text-xs text-muted-foreground">
                  {t("toolReview.approvals.loadingDetails")}
                </p>
              ) : null}
              <div
                className="mt-3"
                data-testid="tool-approval-review-disclosure"
                key={id}
              >
                <HelpDisclosure title={t("toolReview.title")}>
                  <ToolReviewDetails summary={item.summary} embedded />
                </HelpDisclosure>
              </div>
            </>
          ) : status === "ready" ? (
            <EmptyState title={t("toolReview.approvals.empty")} />
          ) : null}
        </PanelBody>
      </Panel>
      <footer className="shrink-0 space-y-2 border-t px-3 py-2">
        {status === "ready" && item ? (
          <div className="flex justify-end gap-2">
            <Button
              variant="outline"
              disabled={busy}
              onClick={() => void decide("deny")}
              data-decision="deny"
            >
              {t("toolReview.approvals.deny")}
            </Button>
            <Button
              disabled={busy || !currentDetail}
              onClick={() => void decide("allow")}
              onKeyDown={(event) => {
                if (event.key === "Enter") event.preventDefault();
              }}
              data-decision="allow"
            >
              {t("toolReview.approvals.allow")}
            </Button>
          </div>
        ) : null}
        <p className="text-xs text-muted-foreground">
          {t("toolReview.approvals.closeHint")}
        </p>
      </footer>
      <RawSealingDialogs
        dialog={
          unlockOpen && status === "ready" && item ? { kind: "unlock" } : null
        }
        status={sealing}
        onStatus={(next) => {
          setSealing(next);
          clearDetail();
        }}
        onClose={() => {
          setUnlockOpen(false);
          void refresh();
        }}
      />
    </main>
  );
}
