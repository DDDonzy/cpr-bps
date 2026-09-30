use crate::{config::Config, fallback::Registry, management};
use gateway_plugin_sdk::PluginFault;
use gateway_plugin_sdk::call::policy::{
    AccountScheduleCandidate, AccountScheduleDecision, AccountScheduleRequest,
};
use gateway_plugin_sdk::client::{TypedCall, TypedReply};
use std::{collections::HashSet, sync::Arc};

fn choose<'a>(
    candidates: &'a [AccountScheduleCandidate],
    allowed: &HashSet<String>,
) -> Option<&'a AccountScheduleCandidate> {
    candidates
        .iter()
        .filter(|c| allowed.contains(&c.account_id))
        .filter(|c| c.maximum_concurrency == 0 || c.in_flight < c.maximum_concurrency)
        .min_by_key(|c| c.in_flight)
}

pub async fn select(
    call: TypedCall<AccountScheduleRequest>,
    config: Config,
    registry: Arc<Registry>,
) -> Result<TypedReply<AccountScheduleDecision>, PluginFault> {
    if call.request.provider != "openai"
        || !call
            .request
            .model
            .as_deref()
            .is_some_and(|m| config.applies_to_model(m))
    {
        return Ok(TypedReply::new(AccountScheduleDecision::Delegate));
    }
    let available = management::accounts_cached(&call.host).await;
    let allowed: HashSet<String> = available
        .unwrap_or_default()
        .into_iter()
        .filter(|a| a.enabled && config.allows_account(&a.account_id))
        .map(|a| a.account_id)
        .collect();
    if let Some(candidate) = choose(&call.request.candidates, &allowed) {
        return Ok(TypedReply::new(AccountScheduleDecision::Pick {
            account_id: candidate.account_id.clone(),
        }));
    }
    // 候选里没有配置的账号时委托宿主自带调度；上游适配器仍会拦下不在范围内的账号。
    let _ = &registry;
    Ok(TypedReply::new(AccountScheduleDecision::Delegate))
}

#[cfg(test)]
mod tests {
    use super::*;
    fn candidate(id: &str, busy: u32) -> AccountScheduleCandidate {
        AccountScheduleCandidate {
            account_id: id.into(),
            weight: 1,
            in_flight: busy,
            maximum_concurrency: 3,
            last_started_at_ms: None,
            quota_reset_at_ms: None,
            quota_remaining_rank: None,
            failure_rate_basis_points: None,
            first_output_latency_ms: None,
        }
    }
    #[test]
    fn never_picks_outside_host_candidates_or_selected_accounts() {
        let candidates = vec![
            candidate("other", 0),
            candidate("chosen", 1),
            candidate("full", 3),
        ];
        let allowed = HashSet::from([
            "chosen".to_owned(),
            "full".to_owned(),
            "not-authorized".to_owned(),
        ]);
        assert_eq!(choose(&candidates, &allowed).unwrap().account_id, "chosen");
        assert!(choose(&candidates, &HashSet::from(["not-authorized".to_owned()])).is_none());
    }
}
