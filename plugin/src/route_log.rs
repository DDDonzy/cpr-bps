//! 路由事实由实际执行点记录；仅装饰已通过宿主鉴权的日志响应，不改计费或请求路径。
use gateway_plugin_sdk::client::{HostClient, HttpBody, HttpCall, HttpFrame, HttpResponse};
use gateway_plugin_sdk::{ErrorCode, PluginFault};
use serde_json::{Value, json};
use std::collections::HashMap;

const NAMESPACE: &str = "bps.routes";
const BUCKET_SIZE: usize = 64;
fn bucket(id: &str) -> String {
    let hash = id.bytes().fold(2166136261u32, |hash, b| {
        (hash ^ u32::from(b)).wrapping_mul(16777619)
    });
    format!("{:02x}", hash % 128)
}
async fn read(host: &HostClient, key: &str) -> Result<Value, PluginFault> {
    host.call(
        "host.state.get",
        json!({"namespace":NAMESPACE,"key":key}),
        vec![],
    )
    .await
    .map(|v| v.result["record"].clone())
    .map_err(|_| PluginFault::new(ErrorCode::Fault, "BPS route log unavailable"))
}
/// 分桶有界保留，CAS 防止并发覆盖；失败只影响日志，不改变业务响应。
pub async fn record(
    host: &HostClient,
    id: &str,
    channel: &str,
    reason: &str,
    related: Option<&str>,
) {
    if id.is_empty() {
        return;
    }
    let key = bucket(id);
    for _ in 0..4 {
        let Ok(current) = read(host, &key).await else {
            break;
        };
        let mut entries = current["value"]["entries"]
            .as_array()
            .cloned()
            .unwrap_or_default();
        // 回落后的旧 BPS 尝试可能较晚结束，不能覆盖已确定的原生子请求关联。
        if channel != "native_fallback"
            && entries
                .iter()
                .any(|r| r["requestId"] == id && r["channel"] == "native_fallback")
        {
            return;
        }
        entries.retain(|r| r["requestId"] != id);
        entries.push(
            json!({"requestId":id,"channel":channel,"reason":reason,"relatedRequestId":related}),
        );
        if entries.len() > BUCKET_SIZE {
            entries.drain(..entries.len() - BUCKET_SIZE);
        }
        if host.call("host.state.put",json!({"namespace":NAMESPACE,"key":key,"value":{"entries":entries},"expected_version":current["version"]}),vec![]).await.is_ok() { return; }
    }
    let _ = host
        .call(
            "host.log",
            json!({"level":"warn","event":"bps_route_log_write_failed","fields":{"request_id":id}}),
            vec![],
        )
        .await;
}
fn annotate(row: &mut Value, fact: &Value) {
    let endpoint = match fact["channel"].as_str() {
        Some("basis_points" | "rejected" | "bps_error") => "/basispoints/api/responses",
        Some("native_fallback") => "/v1/responses",
        _ => return,
    };
    let Some(route) = row["route"].as_str().map(str::to_owned) else {
        return;
    };
    if row.get("bpsRoute").is_some() {
        return;
    }
    row["originalRoute"] = json!(route);
    row["route"] = json!(endpoint);
    row["bpsRoute"] = fact.clone();
    if row["clientTransport"].is_string() {
        row["originalClientTransport"] = row["clientTransport"].clone();
        row["clientTransport"] = json!(if fact["channel"] == "native_fallback" {
            "BPS回落"
        } else {
            "BPS"
        });
    }
}
fn target(method: &str, uri: &str) -> bool {
    method == "GET"
        && matches!(
            uri.split('?').next().unwrap_or(""),
            "/api/admin/usage/records"
                | "/api/admin/usage/records/detail"
                | "/api/admin/operations/errors"
        )
}
pub async fn handle(call: HttpCall) -> Result<HttpResponse, PluginFault> {
    let selected = target(&call.request.method, &call.request.uri);
    let mut response = call.next.run(call.request).await?;
    if !selected
        || response.status != 200
        || response
            .headers
            .iter()
            .any(|h| h.name.eq_ignore_ascii_case("content-encoding") && h.value != b"identity")
    {
        return Ok(response);
    }
    let mut bytes = Vec::new();
    while let Some(frame) = response.body.read().await? {
        if let HttpFrame::Data(data) = frame {
            bytes.extend(data);
        }
    }
    if let Ok(mut value) = serde_json::from_slice::<Value>(&bytes) {
        let mut empty = Value::Null;
        let data = value.get_mut("data").unwrap_or(&mut empty);
        let mut cache: HashMap<String, Value> = HashMap::new();
        let rows: Vec<&mut Value> =
            if let Some(items) = data.get_mut("items").and_then(Value::as_array_mut) {
                items.iter_mut().collect()
            } else {
                vec![data]
            };
        for row in rows {
            let id = row["requestId"]
                .as_str()
                .or_else(|| row["id"].as_str())
                .unwrap_or("")
                .to_owned();
            if id.is_empty() {
                continue;
            }
            let key = bucket(&id);
            if !cache.contains_key(&key) {
                cache.insert(
                    key.clone(),
                    read(&call.host, &key).await.unwrap_or(Value::Null),
                );
            }
            if let Some(fact) = cache[&key]["value"]["entries"]
                .as_array()
                .and_then(|list| list.iter().find(|v| v["requestId"] == id))
            {
                annotate(row, fact);
            }
        }
        bytes = serde_json::to_vec(&value).unwrap_or(bytes);
    }
    response.headers.retain(|h| {
        !matches!(
            h.name.to_ascii_lowercase().as_str(),
            "content-length" | "etag"
        )
    });
    response.body = HttpBody::from_bytes(bytes);
    Ok(response)
}
#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn only_decorates_authorized_log_shapes_and_preserves_facts() {
        assert!(target("GET", "/api/admin/usage/records?pageSize=20"));
        assert!(!target("POST", "/v1/responses"));
        assert!(!target("GET", "/api/admin/usage/records/summary"));
        let mut row =
            json!({"route":"/v1/responses","inputTokens":19,"clientTransport":"internal_plugin"});
        annotate(&mut row, &json!({"channel":"native_fallback"}));
        assert_eq!(row["route"], "/v1/responses");
        assert_eq!(row["inputTokens"], 19);
        assert_eq!(row["clientTransport"], "BPS回落");
        assert_eq!(row["originalClientTransport"], "internal_plugin");
        assert_eq!(row["originalRoute"], "/v1/responses");
        annotate(&mut row, &json!({"channel":"basis_points"}));
        assert_eq!(row["route"], "/v1/responses");
    }
}
