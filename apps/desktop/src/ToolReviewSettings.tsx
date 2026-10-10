import { useEffect, useRef, useState } from "react";
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
import { useT } from "./i18n";
import {
  validateToolReviewSettingsPatch,
  type ToolReviewSettingsPatch,
  type ToolReviewSettingsRecord,
  type ToolReviewSettings as Settings,
} from "./tool-review-settings-model";

type NoticeKey =
  | "toolReview.loadFailed"
  | "toolReview.saved"
  | "toolReview.conflict"
  | "toolReview.saveFailed"
  | "toolReview.invalid"
  | "toolReview.keyRequired";
type Draft = Omit<Settings, "revision" | "credential_configured">;
function draftOf(settings: Settings): Draft {
  const { enabled, endpoint, model, prompt, mode, timeout_ms } = settings;
  return { enabled, endpoint, model, prompt, mode, timeout_ms };
}

export function ToolReviewSettings({
  coreSessionKey,
  isReady,
}: {
  coreSessionKey: string | null;
  isReady: boolean;
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
  const generation = useRef(0);
  const promptInput = useRef<HTMLTextAreaElement>(null);
  const expandedPromptInput = useRef<HTMLTextAreaElement>(null);
  const [promptExpanded, setPromptExpanded] = useState(false);

  useEffect(() => {
    const current = ++generation.current;
    setRecord(null);
    setDraft(null);
    setApiKey("");
    setConfirmation(null);
    setPromptExpanded(false);
    setError(null);
    setMessage(null);
    setPhase("loading");
    if (isReady && coreSessionKey !== null) {
      void getToolReviewSettings()
        .then((next) => {
          if (generation.current !== current) return;
          setRecord(next);
          setDraft(draftOf(next.settings));
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
  const requestSave = () => {
    if (!draft || !record || busy) return;
    const patch: ToolReviewSettingsPatch = {
      ...draft,
      ...(apiKey ? { api_key: apiKey } : {}),
    };
    try {
      validateToolReviewSettingsPatch(patch);
    } catch {
      setError("toolReview.invalid");
      return;
    }
    if (draft.enabled && !record.settings.credential_configured && !apiKey) {
      setError("toolReview.keyRequired");
      return;
    }
    if (
      draft.enabled &&
      (!record.settings.enabled || draft.endpoint !== record.settings.endpoint)
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
              disabled={busy || !isReady}
              onClick={() => setReload((value) => value + 1)}
            >
              {t("common.refresh")}
            </Button>
            <Button
              size="sm"
              disabled={!dirty || busy || !isReady || confirmation !== null}
              onClick={requestSave}
            >
              {busy ? t("common.saving") : t("common.save")}
            </Button>
          </>
        }
      >
        <div className="flex items-center gap-2">
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
            disabled={!draft || busy || !isReady}
            onCheckedChange={(enabled) => change({ enabled })}
            size="sm"
          />
          <HelpPopover label={t("toolReview.title")}>
            {t("toolReview.help")}
          </HelpPopover>
        </div>
      </PanelHeader>
      <PanelBody
        className="flex flex-col gap-3"
        data-testid="tool-review-primary"
      >
        {error ? (
          <p role="alert" className="text-sm text-destructive">
            {t(error)}
          </p>
        ) : null}
        {message ? (
          <p role="status" className="text-sm text-success-foreground">
            {t(message)}
          </p>
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
              <Field label="Jev endpoint" htmlFor="tool-review-endpoint">
                <Input
                  id="tool-review-endpoint"
                  value={draft.endpoint}
                  onChange={(event) => change({ endpoint: event.target.value })}
                  autoComplete="off"
                  spellCheck={false}
                />
              </Field>
              <Field
                label={t("toolReview.key")}
                htmlFor="tool-review-key"
                hint={t(
                  record?.settings.credential_configured
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
              <Field label={t("toolReview.mode")} htmlFor="tool-review-mode">
                <Select
                  value={draft.mode}
                  disabled={busy || confirmation !== null}
                  onValueChange={(value) =>
                    change({ mode: value === "reject" ? "reject" : "mark" })
                  }
                >
                  <SelectTrigger id="tool-review-mode" className="w-full">
                    <SelectValue />
                  </SelectTrigger>
                  <SelectContent>
                    <SelectItem value="mark">{t("toolReview.mark")}</SelectItem>
                    <SelectItem value="reject">
                      {t("toolReview.reject")}
                    </SelectItem>
                  </SelectContent>
                </Select>
              </Field>
              <p className="self-center text-xs text-muted-foreground">
                {t(
                  draft.mode === "mark"
                    ? "toolReview.markHint"
                    : "toolReview.rejectHint",
                )}
              </p>
            </fieldset>
            <div className="flex min-h-48 flex-1 flex-col gap-1.5">
              <div className="flex flex-wrap items-center justify-between gap-1.5">
                <Label htmlFor="tool-review-prompt">
                  {t("toolReview.prompt")}
                </Label>
                <div className="flex flex-wrap gap-1">
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
                          {variableButtons}
                          <HelpPopover
                            label={t("toolReview.promptHint")}
                            inDialog
                          >
                            {t("toolReview.promptHint")}
                          </HelpPopover>
                          <DialogClose asChild>
                            <Button size="sm" variant="outline">
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
                className="min-h-32 flex-1 resize-none font-mono"
                value={draft.prompt}
                disabled={busy || confirmation !== null}
                onChange={(event) => change({ prompt: event.target.value })}
                spellCheck={false}
              />
              <p className="text-xs text-muted-foreground">
                {t("toolReview.promptHint")}
              </p>
            </div>
            <details className="shrink-0 text-sm">
              <summary className="cursor-pointer text-text-secondary">
                {t("toolReview.advanced")}
              </summary>
              <fieldset
                disabled={busy || confirmation !== null}
                className="mt-3 grid gap-3 sm:grid-cols-2"
              >
                <Field
                  label={t("toolReview.model")}
                  htmlFor="tool-review-model"
                >
                  <Input
                    id="tool-review-model"
                    value={draft.model}
                    onChange={(event) => change({ model: event.target.value })}
                  />
                </Field>
                <Field
                  label={t("toolReview.timeout")}
                  htmlFor="tool-review-timeout"
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
            </details>
            <p className="shrink-0 text-xs text-muted-foreground">
              {t("toolReview.nextRequests")}
            </p>
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
            <p className="break-all font-mono text-xs">
              {confirmation?.endpoint}
            </p>
          </>
        }
      />
    </Panel>
  );
}
