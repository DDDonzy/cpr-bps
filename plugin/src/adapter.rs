use crate::fallback::{Registry, Safety};
use crate::worker::Worker;
use base64::{Engine as _, engine::general_purpose::STANDARD as B64};
use gateway_plugin_sdk::call::{
    host::{AuthGetRequest, AuthRuntimeAccount},
    model::{CanonicalEvent, ExecutionEvent, WireEvent, WirePayload},
    upstream_adapter::{
        ContinuationScope, UpstreamAdapterEvent, UpstreamAdapterRequest, UpstreamContinuation,
        UpstreamFailure, UpstreamFailureKind, UpstreamHttpRequest,
    },
};
use gateway_plugin_sdk::client::{
    Empty, HostClient, ResponseStream, StreamSender, TypedCall, TypedReply,
};
use gateway_plugin_sdk::{ErrorCode, PluginFault};
use serde_json::{Value, json};
use std::sync::atomic::Ordering;
use std::{num::NonZeroUsize, sync::Arc};

fn fail(message: impl Into<String>) -> PluginFault {
    PluginFault::new(ErrorCode::Fault, message)
}
fn decode(value: &Value, key: &str) -> Result<Vec<u8>, PluginFault> {
    B64.decode(
        value[key]
            .as_str()
            .ok_or_else(|| fail("converter omitted payload"))?,
    )
    .map_err(|_| fail("invalid converter payload"))
}
fn headers(content: &str, account: Option<&str>) -> Vec<(String, String)> {
    let mut h = vec![
        ("content-type".into(), content.into()),
        ("accept".into(), "text/event-stream".into()),
        ("accept-encoding".into(), "identity".into()),
        ("origin".into(), "https://bps.openai.com".into()),
        ("x-basispoints-auth-mode".into(), "chatgpt".into()),
        (
            "user-agent".into(),
            concat!("cpr-bps-managed-plugin/", env!("CARGO_PKG_VERSION")).into(),
        ),
    ];
    if let Some(account) = account {
        h.push(("x-openai-account-id".into(), account.into()));
    }
    for (k, v) in [
        ("Client-Agent-Profile", "excel"),
        ("Client-Editor", "excel"),
        ("Client-Host", "office"),
        ("Client-Platform", "excel"),
        ("Client-Platform-Class", "PC"),
        ("Client-Product", "basispoints-excel-plugin"),
        ("Client-Runtime", "desktop"),
        ("Office-Host", "Excel"),
        ("Office-Platform", "PC"),
    ] {
        h.push((format!("x-openai-internal-basispoints-{k}"), v.into()));
    }
    h
}
async fn runtime_account(
    host: &HostClient,
    request: &UpstreamAdapterRequest,
) -> Result<AuthRuntimeAccount, PluginFault> {
    let payload = serde_json::to_vec(&AuthGetRequest {
        account_id: request.account_id.clone(),
    })
    .map_err(|_| fail("account metadata encoding"))?;
    let reply = host
        .call("host.auth.get_runtime", json!({}), payload)
        .await
        .map_err(|_| fail("selected account metadata unavailable"))?;
    let account: AuthRuntimeAccount = serde_json::from_slice(&reply.payload)
        .map_err(|_| fail("invalid selected account metadata"))?;
    if account.account_id != request.account_id
        || account.provider_id != "openai"
        || account.credential_revision != request.credential_revision
    {
        return Err(PluginFault::new(
            ErrorCode::Conflict,
            "selected account credential revision changed",
        ));
    };
    Ok(account)
}
fn stream_wire(raw: Vec<u8>) -> Result<WirePayload, PluginFault> {
    let text = std::str::from_utf8(&raw).map_err(|_| fail("non-UTF8 SSE frame"))?;
    let mut event = None;
    let mut data = vec![];
    for line in text.lines() {
        if let Some(v) = line.strip_prefix("event:") {
            event = Some(v.trim_start().to_owned());
        }
        if let Some(v) = line.strip_prefix("data:") {
            data.push(v.strip_prefix(' ').unwrap_or(v));
        }
    }
    if data.is_empty() {
        return Ok(WirePayload::RawSse { frame: raw });
    }
    let value =
        serde_json::from_str(&data.join("\n")).map_err(|_| fail("invalid converted SSE JSON"))?;
    Ok(WirePayload::Json {
        event,
        data: value,
        id: None,
        retry: None,
        raw_sse: Some(raw),
    })
}
async fn send(sender: &StreamSender, event: UpstreamAdapterEvent) -> Result<(), PluginFault> {
    let encoded = event
        .encode()
        .map_err(|_| fail("adapter event encoding failed"))?;
    sender
        .send(encoded)
        .await
        .map_err(|_| PluginFault::new(ErrorCode::Cancelled, "downstream closed"))
}
fn kind(status: u16, code: Option<&str>) -> UpstreamFailureKind {
    if matches!(code, Some("insufficient_quota" | "quota_exceeded")) {
        return UpstreamFailureKind::QuotaExhausted;
    }
    match status {
        400 | 413 | 422 => UpstreamFailureKind::InvalidRequest,
        401 => UpstreamFailureKind::Unauthorized,
        403 => UpstreamFailureKind::PermissionDenied,
        408 | 504 => UpstreamFailureKind::Timeout,
        429 => UpstreamFailureKind::RateLimited,
        _ => UpstreamFailureKind::Unavailable,
    }
}
async fn http_failure(sender: &StreamSender, status: u16, raw: Vec<u8>) -> Result<(), PluginFault> {
    let value = serde_json::from_slice::<Value>(&raw).ok();
    let error = value.as_ref().map(|v| v.get("error").unwrap_or(v));
    let code = error.and_then(|v| v["code"].as_str()).map(str::to_owned);
    let message = error
        .and_then(|v| v["message"].as_str())
        .map(str::to_owned)
        .unwrap_or_else(|| format!("BPS returned HTTP {status}"));
    let wire = if value.is_some() {
        WirePayload::RawJson { body: raw }
    } else {
        WirePayload::RawBody { body: raw }
    };
    let mut e = UpstreamAdapterEvent::new(ExecutionEvent::wire(WireEvent {
        protocol: "openai".into(),
        payload: wire,
    }));
    e.failure = Some(UpstreamFailure {
        kind: kind(status, code.as_deref()),
        status: Some(status),
        retry_after_ms: None,
        message,
        code,
    });
    send(sender, e).await
}
fn conversation(request: &UpstreamAdapterRequest, body: &Value) -> String {
    if let Some(v) = body["prompt_cache_key"].as_str() {
        return v.to_owned();
    }
    for (name, value) in &request.headers {
        if matches!(
            name.to_ascii_lowercase().as_str(),
            "session-id" | "session_id" | "x-codex-session-id"
        ) && let Ok(value) = std::str::from_utf8(value)
        {
            return value.to_owned();
        }
    }
    "anonymous".into()
}
async fn perform(
    call: TypedCall<UpstreamAdapterRequest>,
    sender: StreamSender,
    worker: Arc<Worker>,
    safety: Option<Arc<Safety>>,
) -> Result<(), PluginFault> {
    let mut body: Value = serde_json::from_slice(&call.payload)
        .map_err(|_| PluginFault::new(ErrorCode::InvalidInput, "invalid Responses JSON"))?;
    let conversation = conversation(&call.request, &body);
    let mut scope = serde_json::to_string(&json!([
        call.request.client_key_id,
        call.request.account_id,
        call.request.credential_revision,
        conversation,
        body["model"]
    ]))
    .map_err(|_| fail("scope encoding"))?;
    if let Some(cont) = &call.request.continuation {
        if let Some(s) = cont.state.get("history_scope").and_then(Value::as_str) {
            scope = s.to_owned();
        }
        body["previous_response_id"] = Value::String(cont.upstream_response_id.clone());
    }
    let account = runtime_account(&call.host, &call.request).await?;
    let mut job = worker.open(scope.clone(), body).await?;
    let prepared = loop {
        let v = job
            .receiver
            .recv()
            .await
            .ok_or_else(|| fail("converter stopped during preparation"))?;
        match v["type"].as_str() {
            Some("upload") => {
                let data = decode(&v, "body")?;
                let content = v["content_type"]
                    .as_str()
                    .ok_or_else(|| fail("attachment content type missing"))?;
                if let Some(s) = &safety {
                    s.begin_auxiliary();
                }
                let response = call
                    .host
                    .upstream_http(
                        UpstreamHttpRequest {
                            method: "POST".into(),
                            path: "attachments".into(),
                            headers: headers(content, account.upstream_account_id.as_deref()),
                            query: vec![],
                        },
                        data,
                    )
                    .await?;
                let raw = response.body.collect(16 * 1024 * 1024).await?;
                if !(200..300).contains(&response.status) {
                    http_failure(&sender, response.status, raw).await?;
                    return Ok(());
                }
                let result: Value = serde_json::from_slice(&raw)
                    .map_err(|_| fail("invalid attachment response"))?;
                let id = result["openai_file_id"]
                    .as_str()
                    .ok_or_else(|| fail("attachment file id missing"))?;
                worker
                    .send(json!({"op":"uploaded","id":job.id,"file_id":id}))
                    .await?;
            }
            Some("prepared") => break decode(&v, "body")?,
            Some("error") => {
                if let Some(s) = &safety
                    && !s.network.load(Ordering::SeqCst)
                {
                    s.confirmed.store(true, Ordering::SeqCst);
                }
                local_error(&sender, &v).await?;
                return Ok(());
            }
            _ => return Err(fail("unexpected converter preparation event")),
        }
    };
    if let Some(s) = &safety {
        s.begin_inference();
    }
    if let Some(id) = call.context.request_id.as_deref() {
        crate::route_log::record(&call.host, id, "basis_points", "inference_dispatched", None)
            .await;
    }
    let response = call
        .host
        .upstream_http(
            UpstreamHttpRequest {
                method: "POST".into(),
                path: "responses".into(),
                headers: headers("application/json", account.upstream_account_id.as_deref()),
                query: vec![],
            },
            prepared,
        )
        .await?;
    if !(200..300).contains(&response.status) {
        let status = response.status;
        if let Some(s) = &safety {
            s.confirm_rejection(status);
        }
        let raw = response.body.collect(16 * 1024 * 1024).await?;
        if let Some(id) = call.context.request_id.as_deref() {
            crate::route_log::record(
                &call.host,
                id,
                "bps_error",
                &format!("upstream_http_{status}"),
                None,
            )
            .await;
        }
        http_failure(&sender, status, raw).await?;
        return Ok(());
    }
    let pump_worker = worker.clone();
    let job_id = job.id.clone();
    let mut pump = tokio::spawn(async move {
        let mut body = response.body;
        loop {
            match body.read().await? {
                Some(bytes) => {
                    pump_worker
                        .send(json!({"op":"data","id":job_id,"data":B64.encode(bytes)}))
                        .await?
                }
                None => {
                    pump_worker.send(json!({"op":"eof","id":job_id})).await?;
                    break;
                }
            }
        }
        Ok::<(), PluginFault>(())
    });
    let _abort = Abort(pump.abort_handle());
    let mut pump_done = false;
    let mut terminal = false;
    let mut failed = false;
    loop {
        tokio::select! {
         result=&mut pump,if !pump_done=>{pump_done=true;match result{Ok(Ok(()))=>{},Ok(Err(e))=>return Err(e),Err(_)=>return Err(fail("upstream read task stopped"))}},
         v=job.receiver.recv()=>{let v=v.ok_or_else(||fail("converter stopped before terminal"))?;match v["type"].as_str(){
          Some("frame")=>{
           let facts:Vec<CanonicalEvent>=serde_json::from_value(v["facts"].clone()).map_err(|_|fail("invalid converter event facts"))?;let is_terminal=v["terminal"]==true;
           // Emit Responses events for both client modes; CPR aggregates JSON itself.
           let wire=Some(WireEvent{protocol:"openai".into(),payload:stream_wire(decode(&v,"wire")?)?});
           let mut event=UpstreamAdapterEvent::new(ExecutionEvent{facts,wire,host:None});
           if let Some(value)=v.get("failure"){event.failure=Some(serde_json::from_value(value.clone()).map_err(|_|fail("invalid converter failure"))?);failed=true;}
           if is_terminal {terminal=true;event.service_tier=v["service_tier"].as_str().map(str::to_owned);if let Some(id)=v["response"]["id"].as_str(){event.continuation=Some(UpstreamContinuation{scope:ContinuationScope::Persisted,upstream_response_id:id.to_owned(),state:json!({"history_scope":scope}).as_object().cloned().unwrap_or_default()});}}
           if let Some(s) = &safety { s.note_output(); }
           send(&sender,event).await?;
          },
          Some("error")=>{if !failed{local_error(&sender,&v).await?;failed=true;}},
          Some("end")=>{if !terminal && !failed{return Err(fail("BPS stream ended without a terminal event"))};if failed && let Some(id)=call.context.request_id.as_deref(){crate::route_log::record(&call.host,id,"bps_error","stream_failed",None).await;} return Ok(())},
          _=>return Err(fail("unexpected converter stream event"))
         }}
        }
    }
}
async fn local_error(sender: &StreamSender, v: &Value) -> Result<(), PluginFault> {
    let status = v["status"].as_u64().unwrap_or(502) as u16;
    let raw = serde_json::to_vec(
        &json!({"error":{"type":"bps_conversion_error","code":v["code"],"message":v["message"]}}),
    )
    .map_err(|_| fail("error encoding"))?;
    http_failure(sender, status, raw).await
}
struct Abort(tokio::task::AbortHandle);
impl Drop for Abort {
    fn drop(&mut self) {
        self.0.abort();
    }
}
pub async fn execute(
    call: TypedCall<UpstreamAdapterRequest>,
    worker: Arc<Worker>,
    registry: Arc<Registry>,
    config: crate::config::Config,
) -> Result<TypedReply<Empty>, PluginFault> {
    let (sender, stream) = ResponseStream::channel(NonZeroUsize::new(4).expect("nonzero"));
    let safety = call
        .context
        .request_id
        .as_deref()
        .and_then(|id| registry.get(id));
    if let Some(s) = &safety {
        s.attempted.store(true, Ordering::SeqCst);
        *s.account
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner) =
            Some(call.request.account_id.clone());
    }
    if !config.enabled || !config.allows_account(&call.request.account_id) {
        if let Some(s) = &safety {
            s.confirmed.store(true, Ordering::SeqCst);
        }
        // 尚未发出任何上游请求，交由宿主换号或由请求中间件安全回落原生。
        return Err(PluginFault::new(
            ErrorCode::Rejected,
            "当前账号不在 BPS 账号范围内",
        ));
    }
    let log_host = call.host.clone();
    let log_id = call.context.request_id.clone();
    let cancellation = call.cancellation.clone();
    tokio::spawn(async move {
        let sink = sender.clone();
        let result = tokio::select! {_=cancellation.cancelled()=>Err(PluginFault::new(ErrorCode::Cancelled,"request cancelled")),r=perform(call,sink,worker,safety.clone())=>r};
        if let Err(error) = result {
            if let Some(id) = log_id.as_deref() {
                crate::route_log::record(
                    &log_host,
                    id,
                    "bps_error",
                    if cancellation.is_cancelled() {
                        "cancelled"
                    } else {
                        "adapter_failed"
                    },
                    None,
                )
                .await;
            }
            if !cancellation.is_cancelled()
                && let Some(s) = &safety
                && !s.network.load(Ordering::SeqCst)
            {
                s.confirmed.store(true, Ordering::SeqCst);
            }
            let _ = sender.fail(error).await;
        }
    });
    Ok(TypedReply::new(Empty {}).with_stream(stream))
}
