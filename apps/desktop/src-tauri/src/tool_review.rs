use serde_json::{json, Value};

const INVALID: &str = "invalid tool review settings";

pub(crate) fn validate_settings(value: &Value, patch: bool) -> Result<(), String> {
    let object = value.as_object().ok_or(INVALID)?;
    if object.is_empty() {
        return Err(INVALID.into());
    }
    let fields = [
        "enabled",
        "endpoint",
        "model",
        "prompt",
        "mode",
        "timeout_ms",
    ];
    if !patch
        && fields
            .iter()
            .chain(["revision", "credential_configured"].iter())
            .any(|key| !object.contains_key(*key))
    {
        return Err(INVALID.into());
    }
    if !patch
        && object.get("enabled") == Some(&Value::Bool(true))
        && object.get("credential_configured") != Some(&Value::Bool(true))
    {
        return Err(INVALID.into());
    }
    for (key, value) in object {
        let valid = match key.as_str() {
            "enabled" => value.is_boolean(),
            "endpoint" => value.as_str().is_some_and(|value| {
                value.len() <= 4096
                    && value == value.trim()
                    && reqwest::Url::parse(value).is_ok_and(|url| {
                        matches!(url.scheme(), "http" | "https")
                            && url.host_str().is_some()
                            && url.username().is_empty()
                            && url.password().is_none()
                            && url.query().is_none()
                            && url.fragment().is_none()
                    })
            }),
            "model" => bounded_text(value, 256),
            "prompt" => value.as_str().is_some_and(|value| {
                value.len() <= 128 << 10
                    && value.contains("{{user_request}}")
                    && value.contains("{{tool_call}}")
            }),
            "mode" => matches!(value.as_str(), Some("mark" | "reject")),
            "timeout_ms" => value
                .as_u64()
                .is_some_and(|value| (1..=120000).contains(&value)),
            "revision" if !patch => bounded_text(value, 128),
            "credential_configured" if !patch => value.is_boolean(),
            "api_key" if patch => {
                valid_line(value, 16384, true)
                    && value.as_str().is_some_and(|value| value == value.trim())
            }
            "data_transfer_acknowledged" if patch => value.is_boolean(),
            _ => false,
        };
        if !valid {
            return Err(INVALID.into());
        }
    }
    Ok(())
}

fn valid_line(value: &Value, max: usize, empty: bool) -> bool {
    value.as_str().is_some_and(|value| {
        value.len() <= max
            && (empty || !value.trim().is_empty())
            && !value.chars().any(char::is_control)
    })
}

// Mirrors core validateBoundedText: limits count characters and tab is allowed.
fn bounded_text(value: &Value, max: usize) -> bool {
    value.as_str().is_some_and(|value| {
        value.chars().count() <= max
            && !value.trim().is_empty()
            && !value.chars().any(|c| c < ' ' && c != '\t')
    })
}

pub(crate) fn settings_record(etag: String, body: &[u8]) -> Result<Value, String> {
    let settings: Value = serde_json::from_slice(body).map_err(|_| INVALID)?;
    validate_settings(&settings, false)?;
    Ok(json!({"settings": settings, "etag": etag}))
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn timeout_accepts_one_to_120000_milliseconds_for_reads_and_patches() {
        let mut settings = json!({"enabled":false,"endpoint":"https://api.typesafe.ai/v1/systemone","model":"jev-latest","prompt":"{{user_request}}\n{{tool_call}}","mode":"mark","timeout_ms":5000,"revision":"r1","credential_configured":true});
        for timeout in [json!(1), json!(5000), json!(5001), json!(120000)] {
            settings["timeout_ms"] = timeout.clone();
            assert!(validate_settings(&settings, false).is_ok());
            assert!(validate_settings(&json!({"timeout_ms":timeout}), true).is_ok());
        }
        for timeout in [json!(0), json!(120001), json!(1.5), json!("5000")] {
            settings["timeout_ms"] = timeout.clone();
            assert!(validate_settings(&settings, false).is_err());
            assert!(validate_settings(&json!({"timeout_ms":timeout}), true).is_err());
        }
    }

    #[test]
    fn settings_boundary_rejects_secrets_and_unsafe_configuration() {
        let settings = json!({"enabled":false,"endpoint":"https://api.typesafe.ai/v1/systemone","model":"jev-latest","prompt":"{{user_request}}\n{{tool_call}}","mode":"mark","timeout_ms":5000,"revision":"r1","credential_configured":true});
        assert!(validate_settings(&settings, false).is_ok());
        for (key, value) in [
            ("api_key", json!("secret")),
            ("mode", json!("threshold")),
            ("timeout_ms", json!(120001)),
            ("endpoint", json!("https://user:secret@example.com")),
            ("prompt", json!("{{tool_call}}")),
        ] {
            let mut invalid = settings.clone();
            invalid[key] = value;
            let error = validate_settings(&invalid, false).unwrap_err();
            assert_eq!(error, INVALID);
        }
        assert!(validate_settings(
            &json!({"api_key":"write-only","enabled":true,"data_transfer_acknowledged":true}),
            true
        )
        .is_ok());
        assert!(validate_settings(&json!({"api_key":""}), true).is_ok());
        assert!(validate_settings(&json!({"api_key":"secret\nheader"}), true).is_err());
        assert!(validate_settings(&json!({"revision":"forged"}), true).is_err());
        // Core counts characters, not bytes, and allows tab in model names.
        assert!(validate_settings(&json!({"model":"模".repeat(256)}), true).is_ok());
        assert!(validate_settings(&json!({"model":"jev\tlatest"}), true).is_ok());
        assert!(validate_settings(&json!({"model":"模".repeat(257)}), true).is_err());
        assert!(validate_settings(&json!({"model":"jev\nlatest"}), true).is_err());
    }
}
