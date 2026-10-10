import { useEffect, useLayoutEffect, useRef, useState } from "react";
import { ArrowLeft, Check, Maximize, RefreshCw } from "./components/icons";
import { HelpDisclosure } from "./components/HelpDisclosure";
import { FormMessage } from "./components/FormMessage";
import { IconButton } from "./components/IconButton";
import { Field } from "./components/Field";
import { Panel, PanelBody, PanelHeader } from "./components/Panel";
import { ConfirmDialog } from "./components/ConfirmDialog";
import { HelpPopover } from "./components/HelpPopover";
import { Button } from "./components/ui/button";
import { Input } from "./components/ui/input";
import { Label } from "./components/ui/label";
import { Switch } from "./components/ui/switch";
import { Textarea } from "./components/ui/textarea";
import {
  Dialog,
  DialogClose,
  DialogContent,
  DialogDescription,
  DialogHeader,
  DialogTitle,
  DialogTrigger,
} from "./components/ui/dialog";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "./components/ui/select";
import { getToolReviewSettings, updateToolReviewSettings } from "./bridge";
import { openToolReviewApprovalWindow } from "./tool-review-approval-window";
import { useT } from "./i18n";
import {
  highRiskToolReviewPrompt,
  DEFAULT_JEV_ENDPOINT,
  DEFAULT_JEV_MODEL,
  validateToolReviewSettingsPatch,
  type ToolReviewBackend,
  type ToolReviewSettingsPatch,
  type ToolReviewSettingsRecord,
  type ToolReviewSettings as Settings,
} from "./tool-review-settings-model";

type NoticeKey =
  | "toolReview.loadFailed"
  | "toolReview.saved"
  | "toolReview.presetApplied"
  | "toolReview.conflict"
  | "toolReview.saveFailed"
  | "toolReview.invalid"
  | "toolReview.keyRequired"
  | "toolReview.approvals.failed";
type Draft = Omit<Settings, "revision" | "credential_configured">;
function draftOf(settings: Settings): Draft {
  const {
    backend,
    enabled,
    endpoint,
    model,
    prompt,
    mode,
    batch_calls,
    timeout_ms,
    reject_threshold,
    ask_on_review_error,
    approval_timeout_ms,
  } = settings;
  return {
    backend,
    enabled,
    endpoint,
    model,
    prompt,
    mode,
    batch_calls,
    timeout_ms,
    reject_threshold,
    ask_on_review_error,
    approval_timeout_ms,
  };
}

export function ToolReviewSettings({
  coreSessionKey,
  isReady,
  active = true,
}: {
  coreSessionKey: string | null;
  isReady: boolean;
  active?: boolean;
}) {
  const t = useT();
  const [record, setRecord] = useState<ToolReviewSettingsRecord | null>(null);
  const [draft, setDraft] = useState<Draft | null>(null);
  const [apiKey, setApiKey] = useState("");
  const [phase, setPhase] = useState<"loading" | "ready" | "saving" | "error">(
    "loading",
  );
  // Notices hold translation keys so a language switch only re-renders them.
  const [message, setMessage] = useState<NoticeKey | null>(null);
  const [error, setError] = useState<NoticeKey | null>(null);
  const [confirmation, setConfirmation] =
    useState<ToolReviewSettingsPatch | null>(null);
  const [reload, setReload] = useState(0);
  const [openingApprovals, setOpeningApprovals] = useState(false);
  const generation = useRef(0);
  // Only non-secret connection drafts are cached. Never cache credentials.
  const backendDrafts = useRef<
    Partial<Record<ToolReviewBackend, Pick<Draft, "endpoint" | "model">>>
  >({});
  const promptInput = useRef<HTMLTextAreaElement>(null);
  const expandedPromptInput = useRef<HTMLTextAreaElement>(null);
  const [promptExpanded, setPromptExpanded] = useState(false);
  const [connectionExpanded, setConnectionExpanded] = useState(false);
  const scroller = useRef<HTMLDivElement>(null);
  const scrollTop = useRef(0);
  useLayoutEffect(() => {
    if (active && scroller.current)
      scroller.current.scrollTop = scrollTop.current;
  }, [active]);
  useEffect(() => {
    if (!active) {
      setPromptExpanded(false);
      setConfirmation(null);
    }
  }, [active]);

  useEffect(() => {
    const current = ++generation.current;
    setRecord(null);
    setDraft(null);
    backendDrafts.current = {};
    setApiKey("");
    setConfirmation(null);
    setPromptExpanded(false);
    setConnectionExpanded(false);
    scrollTop.current = 0;
    if (scroller.current) scroller.current.scrollTop = 0;
    setError(null);
    setMessage(null);
    setPhase("loading");
    setOpeningApprovals(false);
    if (isReady && coreSessionKey !== null) {
      void getToolReviewSettings()
        .then((next) => {
          if (generation.current !== current) return;
          setRecord(next);
          backendDrafts.current[next.settings.backend] = {
            endpoint: next.settings.endpoint,
            model: next.settings.model,
          };
          setDraft(draftOf(next.settings));
          setConnectionExpanded(!next.settings.credential_configured);
          setPhase("ready");
        })
        .catch(() => {
          if (generation.current !== current) return;
          setError("toolReview.loadFailed");
          setPhase("error");
        });
    }
    return () => {
      generation.current++;
    };
  }, [coreSessionKey, isReady, reload]);

  const busy = phase === "saving";
  const change = (patch: Partial<Draft>) => {
    setDraft((current) => (current ? { ...current, ...patch } : null));
    setError(null);
    setMessage(null);
  };
  const switchBackend = (backend: ToolReviewBackend) => {
    if (!draft || busy || confirmation !== null || backend === draft.backend)
      return;
    backendDrafts.current[draft.backend] = {
      endpoint: draft.endpoint,
      model: draft.model,
    };
    const connection = backendDrafts.current[backend] ?? {
      endpoint: "",
      model: "",
    };
    setApiKey("");
    change({ backend, ...connection });
    setConnectionExpanded(true);
  };
  const credentialConfigured =
    !!record?.settings.credential_configured &&
    draft?.backend === record.settings.backend;
  const save = async (patch: ToolReviewSettingsPatch) => {
    if (!record || busy) return;
    const current = generation.current;
    setConfirmation(null);
    setPhase("saving");
    setError(null);
    setMessage(null);
    try {
      const next = await updateToolReviewSettings(record.etag, patch);
      if (generation.current !== current) return;
      setRecord(next);
      backendDrafts.current[next.settings.backend] = {
        endpoint: next.settings.endpoint,
        model: next.settings.model,
      };
      setDraft(draftOf(next.settings));
      setApiKey("");
      setMessage("toolReview.saved");
    } catch (cause) {
      if (generation.current !== current) return;
      setError(
        cause instanceof Error &&
          cause.message === "tool_review_settings_conflict"
          ? "toolReview.conflict"
          : "toolReview.saveFailed",
      );
    } finally {
      if (generation.current === current) setPhase("ready");
    }
  };
  const openApprovals = async () => {
    if (openingApprovals || !isReady || coreSessionKey === null) return;
    const current = generation.current;
    setOpeningApprovals(true);
    try {
      await openToolReviewApprovalWindow();
    } catch {
      if (generation.current === current)
        setError("toolReview.approvals.failed");
    } finally {
      if (generation.current === current) setOpeningApprovals(false);
    }
  };
  const requestSave = () => {
    if (!draft || !record || busy) return;
    const changedBackend = draft.backend !== record.settings.backend;
    const patch: ToolReviewSettingsPatch = {
      ...draft,
      ...(apiKey ? { api_key: apiKey } : changedBackend ? { api_key: "" } : {}),
    };
    try {
      validateToolReviewSettingsPatch(patch);
    } catch {
      setError("toolReview.invalid");
      return;
    }
    if (draft.enabled && !credentialConfigured && !apiKey) {
      setConnectionExpanded(true);
      setError("toolReview.keyRequired");
      return;
    }
    if (
      changedBackend ||
      (draft.enabled &&
        (!record.settings.enabled ||
          draft.endpoint !== record.settings.endpoint))
    ) {
      setConfirmation(patch);
    } else {
      void save(patch);
    }
  };
  const insertVariable = (variable: "user_request" | "tool_call") => {
    if (!draft) return;
    const node = promptExpanded
      ? expandedPromptInput.current
      : promptInput.current;
    const start = node?.selectionStart ?? draft.prompt.length;
    const end = node?.selectionEnd ?? start;
    const text = `{{${variable}}}`;
    change({
      prompt: draft.prompt.slice(0, start) + text + draft.prompt.slice(end),
    });
    requestAnimationFrame(() => {
      node?.focus({ preventScroll: true });
      node?.setSelectionRange(start + text.length, start + text.length);
    });
  };
  const presetButton = (
    <Button
      size="sm"
      variant="outline"
      disabled={busy || confirmation !== null}
      title={t("toolReview.highRiskPresetHint")}
      onClick={() => {
        change({ prompt: highRiskToolReviewPrompt });
        setMessage("toolReview.presetApplied");
      }}
    >
      {t("toolReview.useHighRiskPreset")}
    </Button>
  );
  const variableButtons = (["user_request", "tool_call"] as const).map(
    (variable) => (
      <Button
        key={variable}
        size="sm"
        variant="outline"
        disabled={busy || confirmation !== null}
        onClick={() => insertVariable(variable)}
      >
        {t(
          variable === "user_request"
            ? "toolReview.insertUser"
            : "toolReview.insertTool",
        )}
      </Button>
    ),
  );
  const dirty =
    record &&
    draft &&
    (apiKey !== "" ||
      JSON.stringify(draft) !== JSON.stringify(draftOf(record.settings)));

  return (
    <Panel
      className="flex h-full min-h-0 flex-col"
      data-testid="tool-review-settings"
    >
      <PanelHeader
        size="sm"
        className="shrink-0 flex-wrap"
        actions={
          <>
            <Button
              size="sm"
              variant="outline"
              disabled={
                !isReady ||
                coreSessionKey === null ||
                openingApprovals ||
                confirmation !== null
              }
              onClick={() => void openApprovals()}
            >
              {t(
                openingApprovals
                  ? "toolReview.approvals.loading"
                  : "toolReview.approvals.open",
              )}
            </Button>
            <IconButton
              label={t("common.refresh")}
              variant="outline"
              disabled={
                busy || !isReady || phase === "loading" || confirmation !== null
              }
              onClick={() => setReload((value) => value + 1)}
            >
              <RefreshCw aria-hidden="true" />
            </IconButton>
            <Button
              size="sm"
              disabled={!dirty || busy || !isReady || confirmation !== null}
              onClick={requestSave}
            >
              <Check aria-hidden="true" />
              {busy ? t("common.saving") : t("common.save")}
            </Button>
          </>
        }
      >
        <div className="flex flex-wrap items-center gap-2">
          <Label
            className="flex items-center gap-2"
            htmlFor="tool-review-enabled"
          >
            {t("toolReview.enable")}
          </Label>
          <Switch
            id="tool-review-enabled"
            aria-label={t("toolReview.enable")}
            checked={draft?.enabled ?? false}
            disabled={!draft || busy || !isReady || confirmation !== null}
            onCheckedChange={(enabled) => change({ enabled })}
            size="sm"
          />
          <Label htmlFor="tool-review-backend">{t("toolReview.backend")}</Label>
          <Select
            value={draft?.backend ?? "jev"}
            disabled={!draft || busy || !isReady || confirmation !== null}
            onValueChange={(value) => {
              if (value === "jev" || value === "responses")
                switchBackend(value);
            }}
          >
            <SelectTrigger
              id="tool-review-backend"
              size="sm"
              aria-label={t("toolReview.backend")}
              data-testid="tool-review-backend"
            >
              <SelectValue />
            </SelectTrigger>
            <SelectContent>
              <SelectItem value="jev">
                {t("toolReview.backends.jev")}
              </SelectItem>
              <SelectItem value="responses">
                {t("toolReview.backends.responses")}
              </SelectItem>
            </SelectContent>
          </Select>
          <HelpPopover label={t("toolReview.backendHelpLabel")}>
            {t("toolReview.backendHelp")}
          </HelpPopover>
          <Label htmlFor="tool-review-batch-calls">
            {t("toolReview.reviewMethod")}
          </Label>
          <Select
            value={draft?.batch_calls ? "batch" : "individual"}
            disabled={!draft || busy || !isReady || confirmation !== null}
            onValueChange={(value) => {
              if (value === "batch" || value === "individual")
                change({ batch_calls: value === "batch" });
            }}
          >
            <SelectTrigger
              id="tool-review-batch-calls"
              size="sm"
              aria-label={t("toolReview.reviewMethod")}
              data-testid="tool-review-batch-calls"
            >
              <SelectValue />
            </SelectTrigger>
            <SelectContent>
              <SelectItem value="individual">
                {t("toolReview.individualCalls")}
              </SelectItem>
              <SelectItem value="batch">
                {t("toolReview.batchCalls")}
              </SelectItem>
            </SelectContent>
          </Select>
          <HelpPopover label={t("toolReview.reviewMethodHelp")}>
            {t("toolReview.batchCallsHelp")}
          </HelpPopover>
          <HelpPopover label={t("toolReview.title")}>
            {t("toolReview.help")}
          </HelpPopover>
        </div>
      </PanelHeader>
      <PanelBody
        ref={scroller}
        onScroll={(event) => {
          if (active) scrollTop.current = event.currentTarget.scrollTop;
        }}
        className="flex flex-col gap-3"
        data-testid="tool-review-primary"
      >
        {error ? <FormMessage tone="error">{t(error)}</FormMessage> : null}
        {message ? (
          <FormMessage tone="success">{t(message)}</FormMessage>
        ) : null}
        {!draft ? (
          <p role="status" className="text-sm text-muted-foreground">
            {phase === "error"
              ? t("toolReview.retryHint")
              : t(isReady ? "common.loading" : "toolReview.unavailable")}
          </p>
        ) : (
          <>
            <fieldset
              disabled={busy || confirmation !== null}
              className="grid min-w-0 gap-3 sm:grid-cols-2"
            >
              <Field
                label={t("toolReview.mode")}
                htmlFor="tool-review-mode"
                hint={t(
                  draft.mode === "mark"
                    ? "toolReview.markHint"
                    : draft.mode === "ask"
                      ? "toolReview.askHint"
                      : "toolReview.rejectHint",
                )}
              >
                <Select
                  value={draft.mode}
                  disabled={busy || confirmation !== null}
                  onValueChange={(value) => {
                    if (
                      value === "mark" ||
                      value === "reject" ||
                      value === "ask"
                    )
                      change({ mode: value });
                  }}
                >
                  <SelectTrigger id="tool-review-mode" className="w-full">
                    <SelectValue />
                  </SelectTrigger>
                  <SelectContent>
                    <SelectItem value="mark">{t("toolReview.mark")}</SelectItem>
                    <SelectItem value="reject">
                      {t("toolReview.reject")}
                    </SelectItem>
                    <SelectItem value="ask">{t("toolReview.ask")}</SelectItem>
                  </SelectContent>
                </Select>
              </Field>
              <Field
                label={t("toolReview.thresholdPercent")}
                htmlFor="tool-review-threshold"
                hint={t("toolReview.thresholdHint")}
              >
                <Input
                  id="tool-review-threshold"
                  type="number"
                  min={0}
                  max={100}
                  step="any"
                  value={
                    Number.isFinite(draft.reject_threshold)
                      ? Number((draft.reject_threshold * 100).toFixed(8))
                      : ""
                  }
                  onChange={(event) =>
                    change({
                      reject_threshold: event.target.valueAsNumber / 100,
                    })
                  }
                />
              </Field>
            </fieldset>
            {draft.mode === "ask" ? (
              <fieldset
                disabled={busy || confirmation !== null}
                className="grid min-w-0 gap-3 sm:grid-cols-2"
              >
                <Field
                  label={t("toolReview.askOnReviewError")}
                  htmlFor="tool-review-ask-on-error"
                  hint={t("toolReview.askOnReviewErrorHint")}
                >
                  <Switch
                    id="tool-review-ask-on-error"
                    aria-label={t("toolReview.askOnReviewError")}
                    checked={draft.ask_on_review_error}
                    disabled={busy || confirmation !== null}
                    onCheckedChange={(ask_on_review_error) =>
                      change({ ask_on_review_error })
                    }
                    size="sm"
                  />
                </Field>
                <Field
                  label={t("toolReview.approvalTimeout")}
                  htmlFor="tool-review-approval-timeout"
                >
                  <Input
                    id="tool-review-approval-timeout"
                    type="number"
                    min={1000}
                    max={600000}
                    step={1}
                    value={
                      Number.isFinite(draft.approval_timeout_ms)
                        ? draft.approval_timeout_ms
                        : ""
                    }
                    onChange={(event) =>
                      change({
                        approval_timeout_ms: event.target.valueAsNumber,
                      })
                    }
                  />
                </Field>
              </fieldset>
            ) : null}
            <div className="flex min-w-0 flex-auto flex-col gap-1.5">
              <div className="flex flex-wrap items-center justify-between gap-1.5">
                <Label htmlFor="tool-review-prompt">
                  {t("toolReview.prompt")}
                </Label>
                <div className="flex flex-wrap gap-1">
                  {presetButton}
                  {variableButtons}
                  <Dialog
                    open={promptExpanded}
                    onOpenChange={setPromptExpanded}
                  >
                    <DialogTrigger asChild>
                      <Button
                        size="sm"
                        variant="outline"
                        disabled={busy || confirmation !== null}
                      >
                        <Maximize aria-hidden="true" />
                        {t("toolReview.expandPrompt")}
                      </Button>
                    </DialogTrigger>
                    <DialogContent
                      variant="workspace"
                      className="top-[calc(50%+var(--window-chrome-height)/2)] h-[calc(100dvh-var(--window-chrome-height)-2rem)] max-h-[calc(100dvh-var(--window-chrome-height)-2rem)]"
                      showCloseButton={false}
                      data-testid="tool-review-prompt-workspace"
                      onOpenAutoFocus={(event) => {
                        event.preventDefault();
                        const editor = expandedPromptInput.current;
                        editor?.focus({ preventScroll: true });
                        editor?.setSelectionRange(
                          promptInput.current?.selectionStart ?? 0,
                          promptInput.current?.selectionEnd ?? 0,
                        );
                      }}
                    >
                      <DialogHeader className="shrink-0 flex-row flex-wrap items-center justify-between gap-2 border-b px-3 py-2 text-left">
                        <DialogTitle className="text-sm">
                          {t("toolReview.prompt")}
                        </DialogTitle>
                        <div className="flex flex-wrap items-center gap-1">
                          {presetButton}
                          {variableButtons}
                          <HelpPopover
                            label={t("toolReview.promptHint")}
                            inDialog
                          >
                            {t("toolReview.promptHint")}
                          </HelpPopover>
                          <DialogClose asChild>
                            <Button size="sm" variant="outline">
                              <ArrowLeft aria-hidden="true" />
                              {t("toolReview.returnToSettings")}
                            </Button>
                          </DialogClose>
                        </div>
                      </DialogHeader>
                      <DialogDescription className="sr-only">
                        {t("toolReview.expandedPromptHint")}
                      </DialogDescription>
                      <div className="flex min-h-0 flex-1 flex-col p-3">
                        <Textarea
                          ref={expandedPromptInput}
                          aria-label={t("toolReview.prompt")}
                          className="field-sizing-fixed min-h-0 flex-1 resize-none font-mono"
                          data-testid="tool-review-expanded-prompt"
                          value={draft.prompt}
                          onChange={(event) =>
                            change({ prompt: event.target.value })
                          }
                          spellCheck={false}
                        />
                      </div>
                    </DialogContent>
                  </Dialog>
                </div>
              </div>
              <Textarea
                ref={promptInput}
                id="tool-review-prompt"
                className="field-sizing-fixed min-h-32 flex-auto resize-none font-mono"
                value={draft.prompt}
                disabled={busy || confirmation !== null}
                onChange={(event) => change({ prompt: event.target.value })}
                spellCheck={false}
              />
              <p className="text-xs text-muted-foreground">
                {t("toolReview.promptHint")}
              </p>
            </div>
            <HelpDisclosure
              title={t("toolReview.advanced")}
              open={connectionExpanded}
              onOpenChange={setConnectionExpanded}
            >
              <fieldset
                disabled={busy || confirmation !== null}
                className="grid min-w-0 gap-3 sm:grid-cols-2"
              >
                <Field
                  label={t("toolReview.endpoint")}
                  htmlFor="tool-review-endpoint"
                >
                  <Input
                    id="tool-review-endpoint"
                    value={draft.endpoint}
                    placeholder={
                      draft.backend === "responses"
                        ? "https://api.openai.com/v1/responses"
                        : DEFAULT_JEV_ENDPOINT
                    }
                    onChange={(event) =>
                      change({ endpoint: event.target.value })
                    }
                    autoComplete="off"
                    spellCheck={false}
                  />
                </Field>
                <Field
                  label={t("toolReview.key")}
                  htmlFor="tool-review-key"
                  hint={t(
                    credentialConfigured
                      ? "toolReview.keyConfigured"
                      : "toolReview.keyMissing",
                  )}
                >
                  <Input
                    id="tool-review-key"
                    type="password"
                    value={apiKey}
                    onChange={(event) => {
                      setApiKey(event.target.value);
                      setMessage(null);
                    }}
                    autoComplete="new-password"
                    spellCheck={false}
                  />
                </Field>

                <Field
                  label={t("toolReview.model")}
                  htmlFor="tool-review-model"
                >
                  <Input
                    id="tool-review-model"
                    value={draft.model}
                    placeholder={
                      draft.backend === "responses"
                        ? t("toolReview.responsesModelPlaceholder")
                        : DEFAULT_JEV_MODEL
                    }
                    onChange={(event) => change({ model: event.target.value })}
                  />
                </Field>
                <Field
                  label={t("toolReview.timeout")}
                  htmlFor="tool-review-timeout"
                  hint={
                    draft.backend === "responses"
                      ? t("toolReview.responsesTimeoutHint")
                      : undefined
                  }
                >
                  <Input
                    id="tool-review-timeout"
                    type="number"
                    min={1}
                    max={120000}
                    step={1}
                    value={
                      Number.isFinite(draft.timeout_ms) ? draft.timeout_ms : ""
                    }
                    onChange={(event) =>
                      change({ timeout_ms: event.target.valueAsNumber })
                    }
                  />
                </Field>
              </fieldset>
              <p className="mt-2 break-all text-xs text-muted-foreground">
                {t("toolReview.revision")}: {record?.settings.revision}
              </p>
              <p>{t("toolReview.nextRequests")}</p>
            </HelpDisclosure>
          </>
        )}
      </PanelBody>
      <ConfirmDialog
        open={confirmation !== null}
        title={t("toolReview.confirmTitle")}
        confirmLabel={t("toolReview.confirm")}
        onCancel={() => setConfirmation(null)}
        onConfirm={() => {
          if (confirmation)
            void save({ ...confirmation, data_transfer_acknowledged: true });
        }}
        description={
          <>
            <p>{t("toolReview.confirmBody")}</p>
            <p className="text-xs">
              {t("toolReview.backend")}:{" "}
              {confirmation?.backend
                ? t(`toolReview.backends.${confirmation.backend}`)
                : t("toolReview.notRecorded")}
            </p>
            <p className="break-all font-mono text-xs">
              {confirmation?.endpoint}
            </p>
          </>
        }
      />
    </Panel>
  );
}
