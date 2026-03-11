use alloy_primitives::{Address, B256, U256, keccak256};
use serde::Deserialize;
use std::collections::HashMap;

/// Token override configuration for computing balance/allowance storage slots.
#[derive(Clone)]
pub struct TokenOverride {
    /// Standard mapping: balance at slot N
    pub slot: i32,
    /// Allowance slot. -1 means use slot+1.
    pub allowance_slot: i32,
    /// ERC-7201: keccak256(encode(address, base))
    pub base: B256,
    pub allowance_base: B256,
    /// Bit shift for packed storage (e.g., bool + uint248)
    pub shift: u32,
}

impl TokenOverride {
    fn is_erc7201(&self) -> bool {
        self.base != B256::ZERO
    }
}

/// JSON entry format matching token_overrides.json
#[derive(Deserialize)]
struct JsonEntry {
    address: String,
    #[serde(default)]
    slot: i32,
    #[serde(default)]
    allowance_slot: Option<i32>,
    #[serde(default)]
    erc7201_base: Option<String>,
    #[serde(default)]
    erc7201_allowance: Option<String>,
    #[serde(default)]
    shift: u32,
}

/// Compute storage overrides (balance + allowance slots) for a token.
/// Returns Vec of (address, slot, value) triples to set on SnapDB.
pub fn get_override(
    token: Address,
    account: Address,
    balance: U256,
    spender: Address,
    tokens: &HashMap<Address, TokenOverride>,
) -> Vec<(Address, B256, B256)> {
    let ovr = match tokens.get(&token) {
        Some(o) => o,
        None => return vec![],
    };

    let mut result = Vec::with_capacity(2);

    // Balance slot
    let balance_slot = compute_balance_slot(account, ovr);
    let balance_value = if ovr.shift > 0 {
        balance << ovr.shift as usize
    } else {
        balance
    };
    result.push((token, balance_slot, B256::from(balance_value)));

    // Allowance slot
    let allowance_slot = compute_allowance_slot(account, spender, ovr);
    result.push((token, allowance_slot, B256::from(balance)));

    result
}

fn compute_balance_slot(account: Address, ovr: &TokenOverride) -> B256 {
    if ovr.is_erc7201() {
        keccak256_address_hash(account, ovr.base)
    } else {
        keccak256_address_uint(account, ovr.slot as u64)
    }
}

fn compute_allowance_slot(owner: Address, spender: Address, ovr: &TokenOverride) -> B256 {
    let inner_hash = if ovr.is_erc7201() {
        let base = if ovr.allowance_base != B256::ZERO {
            ovr.allowance_base
        } else {
            ovr.base
        };
        keccak256_address_hash(owner, base)
    } else {
        let slot = if ovr.allowance_slot >= 0 {
            ovr.allowance_slot as u64
        } else {
            (ovr.slot + 1) as u64
        };
        keccak256_address_uint(owner, slot)
    };
    keccak256_address_hash(spender, inner_hash)
}

/// keccak256(abi.encode(address, uint256))
fn keccak256_address_uint(addr: Address, slot: u64) -> B256 {
    let mut data = [0u8; 64];
    data[12..32].copy_from_slice(addr.as_slice());
    data[56..64].copy_from_slice(&slot.to_be_bytes());
    keccak256(data)
}

/// keccak256(abi.encode(address, bytes32))
fn keccak256_address_hash(addr: Address, hash: B256) -> B256 {
    let mut data = [0u8; 64];
    data[12..32].copy_from_slice(addr.as_slice());
    data[32..64].copy_from_slice(hash.as_slice());
    keccak256(data)
}

fn parse_b256(hex_str: &str) -> B256 {
    let clean = hex_str.strip_prefix("0x").unwrap_or(hex_str);
    let bytes = hex::decode(clean).expect("invalid hex in JSON");
    let mut buf = [0u8; 32];
    let start = 32 - bytes.len();
    buf[start..].copy_from_slice(&bytes);
    B256::from(buf)
}

pub fn build_token_overrides(json_path: &str) -> HashMap<Address, TokenOverride> {
    let data = std::fs::read_to_string(json_path)
        .unwrap_or_else(|e| panic!("read {}: {}", json_path, e));
    let entries: Vec<JsonEntry> = serde_json::from_str(&data)
        .unwrap_or_else(|e| panic!("parse {}: {}", json_path, e));

    let mut m = HashMap::with_capacity(entries.len());
    for entry in entries {
        let addr: Address = entry.address.parse()
            .unwrap_or_else(|_| panic!("invalid address: {}", entry.address));

        let ovr = if let Some(ref base_hex) = entry.erc7201_base {
            let base = parse_b256(base_hex);
            let allowance_base = entry.erc7201_allowance
                .as_deref()
                .map(parse_b256)
                .unwrap_or(B256::ZERO);
            TokenOverride {
                slot: 0,
                allowance_slot: -1,
                base,
                allowance_base,
                shift: entry.shift,
            }
        } else {
            TokenOverride {
                slot: entry.slot,
                allowance_slot: entry.allowance_slot.map(|s| s as i32).unwrap_or(-1),
                base: B256::ZERO,
                allowance_base: B256::ZERO,
                shift: 0,
            }
        };

        m.insert(addr, ovr);
    }

    eprintln!("loaded {} token overrides from {}", m.len(), json_path);
    m
}
