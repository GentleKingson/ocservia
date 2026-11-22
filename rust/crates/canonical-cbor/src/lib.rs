//! RFC 8949 Section 4.2.1 Core Deterministic CBOR encoder for cross-language
//! signing transcripts and semantic hashes.

#![forbid(unsafe_code)]

use std::fmt;

use ciborium::Value;
use serde::{Serialize, Serializer};
use sha2::{Digest, Sha256};

/// Explicit byte-string wrapper so byte slices serialize as CBOR major type 2
/// (`bstr`), matching Go `[]byte`.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct Bytes<'a>(pub &'a [u8]);

impl Serialize for Bytes<'_> {
    fn serialize<S: Serializer>(&self, serializer: S) -> Result<S::Ok, S::Error> {
        serializer.serialize_bytes(self.0)
    }
}

/// Optional byte-string wrapper that encodes `None` as CBOR `null` and `Some`
/// as CBOR major type 2 (`bstr`).
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct OptBytes<'a>(pub Option<&'a [u8]>);

impl Serialize for OptBytes<'_> {
    fn serialize<S: Serializer>(&self, serializer: S) -> Result<S::Ok, S::Error> {
        match self.0 {
            Some(bytes) => serializer.serialize_bytes(bytes),
            None => serializer.serialize_none(),
        }
    }
}

/// Errors returned while producing deterministic CBOR transcripts.
#[derive(Debug)]
pub enum Error {
    /// Serde conversion into a CBOR value failed.
    Serialize(String),
    /// Floating-point values are disallowed in canonical transcripts.
    UnsupportedFloat,
    /// CBOR tags are disallowed in canonical transcripts.
    UnsupportedTag,
    /// A map contained duplicate keys after canonicalization.
    DuplicateMapKey,
}

impl fmt::Display for Error {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            Self::Serialize(msg) => write!(f, "canonical CBOR serialize error: {msg}"),
            Self::UnsupportedFloat => f.write_str("floats are not permitted in canonical CBOR"),
            Self::UnsupportedTag => f.write_str("CBOR tags are not permitted in canonical CBOR"),
            Self::DuplicateMapKey => f.write_str("duplicate map key in canonical CBOR"),
        }
    }
}

impl std::error::Error for Error {}

/// Encodes `value` using RFC 8949 Section 4.2.1 Core Deterministic CBOR
/// prefixed with `domain`.
///
/// # Errors
///
/// Returns an error if `value` fails to serialize, contains unsupported CBOR
/// floats or tags, or has duplicate map keys.
pub fn marshal<T: Serialize>(domain: &str, value: &T) -> Result<Vec<u8>, Error> {
    let raw = Value::serialized(value).map_err(|err| Error::Serialize(err.to_string()))?;
    let canonical = canonicalize(raw)?;
    let mut out = Vec::with_capacity(domain.len() + 64);
    out.extend_from_slice(domain.as_bytes());
    ciborium::into_writer(&canonical, &mut out).map_err(|err| Error::Serialize(err.to_string()))?;
    Ok(out)
}

/// Computes SHA-256 over [`marshal(domain, value)`].
///
/// # Errors
///
/// Propagates any canonical CBOR encoding error.
pub fn hash<T: Serialize>(domain: &str, value: &T) -> Result<[u8; 32], Error> {
    let bytes = marshal(domain, value)?;
    Ok(Sha256::digest(&bytes).into())
}

fn canonicalize(value: Value) -> Result<Value, Error> {
    match value {
        Value::Integer(_) | Value::Bytes(_) | Value::Text(_) | Value::Bool(_) | Value::Null => {
            Ok(value)
        }
        Value::Float(_) => Err(Error::UnsupportedFloat),
        Value::Tag(_, _) => Err(Error::UnsupportedTag),
        Value::Array(items) => {
            let mut out = Vec::with_capacity(items.len());
            for item in items {
                out.push(canonicalize(item)?);
            }
            Ok(Value::Array(out))
        }
        Value::Map(entries) => {
            let mut encoded_entries: Vec<(Vec<u8>, Value, Value)> =
                Vec::with_capacity(entries.len());
            for (k, v) in entries {
                let ck = canonicalize(k)?;
                let cv = canonicalize(v)?;
                let mut key_bytes = Vec::new();
                ciborium::into_writer(&ck, &mut key_bytes)
                    .map_err(|err| Error::Serialize(err.to_string()))?;
                encoded_entries.push((key_bytes, ck, cv));
            }
            // RFC 8949 Section 4.2.1: keys in every map MUST be sorted in the
            // bytewise lexicographic order of their deterministic encodings.
            encoded_entries.sort_by(|a, b| a.0.cmp(&b.0));
            for pair in encoded_entries.windows(2) {
                if pair[0].0 == pair[1].0 {
                    return Err(Error::DuplicateMapKey);
                }
            }
            let sorted = encoded_entries
                .into_iter()
                .map(|(_, k, v)| (k, v))
                .collect();
            Ok(Value::Map(sorted))
        }
        _ => Err(Error::Serialize("unknown CBOR value variant".to_owned())),
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[derive(Serialize)]
    struct Sample<'a> {
        z_str: &'a str,
        a: u64,
        neg: i64,
        raw: Bytes<'a>,
        opt_present: OptBytes<'a>,
        opt_absent: OptBytes<'a>,
        flag: bool,
        list: &'a [&'a str],
    }

    #[test]
    fn canonical_cbor_matches_rfc8949_core_deterministic_bytes() {
        let sample = Sample {
            z_str: "hello",
            a: 42,
            neg: -7,
            raw: Bytes(&[0xde, 0xad, 0xbe, 0xef]),
            opt_present: OptBytes(Some(&[1, 2])),
            opt_absent: OptBytes(None),
            flag: true,
            list: &["alpha", "beta"],
        };
        let encoded = marshal("ocservia/test/v1\0", &sample).expect("marshal");
        assert!(encoded.starts_with(b"ocservia/test/v1\0"));
        let cbor_part = &encoded["ocservia/test/v1\0".len()..];
        // Verify deterministic key ordering:
        // "a" (len 1), "neg" (len 3), "raw" (len 3), "flag" (len 4), "list" (len 4),
        // "z_str" (len 5), "opt_absent" (len 10), "opt_present" (len 11).
        let decoded: Value = ciborium::from_reader(cbor_part).expect("decode");
        let Value::Map(entries) = decoded else {
            panic!("expected map");
        };
        let keys: Vec<&str> = entries
            .iter()
            .map(|(k, _)| k.as_text().expect("text key"))
            .collect();
        assert_eq!(
            keys,
            vec![
                "a",
                "neg",
                "raw",
                "flag",
                "list",
                "z_str",
                "opt_absent",
                "opt_present"
            ]
        );
        // Cross-language golden hex verified against Go fxamacker/cbor/v2 CoreDetEncOptions.
        let expected_hex = "a86161182a636e6567266372617744deadbeef64666c6167f5646c6973748265616c7068616462657461657a5f7374726568656c6c6f6a6f70745f616273656e74f66b6f70745f70726573656e74420102";
        assert_eq!(hex::encode(cbor_part), expected_hex);
    }
}
