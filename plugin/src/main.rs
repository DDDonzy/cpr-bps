mod adapter;
mod config;
mod fallback;
mod management;
mod middleware;
mod route_log;
mod scheduler;
mod warmup;
mod worker;
use gateway_plugin_sdk::Manifest;
use gateway_plugin_sdk::call::upstream_adapter::{
    BuiltinProvider, UpstreamAdapterDeclaration, UpstreamAdapterRegistration, UpstreamPath,
    UpstreamPathPurpose, UpstreamTransport,
};
use gateway_plugin_sdk::client::{
    MiddlewareCall, MiddlewareResult, PluginBuilder, PluginSession, SessionConfig, TypedReply,
    methods,
};
use std::sync::Arc;

#[tokio::main]
async fn main() -> Result<(), Box<dyn std::error::Error>> {
    let session = PluginSession::accept(
        tokio::io::stdin(),
        tokio::io::stdout(),
        SessionConfig {
            // 业务并发由宿主管理，插件侧沿用 SDK 默认调用容量。
            maximum_calls: 32,
            maximum_callbacks: 32,
            maximum_buffered_stream_chunks: 32,
            // 使用官方 SDK 的回调块预算，不把单块读取申请放大到 16 MiB。
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
    let config = config::Config::parse(handshake.configuration.clone())
        .map_err(|error| std::io::Error::other(error.message))?;
    let worker = worker::Worker::start().await?;
    let registry = Arc::new(fallback::Registry::default());
    let adapter_registry = registry.clone();
    let warmups = Arc::new(warmup::Store::default());
    let models = config.models.clone();
    let adapter_config = config.clone();
    let schedule_config = config.clone();
    let schedule_registry = registry.clone();
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
            let config = adapter_config.clone();
            async move { adapter::execute(call, w, reg, config).await }
        })?
        .on(methods::SCHEDULE_ACCOUNT, move |call| {
            let config = schedule_config.clone();
            let registry = schedule_registry.clone();
            async move { scheduler::select(call, config, registry).await }
        })?
        .management(management::registration(), management::handle)?
        .middleware(move |call: MiddlewareCall| {
            let config = config.clone();
            let registry = registry.clone();
            let warmups = warmups.clone();
            async move {
                match call {
                    MiddlewareCall::Request(call) => {
                        middleware::handle(*call, config, registry, warmups)
                            .await
                            .map(MiddlewareResult::Request)
                    }
                    MiddlewareCall::Http(call) => {
                        route_log::handle(*call).await.map(MiddlewareResult::Http)
                    }
                    other => other.forward().await,
                }
            }
        })?
        .build()?;
    session.run(plugin).await?;
    Ok(())
}
