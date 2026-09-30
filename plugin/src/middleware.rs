use crate::{config::Config, fallback, management, warmup};
use gateway_plugin_sdk::call::middleware::{
    MiddlewareBodyDisposition, MiddlewareBodyFrame, MiddlewareTransport,
};
use gateway_plugin_sdk::client::{MiddlewareBody, MiddlewareResponse, RequestCall};
use gateway_plugin_sdk::{ErrorCode, PluginFault};
use serde_json::Value;
use std::sync::Arc;

pub async fn handle(
    mut call: RequestCall,
    config: Config,
    registry: Arc<fallback::Registry>,
    warmups: Arc<warmup::Store>,
) -> Result<MiddlewareResponse, PluginFault> {
    let selected = call.request.head.operation == "generate"
        && call.request.head.protocol == "openai"
        && call
            .request
            .head
            .model
            .as_ref()
            .is_some_and(|m| config.applies_to_model(m));
    if !selected {
        return call.next.run(call.request).await;
    }
    let settings =
        call.request.head.settings.as_object_mut().ok_or_else(|| {
            PluginFault::new(ErrorCode::Unsupported, "request settings unavailable")
        })?;
    settings.insert("timeout_ms".into(), config.timeout_ms.into());
    let body = serde_json::from_slice::<Value>(&call.request.body).unwrap_or(Value::Null);
    if call.request.head.transport == MiddlewareTransport::WebSocket {
        let conversation = body["prompt_cache_key"]
            .as_str()
            .map(str::to_owned)
            .or_else(|| {
                call.request
                    .head
                    .headers
                    .iter()
                    .find(|h| {
                        h.name.eq_ignore_ascii_case("session-id")
                            || h.name.eq_ignore_ascii_case("session_id")
                    })
                    .and_then(|h| String::from_utf8(h.value.clone()).ok())
            })
            .unwrap_or_default();
        match warmups.prepare(
            &call.request.head.client_key_id,
            call.request.head.model.as_deref().unwrap_or_default(),
            &conversation,
            &call.request.head.request_id,
            &body,
        )? {
            warmup::Action::Ready(response) => return Ok(*response),
            warmup::Action::Rewrite(value) => {
                call.request
                    .replace_body(serde_json::to_vec(&value).map_err(|_| {
                        PluginFault::new(ErrorCode::InvalidInput, "warmup body encoding")
                    })?)
            }
            warmup::Action::Pass => {}
        }
    }
    let body = serde_json::from_slice::<Value>(&call.request.body).unwrap_or(Value::Null);
    let full = fallback::complete_history(&body);
    let request_id = call.request.head.request_id.clone();
    let guard = fallback::Guard::new(registry, request_id.clone());
    let key = call.request.head.client_key_id.clone();
    let model = call.request.head.model.clone().unwrap_or_default();
    let websocket = call.request.head.transport == MiddlewareTransport::WebSocket;
    let streaming = websocket || call.request.head.transport == MiddlewareTransport::HttpSse;
    let host = call.host;
    let own_id = call.context.instance_id;
    let cancellation = call.cancellation;

    // Known BPS-incompatible tiers are detected before creating an attempt.
    // The original payload is handed to CPR; this plugin never sends it twice.
    let bps_tier_unsupported = body["service_tier"]
        .as_str()
        .is_some_and(|v| !matches!(v, "auto" | "default"));
    // 没有可用 BPS 账号时，在调用 next 之前就切换原生通道：
    // 此时尚未创建 attempt，重放原始正文是安全的，也不会落在下游重放限制上。
    let bps_viable = if full && !cancellation.is_cancelled() {
        management::accounts_cached(&host)
            .await
            .map(|list| {
                list.iter()
                    .any(|a| a.enabled && config.allows_account(&a.account_id))
            })
            .unwrap_or(true)
    } else {
        true
    };
    if full && !cancellation.is_cancelled() && (bps_tier_unsupported || !bps_viable) {
        // 尚未创建 attempt，把原始正文交给 CPR 原生通道是安全的。
        // 若当前环境不适合重放，则继续走 BPS，而不是向客户端返回拒绝。
        if matches!(
            fallback::isolated_stack_checked(&host, &own_id).await,
            Ok(true)
        ) && let Ok(response) = fallback::native(
            host.clone(),
            key.clone(),
            model.clone(),
            None,
            body.clone(),
            streaming,
            websocket,
        )
        .await
        {
            return Ok(response);
        }
    }
    let mut result = call.next.run(call.request).await;
    // 下游是冷流：读取首帧后才能知道账号/上游是否在发送前失败。
    // 首帧尚未交付客户端；转发时保留 source_id，避免重建用量或终态事实。
    let first = if let Ok(response) = &mut result {
        if response.body.framing().is_some() {
            Some(response.body.read().await)
        } else {
            None
        }
    } else {
        None
    };
    let failed = result.as_ref().map_or(true, |r| r.status >= 400)
        || matches!(&first, Some(Err(_)))
        || matches!(&first, Some(Ok(Some(frame))) if is_error_frame(frame));
    if failed {
        let fields = serde_json::json!({"attempted":guard.slot.attempted.load(std::sync::atomic::Ordering::SeqCst),"network_started":guard.slot.network.load(std::sync::atomic::Ordering::SeqCst),"confirmed":guard.slot.confirmed.load(std::sync::atomic::Ordering::SeqCst),"full_history":full,"cancelled":cancellation.is_cancelled(),"next_ok":result.is_ok(),"next_status":result.as_ref().map_or(0,|r|r.status)});
        let _ = host
            .call(
                "host.log",
                serde_json::json!({"event":"bps_fallback_guard","level":"info","fields":fields}),
                vec![],
            )
            .await;
    }

    if !failed || !full || cancellation.is_cancelled() || !guard.slot.safe() {
        return forward(result, first, guard).await;
    }
    let stack = fallback::isolated_stack_checked(&host, &own_id).await;
    if !matches!(stack, Ok(true)) {
        return forward(result, first, guard).await;
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
    let native = fallback::native(host, key, model, account, body, streaming, websocket).await;
    match native {
        Ok(response) => Ok(response),
        Err(error) if !streaming => {
            let details = error.details.as_ref();
            let code = details
                .and_then(|v| v.get("code"))
                .filter(|v| !v.is_null())
                .cloned()
                .unwrap_or_else(|| serde_json::to_value(error.code).unwrap_or(Value::Null));
            let value = serde_json::json!({"error":{"type":"server_error","code":code,"message":error.message},"bps_stage":"native_fallback_start"});
            Ok(gateway_plugin_sdk::client::MiddlewareResponse::direct(
                "openai",
                error.http_status.unwrap_or(502),
                vec![],
                gateway_plugin_sdk::client::MiddlewareBody::from_frames(
                    gateway_plugin_sdk::call::middleware::MiddlewareBodyFraming::JsonDocument,
                    vec![
                        gateway_plugin_sdk::call::middleware::MiddlewareBodyFrame::new(
                            serde_json::to_vec(&value).map_err(|_| {
                                PluginFault::new(ErrorCode::Fault, "fallback error encoding")
                            })?,
                            true,
                        ),
                    ],
                ),
            ))
        }
        Err(error) => Err(error),
    }
}

fn is_error_frame(frame: &MiddlewareBodyFrame) -> bool {
    let Ok(text) = std::str::from_utf8(&frame.payload) else {
        return false;
    };
    let is_error = |value: Value| {
        value["error"].is_object()
            || matches!(value["type"].as_str(), Some("error" | "response.failed"))
            || value["status"] == "failed"
    };
    if let Ok(value) = serde_json::from_str::<Value>(text) {
        return is_error(value);
    }
    text.lines()
        .filter_map(|line| line.strip_prefix("data:"))
        .filter_map(|data| serde_json::from_str::<Value>(data.trim()).ok())
        .any(is_error)
}

async fn forward(
    result: Result<MiddlewareResponse, PluginFault>,
    first: Option<Result<Option<MiddlewareBodyFrame>, PluginFault>>,
    guard: fallback::Guard,
) -> Result<MiddlewareResponse, PluginFault> {
    let mut response = result?;
    let Some(first) = first else {
        return Ok(response);
    };
    let first = match first {
        Ok(frame) => frame,
        Err(error) => {
            let _ = response.body.close().await;
            return Err(error);
        }
    };
    let framing = response
        .body
        .framing()
        .ok_or_else(|| PluginFault::new(ErrorCode::Fault, "下游正文格式丢失"))?;
    let (sender, output) =
        MiddlewareBody::channel(framing, std::num::NonZeroUsize::new(4).expect("nonzero"));
    let mut input = std::mem::replace(&mut response.body, output);
    tokio::spawn(async move {
        let _guard = guard;
        let mut cached = Some(first);
        let mut completed = false;
        loop {
            let next = if let Some(frame) = cached.take() {
                Ok(frame)
            } else {
                input.read().await
            };
            match next {
                Ok(Some(mut frame)) => {
                    if frame.source_id() == 0
                        || frame.disposition() != MiddlewareBodyDisposition::Only
                    {
                        let _ = sender
                            .fail(PluginFault::new(ErrorCode::Fault, "下游帧来源无效"))
                            .await;
                        break;
                    }
                    // 保留 SDK 签发的 source_id/disposition；结束由宿主源帧派生。
                    frame.terminal = false;
                    if sender.send(frame).await.is_err() {
                        break;
                    }
                }
                Ok(None) => {
                    completed = true;
                    break;
                }
                Err(error) => {
                    let _ = sender.fail(error).await;
                    break;
                }
            }
        }
        // 正常结束仍需由宿主 commit_downstream 提交原执行；提前 close 会取消它。
        if !completed {
            let _ = input.close().await;
        }
    });
    Ok(response)
}

#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn distinguishes_transport_failure_from_assistant_content() {
        let failure =
            MiddlewareBodyFrame::new(br#"{"error":{"message":"unavailable"}}"#.to_vec(), true);
        assert!(is_error_frame(&failure));
        let response = MiddlewareBodyFrame::new(
            br#"{"status":"completed","output":[{"content":[{"text":"error"}]}]}"#.to_vec(),
            true,
        );
        assert!(!is_error_frame(&response));
    }
}
