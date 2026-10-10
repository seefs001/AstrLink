import { describe, expect, it } from "vitest";
import {
  emptyTrajectoryFields,
  type AuditContent,
  type RequestRecord,
} from "./request-record-model";
import { parseResponsePreview } from "./response-preview-model";
import { locateReviewedOutputs } from "./tool-review-location";

function record(
  protocol = "openai.responses",
  names = ["same"],
): RequestRecord {
  return {
    ...emptyTrajectoryFields,
    id: "request",
    parent_request_id: null,
    attempt_index: 1,
    child_count: 0,
    started_at: "2026-10-01T00:00:00Z",
    completed_at: "2026-10-01T00:00:01Z",
    status: "succeeded",
    input_protocol: protocol,
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
      calls: names.map((name, index) => ({
        name,
        index,
        action: "delivered",
        verdict: "pass",
        model: "test",
        latency_ms: 1,
      })),
    },
  };
}
function content(value: unknown, stream = false): AuditContent {
  const body = stream
    ? (value as unknown[])
        .map((frame) => `data: ${JSON.stringify(frame)}\n\n`)
        .join("")
    : JSON.stringify(value);
  return {
    request_id: "request",
    view: "shareable",
    http_meta: null,
    request_body: null,
    response_content: {
      content: body,
      media_type: stream ? "text/event-stream" : "application/json",
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
const fn = (id = "a", args = "{}") => ({
  type: "function_call",
  id,
  call_id: `call_${id}`,
  name: "same",
  arguments: args,
});
const final = (output: unknown[] = []) => ({
  type: "response.completed",
  response: { status: "completed", output },
});
const item = (value: unknown, index: number, done = true) => ({
  type: `response.output_item.${done ? "done" : "added"}`,
  item: value,
  output_index: index,
});
const response = (output: unknown[] = [fn()]) => ({
  status: "completed",
  output,
});
async function locate(r: RequestRecord, c: AuditContent) {
  const part = c.response_content ?? c.upstream_response_content;
  const preview = part
    ? await parseResponsePreview(part, undefined, true)
    : { outputs: [], incomplete: false, unparsed: false };
  return locateReviewedOutputs(r, c, preview);
}
function indexes(result: Awaited<ReturnType<typeof locate>>) {
  return result.outputs
    .filter((output) => output.reviewIndex !== undefined)
    .map((output) => [output.id, output.reviewIndex]);
}
async function unreliable(r: RequestRecord, c: AuditContent) {
  const result = await locate(r, c);
  expect(result.failure).toBe("unreliable");
  expect(indexes(result)).toEqual([]);
}

describe("reviewed CLIENT tool ordinals", () => {
  it("filters reasoning and hosted tools, ordering same-name function/custom calls by slot, not arrival", async () => {
    const custom = {
      type: "custom_tool_call",
      id: "b",
      call_id: "call_b",
      name: "same",
      input: "plain input",
    };
    const result = await locate(
      record("openai.responses", ["same", "same"]),
      content(
        [
          item(custom, 4),
          item({ type: "web_search_call", id: "hosted" }, 1),
          item(fn(), 2),
          item({ type: "reasoning", summary: [{ text: "thinking" }] }, 0),
          final(),
        ],
        true,
      ),
    );
    expect(result.failure).toBeUndefined();
    expect(indexes(result)).toEqual([
      ["call_b", 1],
      ["call_a", 0],
    ]);
    expect(
      result.outputs.find((output) => output.name === "web_search_call")
        ?.reviewIndex,
    ).toBeUndefined();
    expect(result.outputs.some((output) => output.kind === "reasoning")).toBe(
      true,
    );
  });

  it("accepts nonempty matching completed snapshots and done-only empty output", async () => {
    for (const terminal of [final([fn()]), final()]) {
      const result = await locate(
        record(),
        content([item(fn(), 0), terminal], true),
      );
      expect(result.failure).toBeUndefined();
      expect(indexes(result)).toEqual([["call_a", 0]]);
    }
  });

  it("accepts argument deltas plus independently completed items before an empty terminal", async () => {
    const result = await locate(
      record(),
      content(
        [
          item({ ...fn(), arguments: "" }, 0, false),
          {
            type: "response.function_call_arguments.delta",
            item_id: "a",
            delta: '{"n":',
          },
          {
            type: "response.function_call_arguments.delta",
            output_index: 0,
            delta: "1}",
          },
          item(fn("a", '{"n":1}'), 0),
          final(),
        ],
        true,
      ),
    );
    expect(result.failure).toBeUndefined();
  });

  it.each([
    [item(fn(), 0, false), final()],
    [item(fn(), 0), final([{ type: "message", content: [] }])],
    [item(fn(), 1), final([fn()])],
    [item(fn(), 0), final([fn("a", '{"changed":true}')])],
    [item(fn(), 0), final([{ ...fn(), call_id: "different" }])],
    [item(fn(), 0), final([{ ...fn(), name: "other" }])],
    [item(fn(), 0), final(), final([fn()])],
    [
      item(fn(), 0),
      { type: "response.completed", response: { status: "completed" } },
    ],
    [
      item(fn(), 0),
      { type: "response.unknown_tool.delta", delta: "x" },
      final(),
    ],
    [
      item({ ...fn(), arguments: "" }, 0, false),
      {
        type: "response.function_call_arguments.delta",
        output_index: 0,
        delta: '{"v":1}',
      },
      item(fn("a", '{"v":2}'), 0),
      final(),
    ],
  ])(
    "rejects missing done or contradictory Responses snapshots %#",
    async (...frames) => {
      await unreliable(record(), content(frames, true));
    },
  );

  it("uses delivery action, not pending risk classification, including mark errors", async () => {
    const r = record();
    r.tool_review!.status = "pending";
    r.status = "pending";
    r.completed_at = null;
    r.tool_review!.calls[0]!.verdict = "error";
    expect((await locate(r, content(response()))).failure).toBeUndefined();
    for (const action of ["withheld", "pending"] as const) {
      r.tool_review!.calls[0]!.action = action;
      const result = await locate(r, content(response()));
      expect(result.failure).toBe("undelivered");
      expect(indexes(result)).toEqual([]);
    }
  });

  it("sorts Chat candidates and custom slots, preserving hosted-free dense indexes", async () => {
    const r = record("openai.chat", ["same", "same", "same"]);
    const result = await locate(
      r,
      content({
        choices: [
          {
            index: 1,
            finish_reason: "tool_calls",
            message: {
              tool_calls: [
                {
                  index: 0,
                  id: "c",
                  type: "custom",
                  custom: { name: "same", input: "code" },
                },
              ],
            },
          },
          {
            index: 0,
            finish_reason: "tool_calls",
            message: {
              tool_calls: [
                {
                  index: 5,
                  id: "b",
                  type: "function",
                  function: { name: "same", arguments: "{}" },
                },
                {
                  index: 2,
                  id: "a",
                  type: "function",
                  function: { name: "same", arguments: "{}" },
                },
              ],
            },
          },
        ],
      }),
    );
    expect(result.failure).toBeUndefined();
    expect(indexes(result)).toEqual([
      ["c", 2],
      ["b", 1],
      ["a", 0],
    ]);
  });

  it.each([false, true])(
    "supports legacy Chat function calls, streaming=%s",
    async (stream) => {
      const message = { function_call: { name: "same", arguments: "{}" } };
      const frame = {
        choices: [
          {
            index: 0,
            [stream ? "delta" : "message"]: message,
            finish_reason: "function_call",
          },
        ],
      };
      const result = await locate(
        record("openai.chat"),
        content(stream ? [frame] : frame, stream),
      );
      expect(result.failure).toBeUndefined();
      expect(indexes(result)).toEqual([[undefined, 0]]);
    },
  );

  it("supports streamed custom inputs without losing name/argument fragments in ordinary preview", async () => {
    const c = content(
      [
        {
          choices: [
            {
              index: 0,
              delta: {
                tool_calls: [
                  {
                    index: 3,
                    type: "custom",
                    id: "custom",
                    custom: { name: "same", input: "a" },
                  },
                ],
              },
            },
          ],
        },
        {
          choices: [
            {
              index: 0,
              delta: { tool_calls: [{ index: 3, custom: { input: "b" } }] },
              finish_reason: "tool_calls",
            },
          ],
        },
      ],
      true,
    );
    const result = await locate(record("openai.chat"), c);
    expect(result.failure).toBeUndefined();
    expect(result.outputs[0]).toMatchObject({
      name: "same",
      text: "ab",
      reviewIndex: 0,
    });
  });

  it("keeps fragmented Chat names readable but refuses an unprovable collector identity", async () => {
    const c = content(
      [
        {
          choices: [
            {
              index: 0,
              delta: {
                tool_calls: [
                  {
                    index: 0,
                    type: "function",
                    function: { name: "sa", arguments: "{" },
                  },
                ],
              },
            },
          ],
        },
        {
          choices: [
            {
              index: 0,
              delta: {
                tool_calls: [
                  { index: 0, function: { name: "me", arguments: "}" } },
                ],
              },
              finish_reason: "tool_calls",
            },
          ],
        },
      ],
      true,
    );
    const result = await locate(record("openai.chat"), c);
    expect(result.failure).toBe("unreliable");
    expect(result.outputs[0]).toMatchObject({ name: "same", text: "{}" });
    expect(indexes(result)).toEqual([]);
  });

  it("rejects partial/mixed/duplicate Chat slots", async () => {
    const tool = {
      index: 0,
      id: "a",
      type: "function",
      function: { name: "same", arguments: "{}" },
    };
    for (const message of [
      { tool_calls: [tool, tool] },
      { function_call: tool.function, tool_calls: [tool] },
      { tool_calls: [{ ...tool, type: "unknown" }] },
    ])
      await unreliable(
        record("openai.chat"),
        content({
          choices: [{ index: 0, message, finish_reason: "tool_calls" }],
        }),
      );
    await unreliable(
      record("openai.chat"),
      content(
        [{ choices: [{ index: 0, delta: { tool_calls: [tool] } }] }],
        true,
      ),
    );
  });

  it.each([false, true])(
    "keeps Anthropic server tools visible but unindexed, streaming=%s",
    async (stream) => {
      const blocks = [
        { type: "server_tool_use", name: "search", id: "host", input: {} },
        { type: "tool_use", name: "same", id: "a", input: {} },
        { type: "mcp_tool_use", name: "remote", id: "mcp", input: {} },
        { type: "tool_use", name: "same", id: "b", input: { n: 2 } },
      ];
      const c = stream
        ? content(
            [
              ...blocks.flatMap((block, index) => [
                { type: "content_block_start", index, content_block: block },
                { type: "content_block_stop", index },
              ]),
              { type: "message_delta", delta: { stop_reason: "tool_use" } },
              { type: "message_stop" },
            ],
            true,
          )
        : content({
            type: "message",
            content: blocks,
            stop_reason: "tool_use",
          });
      const result = await locate(
        record("anthropic.messages", ["same", "same"]),
        c,
      );
      expect(result.failure).toBeUndefined();
      expect(indexes(result)).toEqual([
        ["a", 0],
        ["b", 1],
      ]);
      expect(
        result.outputs.find((output) => output.id === "host")?.reviewIndex,
      ).toBeUndefined();
    },
  );

  const gemini = (functions: unknown[], finishReason?: string, index = 0) => ({
    candidates: [
      {
        index,
        content: { parts: functions.map((functionCall) => ({ functionCall })) },
        finishReason,
      },
    ],
  });
  const gf = (id?: string, n = 1) => ({
    name: "same",
    args: { n },
    ...(id ? { id } : {}),
  });

  it("locates Gemini JSON, chunk arrays and SSE ID snapshots without replacing narration", async () => {
    const first = {
      candidates: [
        {
          index: 0,
          content: {
            parts: [{ text: "narration" }, { functionCall: gf("a") }],
          },
        },
      ],
    };
    const frames = [
      first,
      gemini([gf("a")], "STOP"),
      gemini([gf("a")], "STOP"),
    ];
    for (const c of [content(frames), content(frames, true)]) {
      const result = await locate(record("google.generate_content"), c);
      expect(result.failure).toBeUndefined();
      expect(result.outputs).toHaveLength(2);
      expect(result.outputs[0]).toMatchObject({
        kind: "text",
        text: "narration",
      });
      expect(indexes(result)).toEqual([[undefined, 0]]);
    }
    expect(
      (
        await locate(
          record("google.generate_content"),
          content(gemini([gf()], "STOP")),
        )
      ).failure,
    ).toBeUndefined();
  });

  it("does not deduplicate equal no-ID nonterminal Gemini calls by name or args", async () => {
    const result = await locate(
      record("google.generate_content", ["same", "same"]),
      content([gemini([gf()]), gemini([gf()]), gemini([], "STOP")], true),
    );
    expect(result.failure).toBeUndefined();
    expect(indexes(result)).toEqual([
      [undefined, 0],
      [undefined, 1],
    ]);
  });

  it("accepts repeated initial final snapshots by position and sorts multiple Gemini candidates", async () => {
    const frame = {
      candidates: [
        gemini([gf("b")], "STOP", 1).candidates[0],
        gemini([gf("a")], "STOP", 0).candidates[0],
      ],
    };
    const result = await locate(
      record("google.generate_content", ["same", "same"]),
      content([frame, frame], true),
    );
    expect(result.failure).toBeUndefined();
    expect(indexes(result)).toEqual([
      [undefined, 1],
      [undefined, 0],
    ]);
    const noIDFinal = gemini([gf()], "STOP");
    expect(
      (
        await locate(
          record("google.generate_content"),
          content([noIDFinal, noIDFinal], true),
        )
      ).failure,
    ).toBeUndefined();
  });

  it("rejects ambiguous no-ID cumulative finals, conflicting IDs and partial Gemini inputs", async () => {
    for (const frames of [
      [gemini([gf()]), gemini([gf()], "STOP")],
      [gemini([gf("a")]), gemini([gf("a", 2)], "STOP")],
      [gemini([gf("a"), gf("a")], "STOP")],
      [gemini([{ ...gf(), willContinue: true }], "STOP")],
      [gemini([{ ...gf(), partialArgs: [] }], "STOP")],
      [gemini([gf()], "MAX_TOKENS")],
      [gemini([gf()])],
    ])
      await unreliable(
        record("google.generate_content"),
        content(frames, true),
      );
  });

  it("rejects missing candidate indexes in multicandidate streams and unsafe numeric snapshots", async () => {
    await unreliable(
      record("google.generate_content", ["same", "same"]),
      content(
        [
          {
            candidates: [
              {
                content: { parts: [{ functionCall: gf("a") }] },
                finishReason: "STOP",
              },
            ],
          },
          gemini([gf("b")], "STOP", 1),
        ],
        true,
      ),
    );
    const c = content(gemini([gf("a")], "STOP"));
    c.response_content!.content = c.response_content!.content.replace(
      '"n":1',
      '"n":9007199254740993',
    );
    await unreliable(record("google.generate_content"), c);
  });

  it("does not delete ordinary outputs on a failed Gemini proof", async () => {
    const c = content([gemini([gf()]), gemini([gf()], "STOP")], true);
    const preview = await parseResponsePreview(
      c.response_content!,
      undefined,
      true,
    );
    const result = await locateReviewedOutputs(
      record("google.generate_content"),
      c,
      preview,
    );
    expect(result.failure).toBe("unreliable");
    expect(result.outputs).toEqual(preview.outputs);
    expect(result.outputs).toHaveLength(2);
  });

  it("requires this request's readable untruncated CLIENT capture and removes old proof", async () => {
    const r = record(),
      good = content(response());
    const stale = await parseResponsePreview(
      good.response_content!,
      undefined,
      true,
    );
    stale.outputs[0]!.reviewIndex = 0;
    const cases: Array<[AuditContent, string]> = [
      [{ ...good, request_id: "different" }, "unreliable"],
      [{ ...good, response_content: null }, "missing"],
      [
        {
          ...good,
          response_content: null,
          upstream_response_content: good.response_content,
        },
        "missing",
      ],
      [
        {
          ...good,
          response_content: { ...good.response_content!, truncated: true },
        },
        "unreliable",
      ],
      [
        {
          ...good,
          response_content: null,
          upstream_response_content: good.response_content,
          withheld: {
            response_content: {
              reason: "raw_locked",
              raw_available: true,
              media_type: "application/json",
              truncated: false,
              captured_bytes: 10,
            },
          },
        },
        "locked",
      ],
    ];
    for (const [c, failure] of cases) {
      const result = await locateReviewedOutputs(r, c, stale);
      expect(result.failure).toBe(failure);
      expect(indexes(result)).toEqual([]);
    }
    r.tool_review = undefined;
    expect(
      (await locateReviewedOutputs(r, good, stale)).outputs[0]!.reviewIndex,
    ).toBeUndefined();
  });

  it("does not prove a preview from a different body even if the name and slot match", async () => {
    const preview = await parseResponsePreview(
      content(response([fn("a", '{"other":true}')])).response_content!,
      undefined,
      true,
    );
    const result = await locateReviewedOutputs(
      record(),
      content(response()),
      preview,
    );
    expect(result.failure).toBe("unreliable");
    expect(indexes(result)).toEqual([]);
  });

  it("rejects invalid SSE, truncated tails, summary gaps and unknown shapes", async () => {
    const r = record();
    r.tool_review!.calls[0]!.index = 2;
    await unreliable(r, content(response()));
    await unreliable(
      record(),
      content(response([{ type: "unknown_tool", name: "same" }])),
    );
    await unreliable(record(), content([response()]));
    for (const tail of [
      "data: {oops}\n\n",
      "data: {}",
      "data: null\n\n",
      "data: []\n\n",
      "data: [DONE]\n\ndata: {}\n\n",
    ]) {
      const c = content([item(fn(), 0), final()], true);
      c.response_content!.content += tail;
      await unreliable(record(), c);
    }
  });
});
