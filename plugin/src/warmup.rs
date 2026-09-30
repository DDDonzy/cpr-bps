use gateway_plugin_sdk::call::middleware::{MiddlewareBodyFrame, MiddlewareBodyFraming};
use gateway_plugin_sdk::client::{MiddlewareBody, MiddlewareResponse};
use gateway_plugin_sdk::{ErrorCode, PluginFault};
use serde_json::{Value, json};
use std::{
    collections::HashMap,
    sync::Mutex,
    time::{Duration, Instant, SystemTime, UNIX_EPOCH},
};
const PREFIX: &str = "resp_bps_warmup_";
struct Entry {
    key: String,
    model: String,
    conversation: String,
    input: Value,
    parent: Option<String>,
    bytes: usize,
    expires: Instant,
}
#[derive(Default)]
pub struct Store {
    entries: Mutex<HashMap<String, Entry>>,
}
pub enum Action {
    Pass,
    Rewrite(Value),
    Ready(Box<MiddlewareResponse>),
}
fn bad(message: &str) -> PluginFault {
    PluginFault::new(ErrorCode::InvalidInput, message)
}
fn inputs(v: &Value) -> Result<Vec<Value>, PluginFault> {
    match v {
        Value::Null => Ok(vec![]),
        Value::Array(a) => Ok(a.clone()),
        Value::String(s) => Ok(vec![
            json!({"role":"user","content":[{"type":"input_text","text":s}]}),
        ]),
        _ => Err(bad("invalid warmup input")),
    }
}
impl Store {
    pub fn prepare(
        &self,
        key: &str,
        model: &str,
        conversation: &str,
        request_id: &str,
        body: &Value,
    ) -> Result<Action, PluginFault> {
        let generate = match body.get("generate") {
            None => true,
            Some(Value::Bool(v)) => *v,
            _ => return Err(bad("generate must be a boolean")),
        };
        let now = Instant::now();
        let mut entries = self
            .entries
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner);
        entries.retain(|_, e| e.expires > now);
        let mut value = body.clone();
        let mut changed = false;
        if let Some(id) = body["previous_response_id"]
            .as_str()
            .filter(|id| id.starts_with(PREFIX))
        {
            let entry = entries
                .get(id)
                .filter(|e| e.key == key && e.model == model && e.conversation == conversation)
                .ok_or_else(|| bad("previous_response_not_found: warmup history unavailable"))?;
            let mut all = inputs(&entry.input)?;
            all.extend(inputs(&value["input"])?);
            value["input"] = Value::Array(all);
            if let Some(parent) = &entry.parent {
                value["previous_response_id"] = json!(parent);
            } else {
                value
                    .as_object_mut()
                    .ok_or_else(|| bad("invalid warmup request"))?
                    .remove("previous_response_id");
            }
            changed = true;
        }
        if generate {
            if value.get("generate").is_some() {
                value
                    .as_object_mut()
                    .ok_or_else(|| bad("invalid request"))?
                    .remove("generate");
                changed = true;
            }
            return Ok(if changed {
                Action::Rewrite(value)
            } else {
                Action::Pass
            });
        }
        if value["store"] != false {
            return Ok(if changed {
                Action::Rewrite(value)
            } else {
                Action::Pass
            });
        }
        let input = value.get("input").cloned().unwrap_or(Value::Null);
        let _ = inputs(&input)?;
        let bytes = serde_json::to_vec(&input)
            .map_err(|_| bad("warmup encoding failed"))?
            .len();
        if bytes > 8 * 1024 * 1024 {
            return Err(PluginFault::new(
                ErrorCode::Capacity,
                "warmup history exceeds local cache capacity",
            ));
        }
        entries
            .retain(|_, e| !(e.key == key && e.model == model && e.conversation == conversation));
        if entries.values().map(|e| e.bytes).sum::<usize>() + bytes > 16 * 1024 * 1024 {
            return Err(PluginFault::new(
                ErrorCode::Capacity,
                "warmup cache capacity unavailable",
            ));
        }
        let id = format!("{PREFIX}{request_id}");
        entries.insert(
            id.clone(),
            Entry {
                key: key.to_owned(),
                model: model.to_owned(),
                conversation: conversation.to_owned(),
                input,
                parent: value["previous_response_id"].as_str().map(str::to_owned),
                bytes,
                expires: now + Duration::from_secs(300),
            },
        );
        let created = SystemTime::now()
            .duration_since(UNIX_EPOCH)
            .map_or(0, |d| d.as_secs());
        let response = json!({"id":id,"object":"response","created_at":created,"model":model,"status":"in_progress","output":[],"usage":null});
        let mut completed = response.clone();
        completed["status"] = json!("completed");
        let a = json!({"type":"response.created","sequence_number":0,"response":response});
        let b = json!({"type":"response.completed","sequence_number":1,"response":completed});
        Ok(Action::Ready(Box::new(MiddlewareResponse::direct(
            "openai",
            200,
            vec![],
            MiddlewareBody::from_frames(
                MiddlewareBodyFraming::JsonDocument,
                vec![
                    MiddlewareBodyFrame::new(
                        serde_json::to_vec(&a).map_err(|_| bad("warmup response encoding"))?,
                        false,
                    ),
                    MiddlewareBodyFrame::new(
                        serde_json::to_vec(&b).map_err(|_| bad("warmup response encoding"))?,
                        true,
                    ),
                ],
            ),
        ))))
    }
}
#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn expands_warmup_and_preserves_real_parent() {
        let s = Store::default();
        let b = json!({"generate":false,"store":false,"input":[{"role":"user","content":"first"}],"previous_response_id":"resp_real"});
        assert!(matches!(
            s.prepare("k", "m", "c", "1", &b).unwrap(),
            Action::Ready(_)
        ));
        let next = json!({"previous_response_id":"resp_bps_warmup_1","input":[{"role":"user","content":"next"}]});
        let Action::Rewrite(v) = s.prepare("k", "m", "c", "2", &next).unwrap() else {
            panic!("missing rewrite")
        };
        assert_eq!(v["input"].as_array().unwrap().len(), 2);
        assert_eq!(v["previous_response_id"], "resp_real");
        assert!(s.prepare("other-key", "m", "c", "3", &next).is_err());
        assert!(
            s.prepare("k", "m", "other-conversation", "4", &next)
                .is_err()
        );
    }
}
