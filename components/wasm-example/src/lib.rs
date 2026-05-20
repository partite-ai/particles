//! Minimal native-WASM particle. Implements `particle:runtime` —
//! tools, health, manifest — directly in Rust. Built as a wasi:p2
//! component; the build pipeline packages it via `particle build
//! --component <path>`.
//!
//! Two tools (`echo`, `add`) and a `ping`. No credentials, no kv,
//! no HTTP — keeps this fixture standalone for the test suite.

wit_bindgen::generate!({
    world: "wasm-particle",
    path: "wit",
    generate_all,
});

use exports::particle::runtime::tools::{Guest as ToolsGuest, ToolDef, ToolError};
use exports::particle::runtime::health::{Guest as HealthGuest, HealthError, PingResult, Status};
use exports::particle::runtime::manifest::{
    Guest as ManifestGuest, ManifestError, ParticleManifest,
    CapabilitySet, ToolEntry,
};

struct Component;

impl ToolsGuest for Component {
    fn list_tools() -> Vec<ToolDef> {
        vec![
            ToolDef {
                name: "echo".into(),
                description: "Echo the input string back.".into(),
                input_schema_json: r#"{"type":"object","properties":{"input":{"type":"string"}},"required":["input"]}"#.into(),
            },
            ToolDef {
                name: "add".into(),
                description: "Add two numbers and return the sum.".into(),
                input_schema_json: r#"{"type":"object","properties":{"a":{"type":"number"},"b":{"type":"number"}},"required":["a","b"]}"#.into(),
            },
        ]
    }

    fn call_tool(name: String, arguments_json: String) -> Result<String, ToolError> {
        // Args are already validated host-side against the input
        // schema — we can string-search for the values without a
        // real JSON parser. Keeps this fixture dependency-free.
        match name.as_str() {
            "echo" => {
                let input = extract_string(&arguments_json, "input")
                    .ok_or_else(|| ToolError::HandlerError("missing input".into()))?;
                Ok(format!(r#"{{"result":"{}"}}"#, input))
            }
            "add" => {
                let a = extract_number(&arguments_json, "a")
                    .ok_or_else(|| ToolError::HandlerError("missing a".into()))?;
                let b = extract_number(&arguments_json, "b")
                    .ok_or_else(|| ToolError::HandlerError("missing b".into()))?;
                Ok(format!(r#"{{"sum":{}}}"#, a + b))
            }
            _ => Err(ToolError::NotFound),
        }
    }
}

impl HealthGuest for Component {
    fn ping() -> Result<PingResult, HealthError> {
        Ok(PingResult {
            status: Status::Ok,
            message: Some("native wasm particle alive".into()),
            details: None,
        })
    }
}

impl ManifestGuest for Component {
    fn get_manifest() -> Result<ParticleManifest, ManifestError> {
        Ok(ParticleManifest {
            name: "wasm-example".into(),
            description: "Native-WASM particle fixture — echo + add.".into(),
            version: "0.1.0".into(),
            capabilities: CapabilitySet { http: None },
            credentials: vec![],
            tools: vec![
                ToolEntry {
                    name: "echo".into(),
                    description: "Echo the input string back.".into(),
                    input_schema_json: r#"{"type":"object","properties":{"input":{"type":"string"}},"required":["input"]}"#.into(),
                },
                ToolEntry {
                    name: "add".into(),
                    description: "Add two numbers and return the sum.".into(),
                    input_schema_json: r#"{"type":"object","properties":{"a":{"type":"number"},"b":{"type":"number"}},"required":["a","b"]}"#.into(),
                },
            ],
        })
    }
}

export!(Component);

// -- tiny JSON helpers ----------------------------------------------------
//
// The arguments JSON has the shape `{"a": 1, "b": 2}` — we know
// the keys exactly. A real component would use serde_json; here we
// keep the fixture small enough to compile in seconds without deps.

fn extract_string(json: &str, key: &str) -> Option<String> {
    let needle = format!("\"{}\"", key);
    let pos = json.find(&needle)?;
    let after = &json[pos + needle.len()..];
    let colon = after.find(':')?;
    let rest = after[colon + 1..].trim_start();
    let rest = rest.strip_prefix('"')?;
    let end = rest.find('"')?;
    Some(rest[..end].to_string())
}

fn extract_number(json: &str, key: &str) -> Option<f64> {
    let needle = format!("\"{}\"", key);
    let pos = json.find(&needle)?;
    let after = &json[pos + needle.len()..];
    let colon = after.find(':')?;
    let rest = after[colon + 1..].trim_start();
    // number ends at ',', '}', or whitespace
    let end = rest.find(|c: char| c == ',' || c == '}' || c.is_whitespace())
        .unwrap_or(rest.len());
    rest[..end].parse().ok()
}
