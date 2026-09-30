use gateway_plugin_sdk::call::{
    host::{
        ModelEventBatch, ModelExecuteRequest, ModelOperation, ModelStreamReadResult,
        ModelStreamResult,
    },
    middleware::{MiddlewareBodyFrame, MiddlewareBodyFraming, MiddlewareHeader, http::Version},
    model::WirePayload,
};
use gateway_plugin_sdk::client::{
    HostClient, HttpBody, HttpFrame, HttpRequest, MiddlewareBody, MiddlewareBodySender,
    MiddlewareResponse, SessionError,
};
use gateway_plugin_sdk::{ErrorCode, PluginFault};
use serde_json::{Value, json};
use std::{
    collections::{BTreeMap, HashMap, HashSet},
    num::NonZeroUsize,
    sync::{
        Arc, Mutex,
        atomic::{AtomicBool, AtomicUsize, Ordering},
    },
};

#[derive(Default)]
pub struct Safety {
    pub attempted: AtomicBool,
    pub network: AtomicBool,
    pub confirmed: AtomicBool,
    inference_pending: AtomicUsize,
    rejected_inference: AtomicUsize,
    auxiliary_started: AtomicBool,
    output_delivered: AtomicBool,
    pub account: Mutex<Option<String>>,
}
impl Safety {
    pub fn safe(&self) -> bool {
        self.attempted.load(Ordering::SeqCst)
            && self.confirmed.load(Ordering::SeqCst)
            && self.inference_pending.load(Ordering::SeqCst) == 0
            && !self.auxiliary_started.load(Ordering::SeqCst)
            && !self.output_delivered.load(Ordering::SeqCst)
            && (!self.network.load(Ordering::SeqCst)
                || self.rejected_inference.load(Ordering::SeqCst) > 0)
    }

    pub fn begin_inference(&self) {
        self.network.store(true, Ordering::SeqCst);
        self.inference_pending.fetch_add(1, Ordering::SeqCst);
    }

    pub fn begin_auxiliary(&self) {
        self.network.store(true, Ordering::SeqCst);
        self.auxiliary_started.store(true, Ordering::SeqCst);
    }

    pub fn note_output(&self) {
        self.output_delivered.store(true, Ordering::SeqCst);
    }

    /// 只有明确拒收、且未交付任何响应或产生附件副作用时才允许回落。
    /// 超时、断链和 5xx 不能证明上游没有开始执行。
    pub fn confirm_rejection(&self, status: u16) {
        if matches!(status, 400 | 401 | 403 | 404 | 405 | 413 | 415 | 422 | 429)
            && self
                .inference_pending
                .fetch_update(Ordering::SeqCst, Ordering::SeqCst, |v| v.checked_sub(1))
                .is_ok()
        {
            self.rejected_inference.fetch_add(1, Ordering::SeqCst);
            self.confirmed.store(true, Ordering::SeqCst);
        }
    }
}
#[derive(Default)]
pub struct Registry {
    slots: Mutex<HashMap<String, Arc<Safety>>>,
}
impl Registry {
    pub fn insert(&self, id: &str) -> Arc<Safety> {
        let slot = Arc::new(Safety::default());
        self.slots
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner)
            .insert(id.to_owned(), slot.clone());
        slot
    }
    pub fn get(&self, id: &str) -> Option<Arc<Safety>> {
        self.slots
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner)
            .get(id)
            .cloned()
    }
    pub fn remove(&self, id: &str) {
        self.slots
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner)
            .remove(id);
    }
}
pub fn complete_history(body: &Value) -> bool {
    if !body["previous_response_id"].is_null() || !body["conversation"].is_null() {
        return false;
    }
    let Some(items) = body["input"].as_array() else {
        return body["input"].is_string();
    };
    let mut calls = HashSet::new();
    for i in items {
        match i["type"].as_str().unwrap_or("") {
            "item_reference" => return false,
            "function_call" | "custom_tool_call" | "tool_search_call" => {
                if let Some(id) = i["call_id"].as_str() {
                    calls.insert(id);
                }
            }
            "function_call_output" | "custom_tool_call_output" | "tool_search_output"
                if !i["call_id"].as_str().is_some_and(|id| calls.contains(id)) =>
            {
                return false;
            }
            _ => {}
        }
    }
    true
}
fn fault(message: &str) -> PluginFault {
    PluginFault::new(ErrorCode::Fault, message)
}
fn remote(error: SessionError) -> PluginFault {
    match error {
        SessionError::Remote(f) => f,
        _ => fault("native fallback callback unavailable"),
    }
}
// Do not re-run unrelated mutating plugins when initiating a fallback child.
pub async fn isolated_stack_checked(host: &HostClient, own_id: &str) -> Result<bool, PluginFault> {
    let request = HttpRequest {
        settings: Value::Null,
        method: "GET".into(),
        uri: "/api/admin/plugins/instances".into(),
        version: Version::Http11,
        headers: vec![],
        timeout_ms: Some(10000),
        body: HttpBody::empty(),
    };
    let mut response = host.dispatch_http(request).await?;
    if response.status != 200 {
        return Err(fault(&format!("stack check HTTP {}", response.status)));
    }
    let mut bytes = vec![];
    while let Some(frame) = response.body.read().await? {
        if let HttpFrame::Data(data) = frame {
            if bytes.len() + data.len() > 1024 * 1024 {
                return Err(fault("stack check response too large"));
            };
            bytes.extend(data);
        }
    }
    let v: Value = serde_json::from_slice(&bytes).map_err(|_| fault("stack check invalid JSON"))?;
    // 官方管理 API 在不同版本中返回 data 数组或 { instances: [...] }，两者都接受。
    let data = v.get("data").unwrap_or(&v);
    let items = data
        .as_array()
        .or_else(|| data.get("instances").and_then(Value::as_array))
        .ok_or_else(|| fault("stack check expected instance list"))?;
    Ok(items.iter().all(|i| {
        i["enabled"] != true
            || i["id"] == own_id
            || i["bindings"].as_array().is_some_and(|bindings| {
                bindings.iter().all(|b| {
                    !matches!(
                        b["stage"].as_str(),
                        Some(
                            "http"
                                | "websocket"
                                | "service"
                                | "request"
                                | "attempt"
                                | "routing"
                                | "scheduling"
                                | "upstream"
                        )
                    )
                })
            })
    }))
}
async fn read(host: &HostClient, id: &str) -> Result<(ModelEventBatch, bool), PluginFault> {
    let reply = host
        .call(
            "host.model.stream_read",
            json!({"stream":id,"maximum_bytes":1024*1024}),
            vec![],
        )
        .await
        .map_err(remote)?;
    let state: ModelStreamReadResult =
        serde_json::from_value(reply.result).map_err(|_| fault("invalid native stream state"))?;
    let batch = if reply.payload.is_empty() {
        ModelEventBatch::default()
    } else {
        ModelEventBatch::decode(&reply.payload).map_err(|_| fault("invalid native model events"))?
    };
    Ok((batch, state.end))
}
async fn pump(
    host: HostClient,
    id: String,
    sender: MiddlewareBodySender,
    streaming: bool,
    websocket: bool,
) -> Result<(), PluginFault> {
    let mut terminal = None;
    let mut items = BTreeMap::new();
    let mut ended = false;
    let result = async {
        while !ended {
            let (batch, end) = read(&host, &id).await?;
            ended = end;
            for e in batch.events {
                let Some(wire) = e.wire else { continue };
                let (payload, data) = match wire.payload {
                    WirePayload::Json {
                        event,
                        data,
                        raw_sse,
                        ..
                    } => {
                        let kind = event.or_else(|| data["type"].as_str().map(str::to_owned));
                        let raw = raw_sse.unwrap_or_else(|| {
                            format!(
                                "event: {}\ndata: {}\n\n",
                                kind.as_deref().unwrap_or("message"),
                                data
                            )
                            .into_bytes()
                        });
                        (raw, Some(data))
                    }
                    WirePayload::RawSse { frame } => (frame, None),
                    WirePayload::RawJson { body } => {
                        let value = serde_json::from_slice::<Value>(&body)
                            .map_err(|_| fault("invalid native JSON"))?;
                        (body, Some(value))
                    }
                    WirePayload::RawBody { body } => (body, None),
                };
                let mut is_terminal = false;
                if let Some(data) = &data {
                    let kind = data["type"].as_str().unwrap_or("");
                    if kind == "response.output_item.done"
                        && let Some(index) = data["output_index"].as_u64()
                    {
                        items.insert(index, data["item"].clone());
                    }
                    if matches!(kind, "response.completed" | "response.incomplete") {
                        terminal = Some(data["response"].clone());
                        is_terminal = true;
                    }
                    if matches!(kind, "error" | "response.failed") {
                        let err = data.get("error").or_else(|| data["response"].get("error"));
                        let message = err
                            .and_then(|e| e["message"].as_str())
                            .unwrap_or("native upstream failed");
                        return Err(PluginFault::new(ErrorCode::Upstream, message));
                    }
                }
                if streaming {
                    let payload = if websocket {
                        if let Some(data) = data {
                            serde_json::to_vec(&data)
                                .map_err(|_| fault("native websocket encoding"))?
                        } else {
                            continue;
                        }
                    } else {
                        payload
                    };
                    sender
                        .send(MiddlewareBodyFrame::new(payload, is_terminal))
                        .await
                        .map_err(|_| {
                            PluginFault::new(ErrorCode::Cancelled, "fallback downstream closed")
                        })?;
                }
            }
        }
        if !streaming {
            let mut response =
                terminal.ok_or_else(|| fault("native stream omitted terminal response"))?;
            if response["output"].as_array().is_some_and(Vec::is_empty) && !items.is_empty() {
                response["output"] = Value::Array(items.into_values().collect());
            }
            sender
                .send(MiddlewareBodyFrame::new(
                    serde_json::to_vec(&response).map_err(|_| fault("native JSON encoding"))?,
                    true,
                ))
                .await
                .map_err(|_| {
                    PluginFault::new(ErrorCode::Cancelled, "fallback downstream closed")
                })?;
        }
        Ok(())
    }
    .await;
    if !ended {
        let _ = host
            .call("host.model.stream_close", json!({"stream":id}), vec![])
            .await;
    }
    result
}
pub struct NativeRequest<'a> {
    pub parent_id: &'a str,
    pub reason: &'a str,
    pub key: String,
    pub model: String,
    pub account: Option<String>,
    pub body: Value,
    pub streaming: bool,
    pub websocket: bool,
}
pub async fn native(
    host: HostClient,
    request: NativeRequest<'_>,
) -> Result<MiddlewareResponse, PluginFault> {
    let NativeRequest {
        parent_id,
        reason,
        key,
        model,
        account,
        mut body,
        streaming,
        websocket,
    } = request;
    body["stream"] = Value::Bool(true);
    let request = ModelExecuteRequest {
        client_key_id: Some(key),
        model,
        protocol: "openai".into(),
        operation: ModelOperation::Generate,
        provider: Some("openai".into()),
        account_id: account,
        previous_response_id: None,
    };
    let reply = host
        .call(
            "host.model.execute_stream",
            serde_json::to_value(request).map_err(|_| fault("native request encoding"))?,
            serde_json::to_vec(&body).map_err(|_| fault("native body encoding"))?,
        )
        .await
        .map_err(remote)?;
    let result: ModelStreamResult =
        serde_json::from_value(reply.result).map_err(|_| fault("invalid native stream handle"))?;
    crate::route_log::record(
        &host,
        parent_id,
        "native_fallback",
        reason,
        Some(&result.request_id),
    )
    .await;
    crate::route_log::record(
        &host,
        &result.request_id,
        "native_fallback",
        reason,
        Some(parent_id),
    )
    .await;
    let framing = if streaming && !websocket {
        MiddlewareBodyFraming::SseEvent
    } else {
        MiddlewareBodyFraming::JsonDocument
    };
    let (sender, out) = MiddlewareBody::channel(framing, NonZeroUsize::new(4).expect("nonzero"));
    tokio::spawn(async move {
        if let Err(e) = pump(host, result.stream, sender.clone(), streaming, websocket).await {
            let _ = sender.fail(e).await;
        }
    });
    Ok(MiddlewareResponse::direct(
        "openai",
        200,
        vec![
            MiddlewareHeader {
                name: "content-type".into(),
                value: if streaming && !websocket {
                    b"text/event-stream".to_vec()
                } else {
                    b"application/json".to_vec()
                },
            },
            MiddlewareHeader {
                name: "x-excel-bps-route".into(),
                value: b"native-fallback".to_vec(),
            },
            MiddlewareHeader {
                name: "x-excel-bps-native-request-id".into(),
                value: result.request_id.into_bytes(),
            },
        ],
        out,
    ))
}

#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn forbids_reference_only_history() {
        assert!(!complete_history(
            &json!({"previous_response_id":"resp_old","input":[]})
        ));
        assert!(!complete_history(
            &json!({"input":[{"type":"function_call_output","call_id":"c","output":"x"}]})
        ));
        assert!(complete_history(
            &json!({"input":[{"role":"user","content":"hello"}]})
        ));
    }
    #[test]
    fn any_network_activity_blocks_fallback() {
        let s = Safety::default();
        s.attempted.store(true, Ordering::SeqCst);
        s.confirmed.store(true, Ordering::SeqCst);
        assert!(s.safe());
        s.network.store(true, Ordering::SeqCst);
        assert!(!s.safe());
    }
    #[test]
    fn explicit_rejection_falls_back_but_uncertain_or_effectful_requests_do_not() {
        let state = Safety::default();
        state.attempted.store(true, Ordering::SeqCst);
        state.begin_inference();
        state.confirm_rejection(422);
        assert!(state.safe());
        state.begin_inference();
        state.confirm_rejection(502);
        assert!(!state.safe());
        let state = Safety::default();
        state.attempted.store(true, Ordering::SeqCst);
        state.begin_auxiliary();
        state.begin_inference();
        state.confirm_rejection(400);
        assert!(!state.safe());
        let state = Safety::default();
        state.attempted.store(true, Ordering::SeqCst);
        state.begin_inference();
        state.confirm_rejection(429);
        state.note_output();
        assert!(!state.safe());
    }
}

pub struct Guard {
    pub slot: Arc<Safety>,
    registry: Arc<Registry>,
    id: String,
}
impl Guard {
    pub fn new(registry: Arc<Registry>, id: String) -> Self {
        let slot = registry.insert(&id);
        Self { slot, registry, id }
    }
}
impl Drop for Guard {
    fn drop(&mut self) {
        self.registry.remove(&self.id);
    }
}
