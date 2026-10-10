import type { AuditContent, RequestRecord } from "./request-record-model";
import type { ResponseOutput, ResponsePreview } from "./response-preview-model";
import { parseSSEIncremental } from "./sse-review-model";

export type ToolLocationFailure =
  | "missing"
  | "locked"
  | "undelivered"
  | "unreliable";
export interface ReviewedOutputs {
  outputs: ResponseOutput[];
  failure?: ToolLocationFailure;
}
type Obj = Record<string, unknown>;
const obj = (value: unknown): Obj =>
  value !== null && typeof value === "object" && !Array.isArray(value)
    ? (value as Obj)
    : {};
const arr = (value: unknown): unknown[] => (Array.isArray(value) ? value : []);
const str = (value: unknown): string =>
  typeof value === "string" ? value : "";
const invalid: () => never = () => {
  throw new Error("Unreliable tool location");
};
const slot = (value: unknown): number =>
  Number.isSafeInteger(value) && Number(value) >= 0 ? Number(value) : invalid();
// Deliberately conservative: ignore JSON whitespace, not number spelling or
// object order. A locator need not accept every equivalence the reviewer does.
const compact = (text: string) =>
  text.replace(/"(?:\\.|[^"\\])*"|\s+/g, (token) =>
    token.startsWith('"') ? token : "",
  );
const sameArgs = (a: string, b: string, kind: string) =>
  kind === "custom" ? a === b : compact(a) === compact(b);
const objectArguments = (value: unknown): string => {
  if (!value || typeof value !== "object" || Array.isArray(value)) invalid();
  return JSON.stringify(
    value,
    (_key, entry: unknown) => {
      // Native JSON parsing cannot distinguish unsafe integer snapshots.
      if (
        typeof entry === "number" &&
        Number.isInteger(entry) &&
        !Number.isSafeInteger(entry)
      )
        invalid();
      return entry;
    },
    2,
  );
};
interface Location {
  key: string;
  group: number;
  slot: number;
  name: string;
  kind: string;
  id: string;
  text?: string;
  snapshot?: boolean;
  done?: boolean;
  itemDone?: boolean;
}
const responseOther = new Set([
  "message",
  "reasoning",
  "compaction",
  "compaction_summary",
  "web_search_call",
  "file_search_call",
  "code_interpreter_call",
  "image_generation_call",
  "mcp_call",
  "mcp_list_tools",
  "mcp_approval_request",
  "function_call_output",
  "custom_tool_call_output",
]);
const anthropicOther = new Set([
  "server_tool_use",
  "mcp_tool_use",
  "text",
  "thinking",
  "redacted_thinking",
  "tool_result",
  "mcp_tool_result",
  "web_search_tool_result",
  "web_fetch_tool_result",
  "code_execution_tool_result",
  "bash_code_execution_tool_result",
  "text_editor_code_execution_tool_result",
  "container_upload",
]);

/**
 * Prove Core's dense group/slot ordinal from this request's readable CLIENT
 * response. No name-first matching and no upstream evidence. This only locates
 * already delivered calls; it does not reproduce the safety review. Unfamiliar
 * or ambiguous wire shapes are left visible, but never get a review index.
 */
export async function locateReviewedOutputs(
  record: RequestRecord,
  content: AuditContent,
  preview: ResponsePreview,
): Promise<ReviewedOutputs> {
  // Even a reused preview must not carry an old proof through a failed read.
  const outputs = preview.outputs.map(
    ({ reviewIndex: _reviewIndex, ...output }) => output,
  );
  const fail = (failure: ToolLocationFailure): ReviewedOutputs => ({
    outputs,
    failure,
  });
  const summary = record.tool_review;
  if (!summary) return { outputs };
  if (content.request_id !== record.id) return fail("unreliable");
  if (content.withheld.response_content)
    return fail(
      content.withheld.response_content.reason === "raw_locked"
        ? "locked"
        : "missing",
    );
  const part = content.response_content;
  if (!part) return fail("missing");
  // Risk classification may still be pending after mark-mode delivery.
  if (
    summary.calls.length === 0 ||
    summary.calls.some((call) => call.action !== "delivered")
  )
    return fail("undelivered");
  if (
    part.truncated ||
    record.audit.response_content_truncated ||
    preview.incomplete ||
    preview.unparsed
  )
    return fail("unreliable");
  try {
    const stream =
      part.media_type.toLowerCase().includes("text/event-stream") ||
      /^\s*(?:event:|data:)/.test(part.content);
    const frameObject = (value: unknown): Obj => {
      if (!value || typeof value !== "object" || Array.isArray(value))
        invalid();
      return value as Obj;
    };
    let frames: Obj[];
    if (stream) {
      const parsed = await parseSSEIncremental(part.content);
      if (parsed.invalidJsonCount || parsed.incompleteLastEvent)
        return fail("unreliable");
      const done = parsed.events.findIndex((event) => event.done);
      if (
        done >= 0 &&
        parsed.events.slice(done + 1).some((event) => event.data.trim())
      )
        invalid();
      frames = parsed.events
        .filter((event) => event.data.trim() && !event.done)
        .map((event) => {
          const value = frameObject(event.json);
          return { ...value, type: value.type ?? event.type };
        });
    } else {
      const value: unknown = JSON.parse(part.content);
      if (
        Array.isArray(value) &&
        record.input_protocol !== "google.generate_content"
      )
        invalid();
      frames = (Array.isArray(value) ? value : [value]).map(frameObject);
    }
    const calls = new Map<string, Location>();
    const ids = new Map<string, string>();
    const completed = new Set<number>();
    const groups = new Set<number>();
    let implicitChunkGroup = false;
    const aliases = new Map<string, string>();
    const bind = (id: string, key: string) => {
      if (ids.has(id) && ids.get(id) !== key) invalid();
      ids.set(id, key);
    };
    const add = (
      key: string,
      group: number,
      position: number,
      name: unknown,
      kind: string,
      id: unknown = "",
    ) => {
      const previous = calls.get(key);
      const n = str(name),
        identity = str(id);
      if (!previous && completed.has(group)) invalid();
      if (
        previous &&
        ((n && previous.name && previous.name !== n) ||
          previous.kind !== kind ||
          (identity && previous.id && previous.id !== identity))
      )
        invalid();
      if (identity) bind(identity, key);
      const call: Location = previous ?? {
        key,
        group,
        slot: position,
        kind,
        name: "",
        id: "",
      };
      call.name = n || call.name;
      call.id = identity || call.id;
      calls.set(key, call);
      groups.add(group);
      return call;
    };
    const snapshot = (call: Location, text: unknown) => {
      if (typeof text !== "string") invalid();
      if (call.text !== undefined && !sameArgs(call.text, text, call.kind))
        invalid();
      call.text = text;
      call.snapshot = true;
    };
    const append = (call: Location | undefined, text: unknown) => {
      if (
        !call ||
        call.done ||
        call.snapshot ||
        completed.has(call.group) ||
        typeof text !== "string"
      )
        invalid();
      call.text = (call.text ?? "") + text;
    };
    const responseSlots = new Map<number, string>();
    const responseCallIDs = new Map<string, string>();
    let responseEmptyFinal: boolean | undefined;
    const responseItem = (value: unknown, position: number, final: boolean) => {
      const item = obj(value),
        type = str(item.type);
      if (responseSlots.has(position) && responseSlots.get(position) !== type)
        invalid();
      responseSlots.set(position, type);
      if (type !== "function_call" && type !== "custom_tool_call") {
        if (!responseOther.has(type)) invalid();
        return;
      }
      const key = `response:${position}:tool`;
      const call = add(
        key,
        0,
        position,
        item.name,
        type === "function_call" ? "function" : "custom",
        item.id,
      );
      const callID = str(item.call_id);
      if (callID) {
        if (responseCallIDs.has(key) && responseCallIDs.get(key) !== callID)
          invalid();
        bind(`call_id:${callID}`, key);
        responseCallIDs.set(key, callID);
      }
      const text = call.kind === "custom" ? item.input : item.arguments;
      if (final || (text !== undefined && text !== "")) snapshot(call, text);
      if (final) {
        if (item.status && item.status !== "completed") invalid();
        call.done = true;
      }
    };
    const blocks = new Map<number, boolean>();
    const legacyGroups = new Map<number, boolean>();
    const geminiOccurrences = new Map<number, number>();
    const geminiFinals = new Map<number, string[]>();
    for (const frame of frames) {
      if (frame.error || frame.type === "error") return fail("unreliable");
      const type = str(frame.type);
      if (record.input_protocol.startsWith("openai.responses")) {
        if (
          [
            "response.failed",
            "response.incomplete",
            "response.cancelled",
          ].includes(type) ||
          obj(frame.response).error
        )
          invalid();
        if (
          type === "response.output_item.added" ||
          type === "response.output_item.done"
        ) {
          const position = slot(frame.output_index);
          responseItem(frame.item, position, type.endsWith(".done"));
          const call = calls.get(`response:${position}:tool`);
          if (call && type.endsWith(".done")) call.itemDone = true;
        } else if (
          /^response\.(function_call_arguments|custom_tool_call_input)\.(delta|done)$/.test(
            type,
          )
        ) {
          const byID = frame.item_id ? ids.get(str(frame.item_id)) : undefined;
          const bySlot =
            frame.output_index === undefined
              ? undefined
              : `response:${slot(frame.output_index)}:tool`;
          if ((frame.item_id && !byID) || (byID && bySlot && byID !== bySlot))
            invalid();
          const call = calls.get(bySlot ?? byID ?? "");
          if (
            !call ||
            (call.kind === "custom") !== type.includes("custom_tool")
          )
            invalid();
          if (type.endsWith(".delta")) append(call, frame.delta);
          else {
            snapshot(
              call!,
              call!.kind === "custom" ? frame.input : frame.arguments,
            );
            call!.done = true;
          }
        } else if (
          /(?:_call|_tool|\.tool)/.test(type) &&
          !/^response\.(web_search_call|file_search_call|code_interpreter_call|image_generation_call|mcp_call|mcp_list_tools)\./.test(
            type,
          )
        )
          invalid();
        const response = type.startsWith("response.")
          ? obj(frame.response)
          : frame;
        const terminal =
          !stream || ["response.completed", "response.done"].includes(type);
        if (terminal) {
          if (
            response.status !== "completed" ||
            !Array.isArray(response.output)
          )
            invalid();
          const items = arr(response.output);
          const emptyFinal = stream && items.length === 0 && calls.size > 0;
          if (
            responseEmptyFinal !== undefined &&
            responseEmptyFinal !== emptyFinal
          )
            invalid();
          responseEmptyFinal = emptyFinal;
          const seen = new Set<string>();
          items.forEach((item, index) => {
            responseItem(item, index, true);
            if (
              ["function_call", "custom_tool_call"].includes(
                str(obj(item).type),
              )
            )
              seen.add(`response:${index}:tool`);
          });
          if (
            [...calls.values()].some(
              (call) => !seen.has(call.key) && !(emptyFinal && call.itemDone),
            )
          )
            invalid();
          completed.add(0);
        } else if (
          arr(response.output).some(
            (item) => !responseOther.has(str(obj(item).type)),
          )
        )
          invalid();
      } else if (record.input_protocol === "openai.chat") {
        const seen = new Set<number>();
        arr(frame.choices).forEach((value, ordinal) => {
          const choice = obj(value),
            group = slot(
              choice.index ??
                (arr(frame.choices).length === 1
                  ? 0
                  : !stream
                    ? ordinal
                    : undefined),
            );
          if (stream && choice.index === undefined) implicitChunkGroup = true;
          if (seen.has(group)) invalid();
          seen.add(group);
          groups.add(group);
          if (choice.delta && choice.message) invalid();
          const delta = choice.delta != null,
            message = obj(choice.delta ?? choice.message);
          const tools = [...arr(message.tool_calls)];
          const legacy = !!message.function_call;
          if (legacy) {
            if (tools.length) invalid();
            tools.push({
              index: 0,
              type: "function",
              function: message.function_call,
            });
          }
          if (tools.length) {
            if (legacyGroups.has(group) && legacyGroups.get(group) !== legacy)
              invalid();
            legacyGroups.set(group, legacy);
          }
          const positions = new Set<number>();
          tools.forEach((value, index) => {
            const tool = obj(value),
              position = slot(tool.index ?? (!delta ? index : undefined));
            if (positions.has(position)) invalid();
            positions.add(position);
            const key = `choice:${group}:tool:${position}`;
            const kind =
              str(tool.type) ||
              (tool.custom
                ? "custom"
                : tool.function
                  ? "function"
                  : (calls.get(key)?.kind ?? ""));
            if (
              !["function", "custom"].includes(kind) ||
              (tool.function && tool.custom) ||
              (kind === "custom" && tool.function) ||
              (kind === "function" && tool.custom)
            )
              invalid();
            const body = obj(tool[kind]);
            const call = add(key, group, position, body.name, kind, tool.id);
            const text = kind === "custom" ? body.input : body.arguments;
            if (text !== undefined) {
              if (delta) append(call, text);
              else snapshot(call, text);
            }
          });
          if (choice.finish_reason) {
            if (
              !["stop", "tool_calls", "function_call"].includes(
                str(choice.finish_reason),
              )
            )
              invalid();
            completed.add(group);
          }
        });
      } else if (record.input_protocol === "anthropic.messages") {
        const block = (value: unknown, position: number, start: boolean) => {
          if (completed.size || blocks.has(position)) invalid();
          blocks.set(position, !start);
          const item = obj(value);
          if (item.type === "tool_use") {
            const call = add(
              `anthropic:${position}`,
              0,
              position,
              item.name,
              "function",
              item.id,
            );
            const text = objectArguments(item.input);
            if (!start || text !== "{}") snapshot(call, text);
          } else if (!anthropicOther.has(str(item.type))) invalid();
        };
        if (type === "content_block_start")
          block(frame.content_block, slot(frame.index), true);
        else if (
          type === "content_block_delta" ||
          type === "content_block_stop"
        ) {
          const position = slot(frame.index),
            call = calls.get(`anthropic:${position}`);
          if (!blocks.has(position) || blocks.get(position) || completed.size)
            invalid();
          if (type === "content_block_stop") {
            blocks.set(position, true);
            if (call) {
              if (call.text === undefined) snapshot(call, "{}");
              call.done = true;
            }
          } else if (call) {
            if (obj(frame.delta).type !== "input_json_delta") invalid();
            append(call, obj(frame.delta).partial_json);
          }
        } else if (type === "message_start") {
          if (
            arr(obj(frame.message).content).some(
              (item) => obj(item).type === "tool_use",
            )
          )
            invalid();
        } else if (!stream && type === "message")
          arr(frame.content).forEach((item, index) =>
            block(item, index, false),
          );
        else if (!["ping", "message_delta", "message_stop"].includes(type))
          invalid();
        const reason = frame.stop_reason ?? obj(frame.delta).stop_reason;
        if (
          reason &&
          !["tool_use", "end_turn", "stop_sequence"].includes(str(reason))
        )
          invalid();
        if (type === "message_stop" || (!stream && reason)) {
          if ([...blocks.values()].some((closed) => !closed)) invalid();
          completed.add(0);
        }
      } else if (record.input_protocol === "google.generate_content") {
        if (obj(frame.promptFeedback).blockReason) invalid();
        const seen = new Set<number>();
        arr(frame.candidates).forEach((value, ordinal) => {
          const candidate = obj(value),
            group = slot(
              candidate.index ??
                (arr(frame.candidates).length === 1
                  ? 0
                  : !stream
                    ? ordinal
                    : undefined),
            );
          if ((stream || frames.length > 1) && candidate.index === undefined)
            implicitChunkGroup = true;
          if (seen.has(group)) invalid();
          seen.add(group);
          groups.add(group);
          const functions = arr(obj(candidate.content).parts).flatMap(
            (value) => {
              const part = obj(value);
              if (
                part.toolCall !== undefined ||
                part.customToolCall !== undefined
              )
                invalid();
              return part.functionCall ? [obj(part.functionCall)] : [];
            },
          );
          const final = candidate.finishReason === "STOP";
          if (
            (candidate.finishReason && !final) ||
            (completed.has(group) && !final)
          )
            invalid();
          const priorFinal = geminiFinals.get(group);
          if (priorFinal && priorFinal.length !== functions.length) invalid();
          const existing = [...calls.values()].filter(
            (call) => call.group === group,
          );
          // Without IDs a nonempty terminal after earlier chunks might be a
          // cumulative snapshot or more calls. Never guess by name/arguments.
          if (
            !priorFinal &&
            final &&
            existing.length &&
            functions.some((fn) => !str(fn.id))
          )
            invalid();
          const eventKeys: string[] = [];
          functions.forEach((fn, i) => {
            if (
              fn.partialArgs !== undefined ||
              fn.willContinue === true ||
              !fn.args ||
              Array.isArray(fn.args) ||
              typeof fn.args !== "object"
            )
              invalid();
            const id = str(fn.id);
            const knownKey = priorFinal?.[i] ?? (id ? ids.get(id) : undefined);
            if (priorFinal && id && ids.get(id) !== knownKey) invalid();
            const known = calls.get(knownKey ?? "");
            if (known && known.group !== group) invalid();
            const position =
              known?.slot ??
              [...calls.values()].filter((call) => call.group === group).length;
            const call = add(
              `gemini:${group}:call:${position}`,
              group,
              position,
              fn.name,
              "function",
              id,
            );
            snapshot(call, objectArguments(fn.args));
            if (eventKeys.includes(call.key)) invalid();
            eventKeys.push(call.key);
            const occurrence = geminiOccurrences.get(group) ?? 0;
            geminiOccurrences.set(group, occurrence + 1);
            aliases.set(`gemini:${group}:tool:${occurrence}`, call.key);
          });
          if (final) {
            completed.add(group);
            geminiFinals.set(group, eventKeys);
          }
        });
      } else return fail("unreliable");
    }
    const ordered = [...calls.values()].sort(
      (a, b) => a.group - b.group || a.slot - b.slot,
    );
    const reviews = [...summary.calls].sort((a, b) => a.index - b.index);
    if (
      (implicitChunkGroup && groups.size > 1) ||
      [...groups].sort((a, b) => a - b).some((group, index) => group !== index)
    )
      return fail("unreliable");
    for (const call of ordered) {
      if (call.kind !== "custom") objectArguments(JSON.parse(call.text ?? ""));
    }
    if (
      ordered.length !== reviews.length ||
      [...groups].some((group) => !completed.has(group)) ||
      !completed.size ||
      ordered.some(
        (call, index) =>
          !call.name ||
          call.text === undefined ||
          reviews[index]?.index !== index ||
          reviews[index]?.name !== call.name,
      )
    )
      return fail("unreliable");
    const indexes = new Map(ordered.map((call, index) => [call.key, index]));
    const displayed = new Set<string>();
    const located: ResponseOutput[] = [];
    for (const output of outputs) {
      const key = aliases.get(output.sourceKey ?? "") ?? output.sourceKey ?? "";
      const call = calls.get(key),
        reviewIndex = indexes.get(key);
      if (!call || reviewIndex === undefined) {
        located.push(output);
        continue;
      }
      if (
        output.kind !== "tool" ||
        output.name !== call.name ||
        !sameArgs(output.text, call.text!, call.kind)
      )
        return fail("unreliable");
      // Gemini's ordinary preview intentionally pairs results by name/order.
      // Its wire IDs prove occurrence aliases here, without changing that UI.
      const expectedID = aliases.has(output.sourceKey ?? "")
        ? ""
        : responseCallIDs.get(key) || call.id;
      if ((output.id ?? "") !== expectedID) return fail("unreliable");
      if (displayed.has(key)) {
        // Only Gemini wire occurrences with proven snapshot identity collapse.
        if (!aliases.has(output.sourceKey ?? "")) return fail("unreliable");
        continue;
      }
      displayed.add(key);
      located.push({ ...output, reviewIndex });
    }
    if (displayed.size !== ordered.length) return fail("unreliable");
    return { outputs: located };
  } catch {
    return fail("unreliable");
  }
}
