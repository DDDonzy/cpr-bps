mod adapter;
mod fallback;
mod warmup;
mod worker;
use gateway_plugin_sdk::call::{
    middleware::MiddlewareTransport,
    upstream_adapter::{
        BuiltinProvider, UpstreamAdapterDeclaration, UpstreamAdapterRegistration, UpstreamPath,
        UpstreamPathPurpose, UpstreamTransport,
    },
};
use gateway_plugin_sdk::client::{
    PluginBuilder, PluginSession, RequestCall, SessionConfig, TypedReply, methods,
};
use gateway_plugin_sdk::{ErrorCode, Manifest, PluginFault};
use serde::Deserialize;
use serde_json::Value;
use std::sync::Arc;

#[derive(Clone, Deserialize)]
#[serde(rename_all = "camelCase", deny_unknown_fields)]
struct Config {
    models: Vec<String>,
    timeout_ms: u64,
    on_failure: String,
}
#[tokio::main]
async fn main() -> Result<(), Box<dyn std::error::Error>> {
    let session = PluginSession::accept(
        tokio::io::stdin(),
        tokio::io::stdout(),
        SessionConfig {
            maximum_calls: 16,
            maximum_callbacks: 16,
            maximum_buffered_stream_chunks: 8,
            maximum_stream_chunk_bytes: 16 * 1024 * 1024,
            ..SessionConfig::default()
        },
    )
    .await?;
    let manifest = Manifest::from_author_slice(include_bytes!("../plugin.json"))?;
    let handshake = session.handshake();
    if handshake.plugin_id != manifest.plugin_id()? || handshake.contributes != manifest.contributes
    {
        return Err("plugin manifest mismatch".into());
    };
    let config: Config = serde_json::from_value(handshake.configuration.clone())?;
    if config.models.is_empty()
        || config.models.iter().any(|s| s.trim() != s || s.is_empty())
        || config.timeout_ms < 60_000
        || !matches!(config.on_failure.as_str(), "reject" | "native_fallback")
    {
        return Err("invalid BPS settings".into());
    }
    let worker = worker::Worker::start().await?;
    let registry = Arc::new(fallback::Registry::default());
    let adapter_registry = registry.clone();
    let warmups = Arc::new(warmup::Store::default());
    let models = config.models.clone();
    let registration = UpstreamAdapterRegistration {
        adapters: vec![UpstreamAdapterDeclaration {
            id: "basis-points".into(),
            provider: BuiltinProvider::OpenAi,
            base_url: "https://bps.openai.com/basispoints/api/".into(),
            paths: vec![
                UpstreamPath {
                    path: "responses".into(),
                    purpose: UpstreamPathPurpose::Inference,
                },
                UpstreamPath {
                    path: "attachments".into(),
                    purpose: UpstreamPathPurpose::Auxiliary,
                },
            ],
            authentication_kinds: vec!["oauth".into()],
            transport: UpstreamTransport::HttpSse,
            protocol: "openai".into(),
            models,
        }],
    };
    let plugin = PluginBuilder::from_json(include_bytes!("../plugin.json"))?
        .on(methods::UPSTREAM_ADAPTER_REGISTER, move |_| {
            let r = registration.clone();
            async move { Ok(TypedReply::new(r)) }
        })?
        .on(methods::UPSTREAM_ADAPTER_EXECUTE, move |call| {
            let w = Arc::clone(&worker);
            let reg = adapter_registry.clone();
            async move { adapter::execute(call, w, reg).await }
        })?
        .middleware(move |mut call: RequestCall| {
            let config = config.clone();
            let registry = registry.clone();
            let warmups=warmups.clone();
            async move {
                let selected = call.request.head.operation == "generate"
                    && call.request.head.protocol == "openai"
                    && call
                        .request
                        .head
                        .model
                        .as_ref()
                        .is_some_and(|m| config.models.contains(m));
                if !selected {
                    return call.next.run(call.request).await;
                }
                let settings = call.request.head.settings.as_object_mut().ok_or_else(|| {
                    PluginFault::new(ErrorCode::Unsupported, "request settings unavailable")
                })?;
                settings.insert("timeout_ms".into(), config.timeout_ms.into());
                let body =
                    serde_json::from_slice::<Value>(&call.request.body).unwrap_or(Value::Null);
                if call.request.head.transport==MiddlewareTransport::WebSocket {
                    let conversation=body["prompt_cache_key"].as_str().map(str::to_owned).or_else(||call.request.head.headers.iter().find(|h|h.name.eq_ignore_ascii_case("session-id")||h.name.eq_ignore_ascii_case("session_id")).and_then(|h|String::from_utf8(h.value.clone()).ok())).unwrap_or_default();
                    match warmups.prepare(&call.request.head.client_key_id,call.request.head.model.as_deref().unwrap_or_default(),&conversation,&call.request.head.request_id,&body)? {
                        warmup::Action::Ready(response)=>return Ok(*response),
                        warmup::Action::Rewrite(value)=>call.request.replace_body(serde_json::to_vec(&value).map_err(|_|PluginFault::new(ErrorCode::InvalidInput,"warmup body encoding"))?),
                        warmup::Action::Pass=>{},
                    }
                }
                let body=serde_json::from_slice::<Value>(&call.request.body).unwrap_or(Value::Null);
                let full = fallback::complete_history(&body);
                let guard = fallback::Guard::new(registry, call.request.head.request_id.clone());
                let key = call.request.head.client_key_id.clone();
                let model = call.request.head.model.clone().unwrap_or_default();
                let websocket = call.request.head.transport == MiddlewareTransport::WebSocket;
                let streaming =
                    websocket || call.request.head.transport == MiddlewareTransport::HttpSse;
                let host = call.host;
                let own_id = call.context.instance_id;
                let cancellation = call.cancellation;

                // Known BPS-incompatible tiers are detected before creating an attempt.
                // The original payload is handed to CPR; this plugin never sends it twice.
                let bps_tier_unsupported=body["service_tier"].as_str().is_some_and(|v|!matches!(v,"auto"|"default"));
                if config.on_failure=="native_fallback" && full && bps_tier_unsupported && !cancellation.is_cancelled() {
                    match fallback::isolated_stack_checked(&host,&own_id).await {
                        Ok(true)=>{},
                        Ok(false)=>return fallback::error_response(PluginFault::new(ErrorCode::Rejected,"Other active plugins prevent safe replay"),"stack_guard",websocket),
                        Err(error)=>return fallback::error_response(error,"stack_check",websocket),
                    }
                    return match fallback::native(host,key,model,None,body,streaming,websocket).await {
                        Ok(response)=>Ok(response),
                        Err(error)=>fallback::error_response(error,"preflight_native_start",websocket),
                    };
                }
                let result = call.next.run(call.request).await;
                let failed = result.as_ref().map_or(true, |r| r.status >= 400);
                if failed {
                    let fields=serde_json::json!({"attempted":guard.slot.attempted.load(std::sync::atomic::Ordering::SeqCst),"network_started":guard.slot.network.load(std::sync::atomic::Ordering::SeqCst),"confirmed":guard.slot.confirmed.load(std::sync::atomic::Ordering::SeqCst),"full_history":full,"cancelled":cancellation.is_cancelled(),"next_ok":result.is_ok(),"next_status":result.as_ref().map_or(0,|r|r.status)});
                    let _=host.call("host.log",serde_json::json!({"event":"bps_fallback_guard","level":"info","fields":fields}),vec![]).await;
                }

                if !failed
                    || config.on_failure != "native_fallback"
                    || !full
                    || cancellation.is_cancelled()
                    || !guard.slot.safe()
                {
                    return result;
                }
                if !fallback::isolated_stack(&host, &own_id).await {
                    return result;
                }
                if let Ok(mut response) = result {
                    response.body.close().await?;
                }
                let account = guard
                    .slot
                    .account
                    .lock()
                    .unwrap_or_else(std::sync::PoisonError::into_inner)
                    .clone();
                match fallback::native(host, key, model, account, body, streaming, websocket).await {
                    Ok(response) => Ok(response),
                    Err(error) if !streaming => {
                        let details=error.details.as_ref();
                        let code=details.and_then(|v|v.get("code")).filter(|v|!v.is_null()).cloned().unwrap_or_else(||serde_json::to_value(error.code).unwrap_or(Value::Null));
                        let value=serde_json::json!({"error":{"type":"server_error","code":code,"message":error.message},"bps_stage":"native_fallback_start"});
                        Ok(gateway_plugin_sdk::client::MiddlewareResponse::direct("openai",error.http_status.unwrap_or(502),vec![],gateway_plugin_sdk::client::MiddlewareBody::from_frames(gateway_plugin_sdk::call::middleware::MiddlewareBodyFraming::JsonDocument,vec![gateway_plugin_sdk::call::middleware::MiddlewareBodyFrame::new(serde_json::to_vec(&value).map_err(|_|PluginFault::new(ErrorCode::Fault,"fallback error encoding"))?,true)])))
                    },
                    Err(error)=>Err(error),
                }
            }
        })?
        .build()?;
    session.run(plugin).await?;
    Ok(())
}
