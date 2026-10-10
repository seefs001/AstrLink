//! Native shell for the operator's live tool-review queue. Only metadata IDs
//! enter the watcher; plaintext detail stays in the requesting IPC response.
//! Closing this window never decides a request.

use std::{
    collections::{BTreeMap, BTreeSet},
    future::Future,
    sync::{Arc, Mutex},
    time::Duration,
};

use reqwest::StatusCode;
use serde::{Deserialize, Serialize};
use tauri::{AppHandle, Emitter, Manager, WebviewUrl, WebviewWindowBuilder};
use tokio::sync::watch;

use crate::{
    i18n::Locale,
    preferences::PreferencesStore,
    sidecar::{CoreManager, CorePhase, CoreView},
};

pub const LABEL: &str = "tool-review-approval";
pub const STATE_EVENT: &str = "tool-review-approval:state";
pub const PATH: &str = "/control/v1/tool-review/approvals";
pub const UNAVAILABLE: &str = "tool_review_approval_unavailable";
pub const INVALID: &str = "tool_review_approval_invalid";
const POLL_INTERVAL: Duration = Duration::from_secs(2);
const MAX_OPEN_ATTEMPTS: u8 = 3;
pub const MAX_PENDING_APPROVALS: usize = 16;

pub fn validate_id(id: &str) -> Result<(), String> {
    if id.is_empty()
        || id.len() > 128
        || !id
            .bytes()
            .all(|byte| byte.is_ascii_alphanumeric() || matches!(byte, b'_' | b'-'))
    {
        return Err(INVALID.into());
    }
    Ok(())
}

pub fn validate_decision(decision: &str) -> Result<(), String> {
    if matches!(decision, "allow" | "deny") {
        Ok(())
    } else {
        Err(INVALID.into())
    }
}

pub fn status_result(status: StatusCode) -> Result<(), String> {
    if status.is_success() {
        return Ok(());
    }
    Err(match status {
        StatusCode::LOCKED => "raw_locked",
        StatusCode::CONFLICT | StatusCode::GONE | StatusCode::NOT_FOUND => {
            "tool_review_approval_not_pending"
        }
        StatusCode::UNAUTHORIZED | StatusCode::FORBIDDEN => "tool_review_approval_forbidden",
        StatusCode::BAD_REQUEST | StatusCode::UNPROCESSABLE_ENTITY => INVALID,
        _ => UNAVAILABLE,
    }
    .into())
}

// Explicit projections prevent a future additive raw field on Core's queue
// response from becoming visible through the metadata command or watcher.
#[derive(Deserialize, Serialize)]
pub struct PendingList {
    pub items: Vec<PendingApproval>,
}

#[derive(Deserialize, Serialize)]
pub struct PendingApproval {
    pub id: String,
    request_id: String,
    requested_at: String,
    expires_at: String,
    summary: Summary,
}

#[derive(Deserialize, Serialize)]
struct Summary {
    #[serde(
        default,
        deserialize_with = "crate::tool_review::deserialize_optional_backend",
        skip_serializing_if = "Option::is_none"
    )]
    backend: Option<crate::tool_review::Backend>,
    status: String,
    mode: String,
    model: String,
    config_revision: String,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    batch_calls: Option<bool>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    reject_threshold: Option<f64>,
    started_at: String,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    completed_at: Option<String>,
    latency_ms: u64,
    calls: Vec<ReviewCall>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    error_code: Option<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    approval: Option<Approval>,
}

#[derive(Deserialize, Serialize)]
struct ReviewCall {
    index: u64,
    name: String,
    verdict: String,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    model_verdict: Option<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    reason_code: Option<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pass_probability: Option<f64>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    flag_probability: Option<f64>,
    action: String,
    latency_ms: u64,
    model: String,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    error_code: Option<String>,
}

#[derive(Deserialize, Serialize)]
pub struct Approval {
    pub id: String,
    state: String,
    review_status: String,
    requested_at: String,
    expires_at: String,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    decided_at: Option<String>,
}

#[derive(Deserialize, Serialize)]
pub struct Detail {
    pub id: String,
    request_id: String,
    user_request: String,
    calls: Vec<DetailCall>,
}

#[derive(Deserialize, Serialize)]
struct DetailCall {
    index: u64,
    name: String,
    arguments: String,
}

#[derive(Clone, Copy, PartialEq, Eq, Serialize)]
#[serde(rename_all = "snake_case")]
enum QueueStatus {
    Loading,
    Ready,
    Offline,
}

#[derive(Default)]
struct Queue {
    current: BTreeSet<String>,
    opened: BTreeSet<String>,
    notified: BTreeSet<String>,
    attempts: BTreeMap<String, u8>,
}

impl Queue {
    fn pending(&mut self, ids: impl IntoIterator<Item = String>) -> (Vec<String>, bool) {
        self.current = ids.into_iter().collect();
        // A successful snapshot is authoritative. Keep closed-window dedup
        // across offline periods, but never retain completed historical IDs.
        self.opened.retain(|id| self.current.contains(id));
        self.notified.retain(|id| self.current.contains(id));
        self.attempts.retain(|id, _| self.current.contains(id));
        let mut notify = false;
        let candidates = self
            .current
            .iter()
            .filter(|id| {
                if self.opened.contains(*id) {
                    return false;
                }
                if self.notified.insert((*id).clone()) {
                    notify = true;
                }
                self.attempts.get(*id).copied().unwrap_or(0) < MAX_OPEN_ATTEMPTS
            })
            .cloned()
            .collect();
        (candidates, notify)
    }

    fn opened(&mut self) {
        self.opened.extend(self.current.iter().cloned());
    }

    fn failed(&mut self, ids: &[String]) {
        for id in ids {
            if !self.current.contains(id) {
                continue;
            }
            let attempts = self.attempts.entry(id.clone()).or_default();
            *attempts = attempts.saturating_add(1);
        }
    }

    fn offline(&mut self) {
        self.current.clear();
        // Remember opened IDs and retry budgets across disconnect/restart.
    }
}

pub struct WindowState {
    queue: Mutex<Queue>,
    shutdown: watch::Sender<bool>,
}

impl Default for WindowState {
    fn default() -> Self {
        let (shutdown, _) = watch::channel(false);
        Self {
            queue: Mutex::new(Queue::default()),
            shutdown,
        }
    }
}

fn locale(app: &AppHandle) -> Locale {
    app.try_state::<Arc<PreferencesStore>>()
        .map(|store| store.snapshot().values.locale)
        .unwrap_or_default()
}

pub fn title(locale: Locale) -> &'static str {
    match locale {
        Locale::En => "Tool review approvals",
        Locale::ZhCN => "工具审查审批",
    }
}

/// Auto-open never steals focus, including when another pending request arrives
/// while the queue window is already minimized or hidden. Explicit open does.
pub fn open(app: &AppHandle, focus: bool) -> Result<(), String> {
    let result = (|| {
        if let Some(window) = app.get_webview_window(LABEL) {
            if focus {
                window.unminimize().map_err(|_| UNAVAILABLE)?;
                window.show().map_err(|_| UNAVAILABLE)?;
                window.set_focus().map_err(|_| UNAVAILABLE)?;
            }
            return Ok(());
        }
        let theme = app
            .try_state::<Arc<PreferencesStore>>()
            .and_then(|store| store.snapshot().values.theme.native_theme())
            .or_else(|| {
                app.get_webview_window("main")
                    .and_then(|window| window.theme().ok())
            })
            .unwrap_or(tauri::Theme::Light);
        let mut builder = WebviewWindowBuilder::new(app, LABEL, WebviewUrl::default())
            .title(title(locale(app)))
            .background_color(crate::theme_background(theme))
            .inner_size(540.0, 660.0)
            .min_inner_size(380.0, 460.0)
            .resizable(true)
            .shadow(true)
            .focused(focus)
            .always_on_top(true)
            .center();
        #[cfg(target_os = "macos")]
        {
            builder = builder
                .title_bar_style(tauri::TitleBarStyle::Overlay)
                .hidden_title(true);
        }
        #[cfg(not(target_os = "macos"))]
        {
            builder = builder.decorations(false);
        }
        builder.build().map_err(|_| UNAVAILABLE)?;
        Ok::<(), String>(())
    })();
    if result.is_ok() {
        if let Some(state) = app.try_state::<WindowState>() {
            if let Ok(mut queue) = state.queue.lock() {
                queue.opened();
            }
        }
    } else {
        eprintln!("tool_review_approval_open_failed");
    }
    result
}

fn notify_pending(app: &AppHandle) {
    let body = match locale(app) {
        Locale::En => {
            "A tool call is waiting for your decision. Open the approval queue to review it."
        }
        Locale::ZhCN => "有工具调用等待您的决定。请打开审批队列查看。",
    };
    #[cfg(target_os = "macos")]
    crate::macos_app::notify(title(locale(app)).into(), body.into());
    #[cfg(not(target_os = "macos"))]
    {
        use tauri_plugin_notification::NotificationExt;
        if app
            .notification()
            .builder()
            .title(title(locale(app)))
            .body(body)
            .show()
            .is_err()
        {
            eprintln!("tool_review_approval_notification_failed");
        }
    }
}

fn publish(app: &AppHandle, status: QueueStatus) {
    // No request identifiers, arguments or user text in cross-window events.
    let _ = app.emit(STATE_EVENT, serde_json::json!({"status": status}));
}

/// Cancels an in-flight request on Core stop or app shutdown, but not on other
/// CoreView metadata updates. Does not keep a strong CoreManager reference.
async fn while_ready<T>(
    changes: &mut watch::Receiver<CoreView>,
    shutdown: &mut watch::Receiver<bool>,
    request: impl Future<Output = T>,
) -> Option<T> {
    tokio::pin!(request);
    loop {
        if *shutdown.borrow() || changes.borrow().phase != CorePhase::Ready {
            return None;
        }
        tokio::select! {
            biased;
            result = shutdown.changed() => {
                if result.is_err() || *shutdown.borrow() { return None; }
            }
            result = changes.changed() => {
                if result.is_err() || changes.borrow().phase != CorePhase::Ready { return None; }
            }
            result = &mut request => return Some(result),
        }
    }
}

pub fn stop(app: &AppHandle) {
    if let Some(state) = app.try_state::<WindowState>() {
        state.shutdown.send_replace(true);
    }
}

pub fn start(app: &AppHandle) {
    let Some(manager) = app.try_state::<Arc<CoreManager>>() else {
        return;
    };
    let Some(state) = app.try_state::<WindowState>() else {
        return;
    };
    let mut changes = manager.subscribe();
    let manager = Arc::downgrade(manager.inner());
    let mut shutdown = state.shutdown.subscribe();
    let app = app.clone();
    // This AppHandle is released on the explicit exit signal, even though the
    // app still owns CoreManager (and therefore its watch sender).
    tauri::async_runtime::spawn(async move {
        let mut ticker =
            tokio::time::interval_at(tokio::time::Instant::now() + POLL_INTERVAL, POLL_INTERVAL);
        ticker.set_missed_tick_behavior(tokio::time::MissedTickBehavior::Skip);
        let mut core_ready = false;
        let mut status = QueueStatus::Offline;
        let mut poll_failed = false;
        loop {
            if *shutdown.borrow() {
                break;
            }
            tokio::select! {
                biased;
                result = shutdown.changed() => {
                    if result.is_err() || *shutdown.borrow() { break; }
                    continue;
                }
                result = changes.changed() => {
                    if result.is_err() { break; }
                    let ready = changes.borrow_and_update().phase == CorePhase::Ready;
                    if ready == core_ready { continue; }
                    core_ready = ready;
                }
                _ = ticker.tick() => {
                    core_ready = changes.borrow().phase == CorePhase::Ready;
                }
            }
            if core_ready {
                if status == QueueStatus::Offline {
                    status = QueueStatus::Loading;
                    publish(&app, status);
                }
                let poll = async {
                    let core = manager.upgrade().ok_or(UNAVAILABLE)?;
                    let pending = core.list_tool_review_approvals().await?;
                    Ok::<_, String>(
                        pending
                            .items
                            .into_iter()
                            .map(|item| item.id)
                            .collect::<Vec<_>>(),
                    )
                };
                if let Some(result) = while_ready(&mut changes, &mut shutdown, poll).await {
                    match result {
                        Ok(ids) => {
                            poll_failed = false;
                            status = QueueStatus::Ready;
                            if let Some(state) = app.try_state::<WindowState>() {
                                let next =
                                    state.queue.lock().ok().map(|mut queue| queue.pending(ids));
                                if let Some((candidates, notify)) = next {
                                    if !candidates.is_empty() && open(&app, false).is_err() {
                                        if let Ok(mut queue) = state.queue.lock() {
                                            queue.failed(&candidates);
                                        }
                                    }
                                    if notify {
                                        notify_pending(&app);
                                    }
                                }
                            }
                            publish(&app, status);
                        }
                        Err(_) => {
                            if !poll_failed {
                                eprintln!("tool_review_approval_poll_failed");
                            }
                            poll_failed = true;
                            status = QueueStatus::Offline;
                            if let Some(state) = app.try_state::<WindowState>() {
                                if let Ok(mut queue) = state.queue.lock() {
                                    queue.offline();
                                }
                            }
                            publish(&app, status);
                        }
                    }
                } else {
                    core_ready = false;
                    status = QueueStatus::Offline;
                    if let Some(state) = app.try_state::<WindowState>() {
                        if let Ok(mut queue) = state.queue.lock() {
                            queue.offline();
                        }
                    }
                    publish(&app, status);
                }
            } else if status != QueueStatus::Offline {
                status = QueueStatus::Offline;
                if let Some(state) = app.try_state::<WindowState>() {
                    if let Ok(mut queue) = state.queue.lock() {
                        queue.offline();
                    }
                }
                publish(&app, status);
            }
            // Ordinary CoreView updates must not bypass the polling/retry
            // cadence. Only a transition back to Ready triggers an early poll.
            ticker.reset();
        }
        publish(&app, QueueStatus::Offline);
    });
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn ids_and_decisions_are_strict_path_safe_inputs() {
        for id in ["a", "Approval_0-9", &"a".repeat(128)] {
            assert!(validate_id(id).is_ok());
        }
        for id in [
            "",
            "../a",
            "a/b",
            "a?b",
            "a#b",
            "a%b",
            " a",
            "a\n",
            "审批",
            &"a".repeat(129),
        ] {
            assert_eq!(validate_id(id).unwrap_err(), INVALID);
        }
        for decision in ["allow", "deny"] {
            assert!(validate_decision(decision).is_ok());
        }
        for decision in ["", "approve", "ALLOW", "allow ", "deny\n"] {
            assert_eq!(validate_decision(decision).unwrap_err(), INVALID);
        }
    }

    #[test]
    fn status_mapping_never_uses_error_bodies() {
        for (status, error) in [
            (423, "raw_locked"),
            (409, "tool_review_approval_not_pending"),
            (410, "tool_review_approval_not_pending"),
            (404, "tool_review_approval_not_pending"),
            (401, "tool_review_approval_forbidden"),
            (403, "tool_review_approval_forbidden"),
            (400, INVALID),
            (422, INVALID),
            (500, UNAVAILABLE),
            (503, UNAVAILABLE),
        ] {
            assert_eq!(
                status_result(StatusCode::from_u16(status).unwrap()).unwrap_err(),
                error
            );
        }
        assert!(status_result(StatusCode::OK).is_ok());
    }

    #[test]
    fn opened_ids_survive_close_disconnect_and_replayed_metadata() {
        let mut queue = Queue::default();
        assert_eq!(
            queue.pending(["a".into(), "a".into()]),
            (vec!["a".into()], true)
        );
        queue.opened();
        assert_eq!(queue.pending(["a".into()]), (vec![], false));
        queue.offline();
        assert_eq!(
            queue.pending(["a".into(), "b".into()]),
            (vec!["b".into()], true)
        );
        queue.opened();
        queue.pending([]);
        assert!(queue.opened.is_empty());
        assert!(queue.notified.is_empty());
        assert!(queue.attempts.is_empty());
        assert_eq!(queue.pending(["new".into()]), (vec!["new".into()], true));
    }

    #[test]
    fn successive_unique_snapshots_bound_all_history_to_current_capacity() {
        let mut queue = Queue::default();
        for round in 0..128 {
            let ids: Vec<String> = (0..MAX_PENDING_APPROVALS)
                .map(|index| format!("approval_{round}_{index}"))
                .collect();
            let (candidates, notify) = queue.pending(ids.clone());
            assert_eq!(candidates.len(), MAX_PENDING_APPROVALS);
            assert!(notify);
            queue.failed(&candidates);
            queue.opened();
            queue.offline();
            assert_eq!(queue.opened.len(), MAX_PENDING_APPROVALS);
            assert_eq!(queue.notified.len(), MAX_PENDING_APPROVALS);
            assert_eq!(queue.attempts.len(), MAX_PENDING_APPROVALS);
            assert_eq!(queue.pending(ids), (vec![], false));
            assert!(queue.opened.iter().all(|id| queue.current.contains(id)));
            assert!(queue.notified.iter().all(|id| queue.current.contains(id)));
            assert!(queue.attempts.keys().all(|id| queue.current.contains(id)));
        }
        queue.pending([]);
        queue.failed(&["retired".into()]);
        assert!(queue.current.is_empty());
        assert!(queue.opened.is_empty());
        assert!(queue.notified.is_empty());
        assert!(queue.attempts.is_empty());
    }

    #[test]
    fn failed_open_retries_are_bounded_and_notification_is_once() {
        let mut queue = Queue::default();
        for attempt in 0..MAX_OPEN_ATTEMPTS {
            let (ids, notify) = queue.pending(["a".into()]);
            assert_eq!(ids, ["a"]);
            assert_eq!(notify, attempt == 0);
            queue.failed(&ids);
        }
        assert_eq!(queue.pending(["a".into()]), (vec![], false));
        queue.offline();
        assert_eq!(queue.pending(["a".into()]), (vec![], false));
        // An explicit successful reopen remembers all current IDs, including
        // one whose automatic retry budget has already been exhausted.
        queue.opened();
        assert!(queue.opened.contains("a"));
        assert_eq!(
            queue.pending(["a".into(), "b".into()]),
            (vec!["b".into()], true)
        );
    }

    #[test]
    fn summary_projection_preserves_backend_without_defaulting_history_or_accepting_invalid_values()
    {
        let summary = serde_json::json!({"status":"pending","mode":"ask","model":"reviewer","config_revision":"r1","started_at":"now","latency_ms":1,"calls":[]});
        let parsed: Summary = serde_json::from_value(summary.clone()).unwrap();
        assert!(serde_json::to_value(parsed)
            .unwrap()
            .get("backend")
            .is_none());
        for backend in ["jev", "responses"] {
            let mut current = summary.clone();
            current["backend"] = serde_json::json!(backend);
            let parsed: Summary = serde_json::from_value(current).unwrap();
            assert_eq!(serde_json::to_value(parsed).unwrap()["backend"], backend);
        }
        for backend in [
            serde_json::json!(null),
            serde_json::json!(""),
            serde_json::json!("openai"),
            serde_json::json!(true),
        ] {
            let mut current = summary.clone();
            current["backend"] = backend;
            assert!(serde_json::from_value::<Summary>(current).is_err());
        }
    }

    #[test]
    fn summary_projection_preserves_batch_snapshots_without_defaulting_history() {
        let summary = serde_json::json!({"status":"pending","mode":"ask","model":"reviewer","config_revision":"r1","started_at":"now","latency_ms":1,"calls":[]});
        let parsed: Summary = serde_json::from_value(summary.clone()).unwrap();
        assert!(serde_json::to_value(parsed)
            .unwrap()
            .get("batch_calls")
            .is_none());
        for batch_calls in [false, true] {
            let mut current = summary.clone();
            current["batch_calls"] = serde_json::json!(batch_calls);
            let parsed: Summary = serde_json::from_value(current).unwrap();
            assert_eq!(
                serde_json::to_value(parsed).unwrap()["batch_calls"],
                batch_calls
            );
        }
        for value in [
            serde_json::json!(1),
            serde_json::json!("false"),
            serde_json::json!([]),
        ] {
            let mut current = summary.clone();
            current["batch_calls"] = value;
            assert!(serde_json::from_value::<Summary>(current).is_err());
        }
    }

    #[test]
    fn metadata_projection_drops_raw_and_future_fields() {
        let body = serde_json::json!({"items":[{
            "id":"approval_a", "request_id":"request_a", "requested_at":"now", "expires_at":"later",
            "user_request":"private text", "arguments":"private arguments",
            "summary":{"status":"pending", "mode":"ask", "model":"reviewer", "config_revision":"r1",
                "started_at":"now", "latency_ms":1, "user_request":"private text", "calls":[{
                    "index":0,"name":"shell","verdict":"flag","action":"pending","latency_ms":1,
                    "model":"reviewer","arguments":"private arguments"
                }]}
        }]});
        let parsed: PendingList = serde_json::from_value(body).unwrap();
        let encoded = serde_json::to_string(&parsed).unwrap();
        assert!(!encoded.contains("private"));
        assert!(!encoded.contains("arguments"));
        assert!(!encoded.contains("user_request"));
    }

    #[test]
    fn in_flight_poll_cancels_on_stop_shutdown_and_closed_channels() {
        let runtime = tokio::runtime::Builder::new_current_thread()
            .enable_all()
            .build()
            .unwrap();
        runtime.block_on(async {
            for shutdown_instead in [false, true] {
                let view = CoreView {
                    phase: CorePhase::Ready,
                    ..CoreView::default()
                };
                let (core, mut changes) = watch::channel(view);
                let (exit, mut shutdown) = watch::channel(false);
                let request = std::future::pending::<()>();
                let poll = while_ready(&mut changes, &mut shutdown, request);
                let stop = async {
                    tokio::task::yield_now().await;
                    if shutdown_instead {
                        exit.send_replace(true);
                    } else {
                        core.send_modify(|view| view.phase = CorePhase::Stopping);
                    }
                };
                let (result, ()) = tokio::time::timeout(Duration::from_secs(1), async {
                    tokio::join!(poll, stop)
                })
                .await
                .expect("poll must stop promptly");
                assert!(result.is_none());
            }
            let (core, mut changes) = watch::channel(CoreView {
                phase: CorePhase::Ready,
                ..CoreView::default()
            });
            let (_exit, mut shutdown) = watch::channel(false);
            drop(core);
            assert!(
                while_ready(&mut changes, &mut shutdown, std::future::pending::<()>())
                    .await
                    .is_none()
            );
        });
    }
}
