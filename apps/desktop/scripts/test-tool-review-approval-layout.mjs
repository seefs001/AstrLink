// Isolated Chrome + mocked native IPC only. Never starts or contacts Core.
// Run with a standalone desktop-edition Rsbuild server and Chrome installed:
// ASK_PREVIEW_URL=http://127.0.0.1:1491 bun scripts/test-tool-review-approval-layout.mjs
import assert from "node:assert/strict";
import { spawn } from "node:child_process";
import { mkdtemp, readFile, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
const origin = process.env.ASK_PREVIEW_URL || "http://127.0.0.1:1491";
assert.match(origin, /^http:\/\/(?:127\.0\.0\.1|localhost):\d+$/);
const directory = await mkdtemp(join(tmpdir(), "astrlink-ask-layout-"));
const chrome = spawn(
  process.env.CHROME_EXECUTABLE ||
    "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
  [
    "--headless=new",
    "--no-first-run",
    "--disable-background-networking",
    "--remote-debugging-port=0",
    `--user-data-dir=${directory}`,
    "about:blank",
  ],
  { stdio: "ignore" },
);
let socket;
try {
  let port;
  for (let attempt = 0; attempt < 100; attempt++) {
    try {
      port = Number(
        (await readFile(join(directory, "DevToolsActivePort"), "utf8")).split(
          "\n",
        )[0],
      );
      break;
    } catch {
      await new Promise((resolve) => setTimeout(resolve, 100));
    }
  }
  assert.ok(port, "Chrome failed to expose its isolated debugger");
  const target = await (
    await fetch(`http://127.0.0.1:${port}/json/new?about:blank`, {
      method: "PUT",
    })
  ).json();
  socket = new WebSocket(target.webSocketDebuggerUrl);
  await new Promise((resolve, reject) => {
    socket.onopen = resolve;
    socket.onerror = reject;
  });
  let sequence = 0;
  const pending = new Map();
  const errors = [];
  socket.onmessage = (event) => {
    const message = JSON.parse(String(event.data));
    if (message.method === "Runtime.exceptionThrown")
      errors.push(
        message.params.exceptionDetails.exception?.description ||
          message.params.exceptionDetails.text,
      );
    if (!message.id) return;
    const callback = pending.get(message.id);
    pending.delete(message.id);
    if (message.error) callback.reject(new Error(message.error.message));
    else callback.resolve(message.result);
  };
  const command = (method, params = {}) =>
    new Promise((resolve, reject) => {
      const id = ++sequence;
      pending.set(id, { resolve, reject });
      socket.send(JSON.stringify({ id, method, params }));
    });
  const evaluate = async (expression) => {
    const result = await command("Runtime.evaluate", {
      expression,
      returnByValue: true,
      awaitPromise: true,
    });
    if (result.exceptionDetails) throw new Error(result.exceptionDetails.text);
    return result.result.value;
  };
  await command("Page.enable");
  await command("Runtime.enable");
  const fixtureSource = `
    window.__ASTRLINK_DESKTOP_PLATFORM__="macos";
    window.__TAURI_EVENT_PLUGIN_INTERNALS__={unregisterListener:()=>{}};
    window.fixtureMode="ready";window.fixtureDecisions=[];window.fixtureDetailReads=0;
    const requested_at=new Date(Date.now()-1000).toISOString(),expires_at=new Date(Date.now()+120000).toISOString();
    const approval={id:"approval_fixture",state:"pending",review_status:"flagged",requested_at,expires_at};
    const summary={status:"pending",mode:"ask",model:"jev",config_revision:"fixture",started_at:requested_at,latency_ms:10,calls:[{index:0,name:"run",verdict:"flag",action:"pending",model:"jev",latency_ms:10,flag_probability:0.9,reason_code:"destructive_action"}],approval};
    const raw={raw_available:true,configured:true,password_set:true,password_required:false,envelopes:["password"],key_verified:true,unlocked:true,unlock_expires_at:null,unlock_idle_seconds:300,retry_after_seconds:0,password_min_length:8,password_max_length:128,key_replaced:false};
    window.__TAURI_INTERNALS__={metadata:{currentWindow:{label:"tool-review-approval"},currentWebview:{label:"tool-review-approval"}},transformCallback:()=>1,unregisterCallback:()=>{},invoke:async(command,args)=>{
      if(command.startsWith("plugin:event|"))return 1;
      if(command==="plugin:window|is_focused")return false;
      if(command.startsWith("plugin:window|"))return false;
      if(command==="list_tool_review_approvals"){
        if(window.fixtureMode==="offline")throw "tool_review_approval_unavailable";
        return {items:window.fixtureMode==="empty"?[]:[{id:approval.id,request_id:"req_fixture",requested_at,expires_at,summary},{id:"approval_second",request_id:"req_second",requested_at,expires_at,summary:{...summary,approval:{...approval,id:"approval_second"}}}]};
      }
      if(command==="raw_sealing_status")return {...raw,unlocked:window.fixtureMode!=="locked"};
      if(command==="get_tool_review_approval"){window.fixtureDetailReads++;return {id:args.approvalId,request_id:args.approvalId==="approval_second"?"req_second":"req_fixture",user_request:"Synthetic fixture user request",calls:[{index:0,name:"run",arguments:"Synthetic long fixture parameter ".repeat(300)}]};}
      if(command==="decide_tool_review_approval"){window.fixtureDecisions.push(args);return {...approval,state:args.decision==="allow"?"approved":"denied",decided_at:new Date().toISOString()};}
      throw "fixture_command_unavailable";
    }};
    // No API or arbitrary external request can touch the running gateway.
    const originalFetch=window.fetch.bind(window);
    window.fetch=(url,options)=>{const target=new URL(String(url),location.href);if(target.origin!==location.origin||/^\\/(?:control|console)\\//.test(target.pathname))throw new Error("unexpected fixture network request");return originalFetch(url,options);};
  `;
  // Catch malformed injection before starting the UI rather than timing out.
  new Function(fixtureSource);
  await command("Page.addScriptToEvaluateOnNewDocument", {
    source: fixtureSource,
  });
  await command("Page.navigate", { url: origin });
  for (let attempt = 0; attempt < 100; attempt++) {
    if (
      await evaluate(
        '!!document.querySelector("[data-testid=tool-approval-plaintext]")',
      )
    )
      break;
    await new Promise((resolve) => setTimeout(resolve, 100));
  }
  assert.ok(
    await evaluate(
      '!!document.querySelector("[data-testid=tool-approval-plaintext]")',
    ),
    "Approval surface did not load: " +
      JSON.stringify({
        errors,
        state: await evaluate(
          "({text:document.body.innerText,reads:window.fixtureDetailReads,native:!!window.__TAURI_INTERNALS__,platform:window.__ASTRLINK_DESKTOP_PLATFORM__})",
        ),
      }),
  );
  const dimensions = [];
  for (const [width, height] of [
    [1280, 720],
    [1024, 600],
    [380, 600],
    [540, 660],
    [380, 460],
  ]) {
    await command("Emulation.setDeviceMetricsOverride", {
      width,
      height,
      deviceScaleFactor: 1,
      mobile: false,
    });
    await evaluate(
      "new Promise(resolve=>requestAnimationFrame(()=>requestAnimationFrame(resolve)))",
    );
    const facts = await evaluate(
      `(()=>{const primary=document.querySelector('[data-testid="tool-review-approval-primary"]');const chrome=parseFloat(getComputedStyle(document.documentElement).getPropertyValue('--window-chrome-height'));return {primary:primary.getBoundingClientRect().height,usable:innerHeight-chrome,chrome,overflow:document.documentElement.scrollWidth>innerWidth,allowFocused:document.activeElement?.dataset.decision==='allow'};})()`,
    );
    assert.equal(facts.chrome, 32, "Native macOS title-bar spacing missing");
    assert.ok(
      facts.primary / facts.usable >= 0.6,
      `Insufficient working height at ${width}x${height}`,
    );
    assert.equal(facts.overflow, false);
    assert.equal(facts.allowFocused, false);
    dimensions.push({
      width,
      height,
      ...facts,
      ratio: Number((facts.primary / facts.usable).toFixed(3)),
    });
  }
  await evaluate(
    `document.querySelector('details summary').click();document.querySelector('[data-testid="tool-review-approval-primary"]').scrollTop=10000;`,
  );
  assert.equal(await evaluate("document.documentElement.scrollTop"), 0);
  assert.equal(await evaluate('document.querySelector("details").open'), true);
  await evaluate(
    `Array.from(document.querySelectorAll('nav button')).find(button=>button.textContent==="req_second").click()`,
  );
  await evaluate(
    "new Promise(resolve=>requestAnimationFrame(()=>requestAnimationFrame(resolve)))",
  );
  assert.equal(await evaluate("document.documentElement.scrollTop"), 0);
  await evaluate('window.fixtureMode="locked"');
  await new Promise((resolve) => setTimeout(resolve, 2300));
  assert.equal(
    await evaluate(
      '!!document.querySelector("[data-testid=tool-approval-plaintext]")',
    ),
    false,
  );
  assert.equal(
    await evaluate('document.querySelector("[data-decision=allow]").disabled'),
    true,
  );
  await evaluate('window.fixtureMode="offline"');
  await new Promise((resolve) => setTimeout(resolve, 2300));
  assert.equal(
    await evaluate('!!document.querySelector("[data-decision=allow]")'),
    false,
  );
  await evaluate('window.fixtureMode="empty"');
  await new Promise((resolve) => setTimeout(resolve, 2300));
  assert.equal(
    await evaluate('!!document.querySelector("[data-decision=deny]")'),
    false,
  );
  assert.deepEqual(await evaluate("window.fixtureDecisions"), []);
  assert.deepEqual(errors, []);
  console.log(
    JSON.stringify(
      {
        dimensions,
        locked: true,
        offline: true,
        empty: true,
        automaticDecisions: 0,
      },
      null,
      2,
    ),
  );
} finally {
  socket?.close();
  const done = new Promise((resolve) => chrome.once("exit", resolve));
  chrome.kill("SIGTERM");
  await done;
  await rm(directory, { recursive: true, force: true });
}
