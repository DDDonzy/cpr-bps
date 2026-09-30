use gateway_plugin_sdk::{ErrorCode, PluginFault};
use serde::{Deserialize, Serialize};
use serde_json::{Value, json};
use std::collections::HashSet;

#[derive(Clone, Copy, Debug, Default, Deserialize, Serialize, PartialEq, Eq)]
#[serde(rename_all = "snake_case")]
pub enum FailurePolicy {
    #[default]
    NativeFallback,
    Reject,
}

/// 宿主实例 configuration 是唯一业务配置；运行绑定完全由此派生。
#[derive(Clone, Debug, Deserialize, Serialize, PartialEq, Eq)]
#[serde(rename_all = "camelCase", default, deny_unknown_fields)]
pub struct Config {
    pub enabled: bool,
    pub account_ids: Vec<String>,
    pub models: Vec<String>,
    pub timeout_ms: u64,
    pub on_failure: FailurePolicy,
}

impl Default for Config {
    fn default() -> Self {
        // 初始值只在清单定义一次；业务运行始终读取宿主保存的实例配置。
        let manifest: Value =
            serde_json::from_str(include_str!("../plugin.json")).expect("valid embedded manifest");
        let defaults = &manifest["configurationSchema"]["default"];
        Self {
            enabled: defaults["enabled"].as_bool().expect("enabled default"),
            account_ids: serde_json::from_value(defaults["accountIds"].clone())
                .expect("account defaults"),
            models: serde_json::from_value(defaults["models"].clone()).expect("model defaults"),
            timeout_ms: defaults["timeoutMs"].as_u64().expect("timeout default"),
            on_failure: FailurePolicy::NativeFallback,
        }
    }
}

impl Config {
    pub fn parse(value: Value) -> Result<Self, PluginFault> {
        let config: Self = serde_json::from_value(value).map_err(|_| invalid("设置格式无效"))?;
        if !(60_000..=14_400_000).contains(&config.timeout_ms) {
            return Err(invalid("等待时间须为 1 至 240 分钟"));
        }
        if config.models.len() > 256 || config.account_ids.len() > 200 {
            return Err(invalid("选择的模型或账号过多"));
        }
        for list in [&config.models, &config.account_ids] {
            let mut seen = HashSet::new();
            if list
                .iter()
                .any(|v| v.trim() != v || v.is_empty() || v.len() > 256 || !seen.insert(v))
            {
                return Err(invalid("模型和账号不能留空、重复或包含首尾空格"));
            }
        }
        Ok(config)
    }

    pub fn applies_to_model(&self, model: &str) -> bool {
        self.enabled && !self.account_ids.is_empty() && self.models.iter().any(|m| m == model)
    }

    pub fn allows_account(&self, account: &str) -> bool {
        self.account_ids.iter().any(|id| id == account)
    }

    pub fn allows_fallback(&self) -> bool {
        self.on_failure == FailurePolicy::NativeFallback
    }

    pub fn bindings(&self) -> Vec<Value> {
        if !self.enabled || self.account_ids.is_empty() || self.models.is_empty() {
            return Vec::new();
        }
        [
            (
                "donzy.excel-bps.middleware",
                "request",
                if self.allows_fallback() {
                    "delegate"
                } else {
                    "reject"
                },
                vec![],
            ),
            (
                "donzy.excel-bps.upstream-adapter",
                "upstream",
                "reject",
                vec!["openai"],
            ),
            (
                "donzy.excel-bps.scheduler",
                "scheduling",
                if self.allows_fallback() {
                    "delegate"
                } else {
                    "reject"
                },
                vec!["openai"],
            ),
        ]
        .into_iter()
        .map(|(contribution, stage, failure_policy, providers)| {
            json!({
                "contribution": contribution, "stage": stage, "order": 0,
                "failurePolicy": failure_policy, "clientKeyIds": [], "accountGroupIds": [],
                "providerIds": providers, "models": [], "identityBindings": []
            })
        })
        .collect()
    }
}

fn invalid(message: &str) -> PluginFault {
    PluginFault::new(ErrorCode::InvalidInput, message)
}

#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn preserves_policy_and_defaults_old_configs_to_fallback() {
        let config =
            Config::parse(json!({"onFailure":"reject", "accountIds":["acct_one"]})).unwrap();
        assert!(!config.allows_fallback());
        assert_eq!(
            serde_json::to_value(&config).unwrap()["onFailure"],
            "reject"
        );
        assert!(
            config
                .bindings()
                .iter()
                .all(|b| b["failurePolicy"] == "reject")
        );
        assert!(Config::parse(json!({})).unwrap().allows_fallback());
        assert!(Config::parse(json!({"onFailure":"unknown"})).is_err());
    }
    #[test]
    fn derives_internal_bindings_without_duplicate_route_settings() {
        let config = Config {
            account_ids: vec!["acct_one".into()],
            ..Config::default()
        };
        let bindings = config.bindings();
        assert_eq!(bindings.len(), 3);
        for binding in bindings {
            assert_eq!(binding["models"], json!([]));
            assert_eq!(binding["clientKeyIds"], json!([]));
        }
        let mut disabled = config;
        disabled.enabled = false;
        assert!(disabled.bindings().is_empty());
    }
    #[test]
    fn validates_account_and_model_selection() {
        let empty = Config::default();
        assert!(!empty.allows_account("any"));
        assert!(!empty.applies_to_model("gpt-6-luna"));
        assert_eq!(
            Config::parse(json!({"models":[],"accountIds":["acct_one"]}))
                .unwrap()
                .bindings()
                .len(),
            0
        );
        assert!(Config::parse(json!({"accountIds":["a","a"]})).is_err());
        let config = Config::parse(json!({"accountIds":["acct_one"]})).unwrap();
        assert!(config.allows_account("acct_one"));
        assert!(!config.allows_account("acct_two"));
    }
}
