import { useCallback, useEffect, useMemo, useRef, useState } from "react";

import { getRequestAuditContent } from "./bridge";
import { i18n } from "./i18n";
import {
  conversationResponsePart,
  parseCallContent,
  type ParsedCall,
} from "./request-conversation-model";
import type { RequestRecord } from "./request-record-model";
import { parseResponsePreview } from "./response-preview-model";
import { locateReviewedOutputs } from "./tool-review-location";

/** Decrypts in flight at once. Bodies run to megabytes, so two is plenty. */
const CONCURRENCY = 2;

export interface ConversationContent {
  parsed: ReadonlyMap<string, ParsedCall>;
  reading: ReadonlySet<string>;
  errors: ReadonlyMap<string, string>;
  /** Reads these calls' content, in the order given, skipping ones already read. */
  request: (records: RequestRecord[]) => void;
}

// A pending call's content grows until it settles, and a capture flag that
// flips means Core now holds a body it did not have. Either reads again.
export function conversationContentVersion(record: RequestRecord): string {
  return [
    record.status,
    record.completed_at,
    record.input_protocol,
    record.streaming,
    JSON.stringify(record.audit),
    JSON.stringify(record.tool_review),
  ].join(":");
}

/**
 * Reads call content for the conversation view on demand and keeps only the
 * parsed result. The original bodies are let go as soon as they are parsed,
 * so an agent turn with hundreds of calls costs the view its summaries, not
 * hundreds of megabytes. `resetKey` drops everything, which is how an unlock
 * that ends takes raw-derived text off the screen.
 */
export function useConversationContent(resetKey: string): ConversationContent {
  const [parsed, setParsed] = useState<ReadonlyMap<string, ParsedCall>>(
    () => new Map(),
  );
  const [reading, setReading] = useState<ReadonlySet<string>>(() => new Set());
  const [errors, setErrors] = useState<ReadonlyMap<string, string>>(
    () => new Map(),
  );
  const generationRef = useRef(0);
  const queueRef = useRef<RequestRecord[]>([]);
  const inFlightRef = useRef(new Set<string>());
  const versionRef = useRef(new Map<string, string>());
  const requestedVersionRef = useRef(new Map<string, string>());
  const cacheKeyRef = useRef(resetKey);
  const activeKeyRef = useRef(resetKey);
  activeKeyRef.current = resetKey;

  useEffect(() => {
    generationRef.current += 1;
    queueRef.current = [];
    inFlightRef.current = new Set();
    versionRef.current = new Map();
    requestedVersionRef.current = new Map();
    cacheKeyRef.current = resetKey;
    setParsed(new Map());
    setReading(new Set());
    setErrors(new Map());
  }, [resetKey]);

  useEffect(
    () => () => {
      generationRef.current += 1;
    },
    [],
  );

  const pump = useCallback(() => {
    const generation = generationRef.current;
    const key = activeKeyRef.current;
    while (
      inFlightRef.current.size < CONCURRENCY &&
      queueRef.current.length > 0
    ) {
      const next = queueRef.current.findIndex(
        (record) => !inFlightRef.current.has(record.id),
      );
      if (next < 0) break;
      const [record] = queueRef.current.splice(next, 1);
      inFlightRef.current.add(record.id);
      setReading((current) => new Set(current).add(record.id));
      const version = conversationContentVersion(record);
      const current = () =>
        generationRef.current === generation && activeKeyRef.current === key;
      const wanted = () =>
        current() && requestedVersionRef.current.get(record.id) === version;
      const finish = () => {
        if (!current()) return;
        inFlightRef.current.delete(record.id);
        setReading((current) => {
          const next = new Set(current);
          next.delete(record.id);
          return next;
        });
        pump();
      };
      void getRequestAuditContent(record.id)
        .then(async (content) => {
          const part = conversationResponsePart(content);
          const preview = part
            ? await parseResponsePreview(part, undefined, true)
            : { outputs: [], incomplete: false, unparsed: false };
          const located = await locateReviewedOutputs(record, content, preview);
          if (!wanted()) return;
          const call = parseCallContent(record, content, located.outputs);
          call.toolLocationFailure = located.failure;
          call.contentVersion = version;
          versionRef.current.set(record.id, version);
          setParsed((current) => new Map(current).set(record.id, call));
          setErrors((current) => {
            if (!current.has(record.id)) return current;
            const next = new Map(current);
            next.delete(record.id);
            return next;
          });
        })
        .catch((error: unknown) => {
          if (!wanted()) return;
          const message =
            error instanceof Error && error.message
              ? error.message
              : i18n.t("records.auditContentFailed");
          setErrors((current) => new Map(current).set(record.id, message));
        })
        .finally(finish);
    }
  }, []);

  const request = useCallback(
    (records: RequestRecord[]) => {
      let queued = false;
      for (const record of records) {
        const version = conversationContentVersion(record);
        if (versionRef.current.get(record.id) === version) continue;
        if (
          requestedVersionRef.current.get(record.id) === version &&
          (inFlightRef.current.has(record.id) ||
            queueRef.current.some((queued) => queued.id === record.id))
        )
          continue;
        requestedVersionRef.current.set(record.id, version);
        versionRef.current.delete(record.id);
        // Invalidate proof immediately, not after a replacement body arrives.
        setParsed((current) => {
          if (!current.has(record.id)) return current;
          const next = new Map(current);
          next.delete(record.id);
          return next;
        });
        setErrors((current) => {
          if (!current.has(record.id)) return current;
          const next = new Map(current);
          next.delete(record.id);
          return next;
        });
        const at = queueRef.current.findIndex(
          (queued) => queued.id === record.id,
        );
        if (at < 0) queueRef.current.push(record);
        else queueRef.current[at] = record;
        queued = true;
      }
      if (queued) pump();
    },
    [pump],
  );

  return useMemo(
    () =>
      cacheKeyRef.current === resetKey
        ? { parsed, reading, errors, request }
        : {
            parsed: new Map<string, ParsedCall>(),
            reading: new Set<string>(),
            errors: new Map<string, string>(),
            request,
          },
    [parsed, reading, errors, request, resetKey],
  );
}
