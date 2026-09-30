//! 路由事实由实际执行点记录；仅装饰已通过宿主鉴权的日志响应，不改计费或请求路径。
use gateway_plugin_sdk::client::HostClient;
use gateway_plugin_sdk::{ErrorCode, PluginFault};
use serde_json::{Value, json};
use std::collections::HashMap;

const NAMESPACE: &str = "bps.routes";
const BUCKET_SIZE: usize = 64;
pub const RETENTION_MS: u64 = 30 * 24 * 60 * 60 * 1000;
pub fn now_ms() -> u64 {
    std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .unwrap_or_default()
        .as_millis() as u64
}
// 旧版未带时间字段；CPR 的 req_ 标识使用 UUIDv7，前 48 位是生成时间。
fn created_ms(value: &Value) -> Option<u64> {
    if let Some(ms) = value["recordedAtMs"].as_u64() {
        return Some(ms);
    }
    let id = value["requestId"].as_str()?.strip_prefix("req_")?;
    if id.len() != 32
        || !id.bytes().all(|b| b.is_ascii_hexdigit())
        || &id[12..13] != "7"
        || !b"89abAB".contains(&id.as_bytes()[16])
    {
        return None;
    }
    u64::from_str_radix(&id[..12], 16).ok()
}
fn cleaned(value: &Value, now: u64) -> Vec<Value> {
    value
        .as_array()
        .into_iter()
        .flatten()
        .filter_map(|entry| {
            let at = created_ms(entry)?;
            if at == 0 || at.saturating_add(RETENTION_MS) <= now || at > now.saturating_add(300_000)
            {
                return None;
            }
            let mut entry = entry.clone();
            entry["recordedAtMs"] = json!(at);
            Some(entry)
        })
        .collect()
}
async fn clean_bucket(host: &HostClient, key: &str) -> Result<Value, PluginFault> {
    for _ in 0..4 {
        let mut current = read(host, key).await?;
        if current.is_null() {
            return Ok(current);
        }
        let entries = cleaned(&current["value"]["entries"], now_ms());
        if current["value"]["entries"] == json!(entries) {
            return Ok(current);
        }
        let result = if entries.is_empty() {
            host.call(
                "host.state.delete",
                json!({"namespace":NAMESPACE,"key":key,"expected_version":current["version"]}),
                vec![],
            )
            .await
        } else {
            host.call("host.state.put",json!({"namespace":NAMESPACE,"key":key,"value":{"entries":entries},"expected_version":current["version"]}),vec![]).await
        };
        if result.is_ok() {
            current["value"]["entries"] = json!(entries);
            return Ok(current);
        }
    }
    Err(PluginFault::new(
        ErrorCode::Conflict,
        "日志清理遇到并发修改",
    ))
}
pub async fn reconcile(host: &HostClient) -> Result<(), PluginFault> {
    for bucket in 0..128 {
        clean_bucket(host, &format!("{bucket:02x}")).await?;
    }
    Ok(())
}
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
        let now = now_ms();
        let mut entries = cleaned(&current["value"]["entries"], now);
        let created = entries
            .iter()
            .find(|r| r["requestId"] == id)
            .and_then(created_ms)
            .unwrap_or(now);
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
            json!({"requestId":id,"channel":channel,"reason":reason,"relatedRequestId":related,"recordedAtMs":created}),
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
/// 只读管理接口接收已鉴权的日志 JSON；不再挂接全局 HTTP 入口。
pub async fn decorate(host: &HostClient, mut value: Value) -> Result<Value, PluginFault> {
    let Some(data) = value.get_mut("data") else {
        return Ok(value);
    };
    let mut cache: HashMap<String, Value> = HashMap::new();
    let rows: Vec<&mut Value> =
        if let Some(items) = data.get_mut("items").and_then(Value::as_array_mut) {
            if items.len() > 100 {
                return Err(PluginFault::new(ErrorCode::InvalidInput, "日志页过大"));
            }
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
            cache.insert(key.clone(), clean_bucket(host, &key).await?);
        }
        if let Some(fact) = cache[&key]["value"]["entries"]
            .as_array()
            .and_then(|list| list.iter().find(|v| v["requestId"] == id))
        {
            annotate(row, fact);
        }
    }
    Ok(value)
}
#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn expires_old_entries_and_migrates_uuid7_timestamps() {
        let now = 1_800_000_000_000u64;
        let v = json!([{"requestId":"new","recordedAtMs":now-1},{"requestId":"old","recordedAtMs":now-RETENTION_MS},{"requestId":"unknown"}]);
        let rows = cleaned(&v, now);
        assert_eq!(rows.len(), 1);
        assert_eq!(rows[0]["requestId"], "new");
        let legacy = json!({"requestId":"req_01a0f3065a6974d6b49ae7bd192b7401"});
        let ts = created_ms(&legacy).unwrap();
        assert_eq!(cleaned(&json!([legacy]), ts + 1).len(), 1);
        assert!(cleaned(&json!([legacy]), ts + RETENTION_MS).is_empty());
    }
    #[test]
    fn only_decorates_authorized_log_shapes_and_preserves_facts() {
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
