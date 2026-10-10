// @vitest-environment happy-dom

import { act } from "react";
import { createRoot, type Root } from "react-dom/client";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import {
  emptyTrajectoryFields,
  type AuditContent,
  type RequestRecord,
} from "./request-record-model";
import {
  conversationContentVersion,
  useConversationContent,
  type ConversationContent,
} from "./use-conversation-content";

const fetchContent = vi.hoisted(() => vi.fn());
vi.mock("./bridge", () => ({ getRequestAuditContent: fetchContent }));
let root: Root;
let container: HTMLDivElement;
let reader: ConversationContent;
const renders: ConversationContent[] = [];
function Reader({ resetKey }: { resetKey: string }) {
  reader = useConversationContent(resetKey);
  renders.push(reader);
  return null;
}
function record(id = "request"): RequestRecord {
  return {
    ...emptyTrajectoryFields,
    id,
    parent_request_id: null,
    attempt_index: 1,
    child_count: 0,
    started_at: "2026-10-01T00:00:00Z",
    completed_at: "2026-10-01T00:00:01Z",
    status: "succeeded",
    input_protocol: "openai.responses",
    requested_model: "test",
    streaming: false,
    route_id: null,
    service_id: null,
    local_access_token_id: null,
    http_status: 200,
    latency_ms: 1000,
    usage: null,
    error: null,
    privacy_restore: null,
    audit: {
      request_body_captured: false,
      response_content_captured: true,
      request_body_truncated: false,
      response_content_truncated: false,
      upstream_request_body_captured: false,
      upstream_response_content_captured: false,
      upstream_request_body_truncated: false,
      upstream_response_content_truncated: false,
    },
    tool_review: {
      status: "passed",
      mode: "mark",
      model: "test",
      config_revision: "1",
      started_at: "2026-10-01T00:00:00Z",
      latency_ms: 1,
      calls: [
        {
          name: "same",
          index: 0,
          action: "delivered",
          verdict: "pass",
          model: "test",
          latency_ms: 1,
        },
      ],
    },
  };
}
function content(id = "request"): AuditContent {
  const body = JSON.stringify({
    status: "completed",
    output: [
      {
        type: "function_call",
        id: "item",
        call_id: "call",
        name: "same",
        arguments: "{}",
      },
    ],
  });
  return {
    request_id: id,
    view: "full",
    http_meta: null,
    request_body: null,
    response_content: {
      content: body,
      exposure: "raw",
      media_type: "application/json",
      truncated: false,
      captured_bytes: body.length,
    },
    upstream_http_meta: null,
    upstream_request_body: null,
    upstream_response_content: null,
    withheld: {},
    privacy_findings: [],
  };
}
function locked(): AuditContent {
  return {
    ...content(),
    response_content: null,
    withheld: {
      response_content: {
        reason: "raw_locked",
        raw_available: true,
        media_type: "application/json",
        captured_bytes: 20,
        truncated: false,
      },
    },
  };
}
function deferred() {
  let resolve!: (value: AuditContent) => void;
  let reject!: (error: Error) => void;
  const promise = new Promise<AuditContent>((done, fail) => {
    resolve = done;
    reject = fail;
  });
  return { promise, resolve, reject };
}
async function render(resetKey = "unlocked") {
  await act(async () => root.render(<Reader resetKey={resetKey} />));
}
async function request(...records: RequestRecord[]) {
  await act(async () => reader.request(records));
}
beforeEach(async () => {
  fetchContent
    .mockReset()
    .mockImplementation(async (id: string) => content(id));
  container = document.createElement("div");
  document.body.append(container);
  root = createRoot(container);
  renders.length = 0;
  await render();
});
afterEach(async () => {
  await act(async () => root.unmount());
  container.remove();
});

it("loads only on demand, caches a verified version and carries the index", async () => {
  expect(fetchContent).not.toHaveBeenCalled();
  const r = record();
  await request(r);
  expect(fetchContent).toHaveBeenCalledExactlyOnceWith(r.id);
  expect(reader.parsed.get(r.id)).toMatchObject({
    contentVersion: conversationContentVersion(r),
    outputs: [{ reviewIndex: 0 }],
  });
  await request(r, { ...r });
  expect(fetchContent).toHaveBeenCalledTimes(1);
});

it("removes old proof before reading a new audit/review version, including failures", async () => {
  const r = record();
  await request(r);
  const changed = {
    ...r,
    audit: { ...r.audit, response_content_truncated: true },
  };
  expect(conversationContentVersion(changed)).not.toBe(
    conversationContentVersion(r),
  );
  const pending = deferred();
  fetchContent.mockReturnValueOnce(pending.promise);
  await request(changed);
  expect(reader.parsed.has(r.id)).toBe(false);
  expect(reader.reading.has(r.id)).toBe(true);
  await act(async () => pending.reject(new Error("read failed")));
  expect(reader.parsed.has(r.id)).toBe(false);
  expect(reader.errors.get(r.id)).toBe("read failed");
  await request(changed);
  expect(reader.errors.has(r.id)).toBe(false);
  expect(reader.parsed.get(r.id)?.contentVersion).toBe(
    conversationContentVersion(changed),
  );
});

it("queues the newest version while the old one is in flight and ignores its late proof", async () => {
  const oldRead = deferred(),
    newRead = deferred();
  fetchContent
    .mockReturnValueOnce(oldRead.promise)
    .mockReturnValueOnce(newRead.promise);
  const r = record();
  await request(r);
  const pending = {
    ...r,
    tool_review: { ...r.tool_review!, status: "pending" as const },
  };
  const withheld = {
    ...pending,
    tool_review: {
      ...pending.tool_review,
      calls: pending.tool_review.calls.map((call) => ({
        ...call,
        action: "withheld" as const,
      })),
    },
  };
  await request(pending);
  await request(withheld);
  expect(fetchContent).toHaveBeenCalledTimes(1);
  await act(async () => oldRead.resolve(content()));
  expect(fetchContent).toHaveBeenCalledTimes(2);
  expect(reader.parsed.has(r.id)).toBe(false);
  await act(async () => newRead.resolve(content()));
  const call = reader.parsed.get(r.id)!;
  expect(call.contentVersion).toBe(conversationContentVersion(withheld));
  expect(call.toolLocationFailure).toBe("undelivered");
  expect(call.outputs[0]).not.toHaveProperty("reviewIndex");
});

it("ignores an obsolete read error and still pumps the replacement version", async () => {
  const oldRead = deferred();
  fetchContent.mockReturnValueOnce(oldRead.promise);
  const r = record();
  await request(r);
  const changed = {
    ...r,
    tool_review: { ...r.tool_review!, status: "pending" as const },
  };
  await request(changed);
  await act(async () => oldRead.reject(new Error("obsolete")));
  expect(reader.errors.size).toBe(0);
  expect(reader.parsed.get(r.id)?.outputs[0]?.reviewIndex).toBe(0);
  expect(reader.parsed.get(r.id)?.contentVersion).toBe(
    conversationContentVersion(changed),
  );
});

it("clears cached raw indexes on lock before rendering and rejects late pre-lock reads", async () => {
  await request(record());
  const oldRead = deferred();
  fetchContent.mockReturnValueOnce(oldRead.promise);
  await request(record("late"));
  renders.length = 0;
  await render("locked");
  expect(renders.every((state) => state.parsed.size === 0)).toBe(true);
  fetchContent.mockResolvedValueOnce(locked());
  await request(record());
  await act(async () => oldRead.resolve(content("late")));
  expect(reader.parsed.has("late")).toBe(false);
  expect(reader.parsed.get("request")).toMatchObject({
    locked: true,
    toolLocationFailure: "locked",
    outputs: [],
  });
});

it("does not let an old unlock overwrite a fresh read of the same request", async () => {
  const oldRead = deferred(),
    newRead = deferred();
  fetchContent
    .mockReturnValueOnce(oldRead.promise)
    .mockReturnValueOnce(newRead.promise);
  await request(record());
  await render("locked");
  await request(record());
  await act(async () => newRead.resolve(locked()));
  await act(async () => oldRead.resolve(content()));
  expect(reader.parsed.get("request")?.toolLocationFailure).toBe("locked");
  expect(reader.parsed.get("request")?.outputs).toEqual([]);
  expect(reader.reading.size).toBe(0);
});

it("retains upstream fallback display but never treats it as client tool proof", async () => {
  const c = content();
  fetchContent.mockResolvedValueOnce({
    ...c,
    response_content: null,
    upstream_response_content: c.response_content,
  });
  await request(record());
  expect(reader.parsed.get("request")).toMatchObject({
    toolLocationFailure: "missing",
    outputs: [{ kind: "tool", name: "same" }],
  });
  expect(reader.parsed.get("request")?.outputs[0]).not.toHaveProperty(
    "reviewIndex",
  );
});

it("bounds concurrent reads and retains ordinary no-review behavior", async () => {
  const first = deferred(),
    second = deferred();
  fetchContent
    .mockReturnValueOnce(first.promise)
    .mockReturnValueOnce(second.promise);
  const records = [
    record("a"),
    record("b"),
    { ...record("c"), tool_review: undefined },
  ];
  await request(...records);
  expect(fetchContent).toHaveBeenCalledTimes(2);
  await act(async () => first.resolve(content("a")));
  expect(fetchContent).toHaveBeenCalledTimes(3);
  expect(reader.parsed.get("c")?.outputs[0]).not.toHaveProperty("reviewIndex");
  expect(reader.parsed.get("c")?.toolLocationFailure).toBeUndefined();
  await act(async () => second.resolve(content("b")));
  expect(reader.reading.size).toBe(0);
});
