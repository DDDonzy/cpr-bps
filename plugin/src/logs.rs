//! 插件内置日志页只通过公开宿主接口读取数据，无域名、端口或实例 ID 配置。
use gateway_plugin_sdk::client::HostClient;
use gateway_plugin_sdk::{ErrorCode, PluginFault};
use serde::Deserialize;
use serde_json::{Value, json};

#[derive(Deserialize)]
#[serde(rename_all = "camelCase", deny_unknown_fields)]
struct Query {
    kind: String,
    current_page: u32,
    page_size: u32,
    start_time: String,
    end_time: String,
    #[serde(default)]
    search: String,
}
fn invalid() -> PluginFault {
    PluginFault::new(ErrorCode::InvalidInput, "日志查询参数无效")
}
fn encode(value: &str) -> String {
    let mut out = String::new();
    for b in value.bytes() {
        if b.is_ascii_alphanumeric() || b"-._~".contains(&b) {
            out.push(char::from(b));
        } else {
            use std::fmt::Write;
            let _ = write!(&mut out, "%{b:02X}");
        }
    }
    out
}
fn bound_times(q: &mut Query, now: u64) -> Result<(), PluginFault> {
    let start = chrono::DateTime::parse_from_rfc3339(&q.start_time)
        .map_err(|_| invalid())?
        .timestamp_millis();
    let end = chrono::DateTime::parse_from_rfc3339(&q.end_time)
        .map_err(|_| invalid())?
        .timestamp_millis();
    let cutoff = now.saturating_sub(crate::route_log::RETENTION_MS) as i64;
    let start = start.max(cutoff);
    let end = end.min(now as i64);
    if start >= end {
        return Err(PluginFault::new(
            ErrorCode::InvalidInput,
            "仅支持查询最近 30 天的日志",
        ));
    }
    q.start_time = chrono::DateTime::from_timestamp_millis(start)
        .ok_or_else(invalid)?
        .to_rfc3339_opts(chrono::SecondsFormat::Millis, true);
    q.end_time = chrono::DateTime::from_timestamp_millis(end)
        .ok_or_else(invalid)?
        .to_rfc3339_opts(chrono::SecondsFormat::Millis, true);
    Ok(())
}
fn uri(q: &Query) -> Result<String, PluginFault> {
    let path = match q.kind.as_str() {
        "usage" => "/api/admin/usage/records",
        "errors" => "/api/admin/operations/errors",
        _ => return Err(invalid()),
    };
    if q.current_page == 0
        || q.current_page > 100_000
        || ![10, 20, 50, 100].contains(&q.page_size)
        || q.search.len() > 512
        || q.start_time.len() > 40
        || q.end_time.len() > 40
        || q.start_time.is_empty()
        || q.end_time.is_empty()
    {
        return Err(invalid());
    }
    Ok(format!(
        "{path}?currentPage={}&pageSize={}&startTime={}&endTime={}&search={}",
        q.current_page,
        q.page_size,
        encode(&q.start_time),
        encode(&q.end_time),
        encode(&q.search)
    ))
}
async fn decorate(host: &HostClient, data: Value) -> Result<Value, PluginFault> {
    // 元数据故障不遮蔽 CPR 原始记录，且未标记的历史不能猜测成原生通道。
    match crate::route_log::decorate(host, json!({"data":data.clone()})).await {
        Ok(mut v) => Ok(v["data"].take()),
        Err(_) => {
            let mut data = data;
            data["bpsWarning"] = json!("路由标记暂不可用，当前显示 CPR 原始记录");
            Ok(data)
        }
    }
}
pub async fn query(host: &HostClient, payload: &[u8]) -> Result<Value, PluginFault> {
    let mut q: Query = serde_json::from_slice(payload).map_err(|_| invalid())?;
    bound_times(&mut q, crate::route_log::now_ms())?;
    let data = crate::management::admin(host, "GET", &uri(&q)?, None).await?;
    decorate(host, data).await
}
pub async fn detail(host: &HostClient, payload: &[u8]) -> Result<Value, PluginFault> {
    #[derive(Deserialize)]
    #[serde(deny_unknown_fields)]
    struct Detail {
        id: String,
    }
    let q: Detail = serde_json::from_slice(payload).map_err(|_| invalid())?;
    if q.id.is_empty()
        || q.id.len() > 128
        || !q
            .id
            .bytes()
            .all(|b| b.is_ascii_alphanumeric() || b"_-".contains(&b))
    {
        return Err(invalid());
    }
    let data = crate::management::admin(
        host,
        "GET",
        &format!("/api/admin/usage/records/detail?id={}", encode(&q.id)),
        None,
    )
    .await?;
    let at = data["createdAt"]
        .as_str()
        .and_then(|v| chrono::DateTime::parse_from_rfc3339(v).ok())
        .map(|v| v.timestamp_millis());
    let now = crate::route_log::now_ms() as i64;
    if at.is_none_or(|ms| ms <= now - crate::route_log::RETENTION_MS as i64 || ms > now + 300_000) {
        return Err(PluginFault::new(
            ErrorCode::InvalidInput,
            "仅支持查询最近 30 天的日志",
        ));
    }
    decorate(host, data).await
}
#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn clamps_queries_to_30_days_and_rejects_expired_ranges() {
        let now = chrono::DateTime::parse_from_rfc3339("2026-10-01T00:00:00Z")
            .unwrap()
            .timestamp_millis() as u64;
        let mut q = Query {
            kind: "usage".into(),
            current_page: 1,
            page_size: 20,
            start_time: "2026-08-01T00:00:00Z".into(),
            end_time: "2026-12-01T00:00:00Z".into(),
            search: String::new(),
        };
        bound_times(&mut q, now).unwrap();
        assert_eq!(q.start_time, "2026-09-01T00:00:00.000Z");
        assert_eq!(q.end_time, "2026-10-01T00:00:00.000Z");
        q.end_time = "2026-08-02T00:00:00Z".into();
        assert!(bound_times(&mut q, now).is_err());
    }
    #[test]
    fn queries_only_fixed_host_routes_and_encodes_values() {
        let mut q = Query {
            kind: "usage".into(),
            current_page: 1,
            page_size: 20,
            start_time: "2026-10-01T00:00:00Z".into(),
            end_time: "2026-10-02T00:00:00Z".into(),
            search: "a&accountId=other/测试".into(),
        };
        let value = uri(&q).unwrap();
        assert!(value.starts_with("/api/admin/usage/records?"));
        assert!(!value.contains("&accountId="));
        assert!(value.contains("%26accountId%3Dother%2F"));
        q.kind = "https://example.invalid".into();
        assert!(uri(&q).is_err());
        q.kind = "errors".into();
        q.page_size = 1000;
        assert!(uri(&q).is_err());
    }
}
