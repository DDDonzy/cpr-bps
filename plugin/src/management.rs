use crate::config::Config;
use gateway_plugin_sdk::call::{
    host::{AuthListRequest, AuthListResult, AuthRuntimeAccount},
    management::{
        ManagementPage, ManagementRegistration, ManagementRequest, ManagementResource,
        ManagementResponse, ManagementRoute,
    },
    middleware::{MiddlewareHeader, http::Version},
};
use gateway_plugin_sdk::client::{
    HostClient, HttpBody, HttpFrame, HttpRequest, TypedCall, TypedReply,
};
use gateway_plugin_sdk::{ErrorCode, PluginFault};
use serde::Deserialize;
use serde_json::{Value, json};
use std::collections::BTreeSet;

fn fault(message: &str) -> PluginFault {
    PluginFault::new(ErrorCode::Fault, message)
}

pub fn registration() -> ManagementRegistration {
    ManagementRegistration {
        routes: [
            ("GET", "settings"),
            ("POST", "settings"),
            ("GET", "catalog"),
        ]
        .into_iter()
        .map(|(method, path)| ManagementRoute {
            method: method.into(),
            path: path.into(),
            request_content_types: if method == "POST" {
                vec!["application/json".into()]
            } else {
                vec![]
            },
            response_content_types: vec!["application/json".into()],
        })
        .collect(),
        resources: ["web/index.html", "web/app.js", "web/style.css"]
            .into_iter()
            .map(|path| ManagementResource {
                path: path.into(),
                public: false,
            })
            .collect(),
        pages: vec![ManagementPage {
            id: "settings".into(),
            title: "BPS 设置".into(),
            description: Some("统一管理账号与模型，失败时自动安全回落原生".into()),
            entry: "web/index.html".into(),
            icon: None,
        }],
        callbacks: vec![],
    }
}

pub async fn admin(
    host: &HostClient,
    method: &str,
    uri: &str,
    value: Option<Value>,
) -> Result<Value, PluginFault> {
    let payload = value
        .map(|v| serde_json::to_vec(&v))
        .transpose()
        .map_err(|_| fault("设置编码失败"))?;
    let headers = if payload.is_some() {
        vec![MiddlewareHeader {
            name: "content-type".into(),
            value: b"application/json".to_vec(),
        }]
    } else {
        vec![]
    };
    let mut response = host
        .dispatch_http(HttpRequest {
            settings: Value::Null,
            method: method.into(),
            uri: uri.into(),
            version: Version::Http11,
            headers,
            timeout_ms: Some(20000),
            body: payload.map_or_else(HttpBody::empty, HttpBody::from_bytes),
        })
        .await?;
    let status = response.status;
    let mut bytes = Vec::new();
    while let Some(frame) = response.body.read().await? {
        if let HttpFrame::Data(data) = frame {
            if bytes.len() + data.len() > 1024 * 1024 {
                return Err(fault("宿主响应过大"));
            }
            bytes.extend(data);
        }
    }
    let value: Value = serde_json::from_slice(&bytes).map_err(|_| fault("宿主返回无效数据"))?;
    if status >= 400 || value["code"].as_u64().is_some_and(|code| code >= 400) {
        let message = value["message"]
            .as_str()
            .unwrap_or("保存失败，请刷新后重试");
        let error_status = if status >= 400 {
            status
        } else {
            value["code"]
                .as_u64()
                .filter(|code| (400..600).contains(code))
                .map_or(502, |code| code as u16)
        };
        let mut error = PluginFault::new(
            if error_status == 409 {
                ErrorCode::Conflict
            } else {
                ErrorCode::Rejected
            },
            message,
        );
        error.http_status = Some(error_status);
        return Err(error);
    }
    Ok(value.get("data").cloned().unwrap_or(value))
}

static ACCOUNT_CACHE: std::sync::Mutex<Option<(std::time::Instant, Vec<AuthRuntimeAccount>)>> =
    std::sync::Mutex::new(None);
const ACCOUNT_CACHE_TTL: std::time::Duration = std::time::Duration::from_secs(5);

/// 账号列表在极短窗口内复用，避免每个请求重复访问管理接口。
pub async fn accounts_cached(host: &HostClient) -> Result<Vec<AuthRuntimeAccount>, PluginFault> {
    if let Ok(guard) = ACCOUNT_CACHE.lock()
        && let Some((at, value)) = guard.as_ref()
        && at.elapsed() < ACCOUNT_CACHE_TTL
    {
        return Ok(value.clone());
    }
    let value = accounts(host).await?;
    if let Ok(mut guard) = ACCOUNT_CACHE.lock() {
        *guard = Some((std::time::Instant::now(), value.clone()));
    }
    Ok(value)
}

pub async fn accounts(host: &HostClient) -> Result<Vec<AuthRuntimeAccount>, PluginFault> {
    let mut result = Vec::new();
    let mut cursor = None;
    loop {
        let payload = serde_json::to_vec(&AuthListRequest {
            provider_id: Some("openai".into()),
            cursor: cursor.clone(),
            limit: 200,
        })
        .map_err(|_| fault("账号查询编码失败"))?;
        let reply = host
            .call("host.auth.list", json!({}), payload)
            .await
            .map_err(|_| fault("无法读取账号列表"))?;
        let page: AuthListResult =
            serde_json::from_slice(&reply.payload).map_err(|_| fault("账号列表格式无效"))?;
        result.extend(
            page.accounts
                .into_iter()
                .filter(|a| a.provider_id == "openai" && a.authentication_kind == "oauth"),
        );
        match page.next_cursor {
            Some(next) if cursor.as_ref() != Some(&next) => cursor = Some(next),
            _ => break,
        }
    }
    Ok(result)
}

async fn instance(host: &HostClient, id: &str) -> Result<Value, PluginFault> {
    let values = admin(host, "GET", "/api/admin/plugins/instances", None).await?;
    values
        .as_array()
        .and_then(|v| v.iter().find(|v| v["id"] == id))
        .cloned()
        .ok_or_else(|| fault("当前插件实例不存在"))
}

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct Save {
    revision: u64,
    configuration: Value,
}

async fn handle_inner(call: &TypedCall<ManagementRequest>) -> Result<Value, PluginFault> {
    let current = instance(&call.host, &call.context.instance_id).await?;
    let config = Config::parse(current["configuration"].clone())?;
    match (call.request.method.as_str(), call.request.path.as_str()) {
        ("GET", "settings") => {
            Ok(json!({"revision":current["revision"],"configuration":config,"fallback":"native"}))
        }
        ("GET", "catalog") => {
            let available = accounts(&call.host).await?;
            let mut models: BTreeSet<String> = config.models.iter().cloned().collect();
            let mut warning = None;
            // 目录来自已有账号缓存，不需要用户选择或提供 Client Key。
            if let Some(account) = available
                .iter()
                .find(|a| a.enabled && config.allows_account(&a.account_id))
                .or_else(|| available.iter().find(|a| a.enabled))
            {
                let uri = format!(
                    "/api/admin/accounts/models?accountId={}",
                    account.account_id
                );
                match admin(&call.host, "GET", &uri, None).await {
                    Ok(value) => {
                        if let Some(items) = value
                            .get("models")
                            .and_then(Value::as_array)
                            .or_else(|| value.as_array())
                        {
                            for item in items {
                                if let Some(id) = item
                                    .as_str()
                                    .or_else(|| item["id"].as_str())
                                    .or_else(|| item["model"].as_str())
                                {
                                    models.insert(id.to_owned());
                                }
                            }
                        }
                    }
                    Err(_) => warning = Some("暂时无法刷新模型目录，已保留当前模型"),
                }
            }
            Ok(
                json!({"accounts":available.iter().map(|a| json!({"id":a.account_id,"name":a.name,"email":a.email,"enabled":a.enabled,"plan":a.plan_type})).collect::<Vec<_>>(),
                "models":models,"warning":warning}),
            )
        }
        ("POST", "settings") => {
            let input: Save = serde_json::from_slice(&call.payload)
                .map_err(|_| PluginFault::new(ErrorCode::InvalidInput, "设置内容无效"))?;
            if current["revision"].as_u64() != Some(input.revision) {
                let mut error =
                    PluginFault::new(ErrorCode::Conflict, "配置已被其他窗口修改，请刷新后重试");
                error.http_status = Some(409);
                return Err(error);
            }
            let next = Config::parse(input.configuration)?;
            let available = accounts(&call.host).await?;
            if next
                .account_ids
                .iter()
                .any(|id| !available.iter().any(|a| &a.account_id == id))
            {
                return Err(PluginFault::new(
                    ErrorCode::InvalidInput,
                    "所选账号不存在或不是 OpenAI OAuth 账号",
                ));
            }
            if serde_json::to_value(&next).ok().as_ref() == Some(&current["configuration"])
                && serde_json::to_value(next.bindings()).ok().as_ref() == Some(&current["bindings"])
            {
                return Ok(
                    json!({"saved":true,"configuration":next,"revision":current["revision"]}),
                );
            }
            let saved = admin(&call.host, "POST", "/api/admin/plugins/instances/update", Some(json!({
                "id":call.context.instance_id, "instance":{
                    "expectedRevision":input.revision,"name":current["name"],"artifactSha256":current["artifactSha256"],
                    "enabled":true,"configuration":next,"bindings":next.bindings()
                }
            }))).await?;
            Ok(json!({"saved":true,"configuration":next,"revision":saved.get("revision")}))
        }
        _ => Err(PluginFault::new(ErrorCode::Unsupported, "未知管理操作")),
    }
}

pub async fn handle(
    call: TypedCall<ManagementRequest>,
) -> Result<TypedReply<ManagementResponse>, PluginFault> {
    let (status, value) = match handle_inner(&call).await {
        Ok(value) => (200, value),
        Err(error) => (
            error.http_status.unwrap_or(match error.code {
                ErrorCode::InvalidInput => 400,
                ErrorCode::Conflict => 409,
                _ => 502,
            }),
            json!({"error":error.message}),
        ),
    };
    let payload = serde_json::to_vec(&value).map_err(|_| fault("响应编码失败"))?;
    Ok(TypedReply::new(ManagementResponse {
        status,
        content_type: "application/json".into(),
        headers: vec![],
    })
    .with_payload(payload))
}
