use alloy_primitives::{Address, B256, Bytes, U256, address};
use rayon::prelude::*;
use revm::context::TxEnv;
use revm::handler::ExecuteEvm;
use revm::primitives::TxKind;
use revm::{Context, MainBuilder, MainContext};
use revm::primitives::hardfork::SpecId;
use std::collections::HashMap;
use std::sync::Arc;

use crate::overrides;
use crate::state::{RpcFetcher, SnapDB, StateSnapshot};

/// Fake address where MultiQuoter bytecode is injected.
const QUOTER_ADDR: Address = address!("00000000000000000000000000000000DEADBEEF");
const DUMMY_CALLER: Address = address!("000000000000000000000000000000000000dEaD");

/// Pool type constant for Uniswap V4

// ---- Types ----

#[derive(Clone)]
pub struct BlockInfo {
    pub number: u64,
    pub timestamp: u64,
    pub basefee: u64,
    pub gas_limit: u64,
    pub difficulty: U256,
    pub coinbase: Address,
}

impl BlockInfo {
    pub fn new(number: u64, timestamp: u64, basefee: u64, gas_limit: u64) -> Self {
        BlockInfo {
            number,
            timestamp,
            basefee,
            gas_limit,
            difficulty: U256::ZERO,
            coinbase: Address::ZERO,
        }
    }
}

#[derive(Clone, Copy, Default)]
pub struct ExtraData {
    pub fee: u32,
    pub tick_spacing: i32,
    pub hooks: Address,
    // Balancer V3 Buffered (type 11)
    pub wrapped_in: Address,
    pub buf_pool: Address,
    pub wrapped_out: Address,
}

impl ExtraData {
    pub fn is_empty(&self) -> bool {
        self.fee == 0 && self.tick_spacing == 0 && self.hooks == Address::ZERO
            && self.wrapped_in == Address::ZERO
    }
}

#[derive(Clone, Copy)]
pub struct PoolEdge {
    pub pool: Address,
    pub pool_type: u8,
    pub token_in: Address,
    pub token_out: Address,
    pub extra_data: ExtraData,
    pub rank: u32,
}

#[derive(Clone)]
pub struct QuoteJob {
    pub pool: Address,
    pub pool_type: u8,
    pub token_in: Address,
    pub token_out: Address,
    pub amount: U256,
    pub extra_data: ExtraData,
}

pub struct QuoteResult {
    pub pool: Address,
    pub pool_type: u8,
    pub token_in: Address,
    pub token_out: Address,
    pub amount: U256,
    pub amount_out: U256,
    pub gas_used: u64,
    pub extra_data: ExtraData,
}

#[derive(Clone, Copy)]
pub struct RouteStep {
    pub pool: Address,
    pub pool_type: u8,
    pub token_in: Address,
    pub token_out: Address,
    pub amount_in: U256,
    pub amount_out: U256,
    pub extra_data: ExtraData,
}

#[derive(Hash, Eq, PartialEq, Clone)]
pub struct QuoteKey {
    pub pool: Address,
    pub token_in: Address,
    pub token_out: Address,
}

pub type PoolsByToken = HashMap<Address, Vec<PoolEdge>>;

pub struct FetchedKeys {
    pub storage: Vec<(Address, B256, U256)>,
    pub accounts: Vec<(Address, U256, B256, revm::bytecode::Bytecode)>,
}

// ---- Parsing ----

pub fn parse_extra_data_str(s: &str) -> ExtraData {
    let mut ed = ExtraData::default();
    for part in s.split(',') {
        if let Some(v) = part.strip_prefix("fee=") {
            ed.fee = v.parse().unwrap_or(0);
        } else if let Some(v) = part.strip_prefix("ts=") {
            ed.tick_spacing = v.parse().unwrap_or(0);
        } else if let Some(v) = part.strip_prefix("hooks=") {
            ed.hooks = v.parse().unwrap_or(Address::ZERO);
        } else if let Some(v) = part.strip_prefix("wi=") {
            ed.wrapped_in = v.parse().unwrap_or(Address::ZERO);
        } else if let Some(v) = part.strip_prefix("bp=") {
            ed.buf_pool = v.parse().unwrap_or(Address::ZERO);
        } else if let Some(v) = part.strip_prefix("wo=") {
            ed.wrapped_out = v.parse().unwrap_or(Address::ZERO);
        }
    }
    ed
}

pub fn parse_pools(content: &str) -> Vec<PoolEdge> {
    let mut edges = Vec::new();
    let mut count: u32 = 0;

    for line in content.lines() {
        let line = line.trim();
        if line.is_empty() || line.chars().all(|c| c.is_ascii_digit()) {
            continue;
        }

        let parts: Vec<&str> = line.split(':').collect();
        if parts.len() < 5 {
            continue;
        }

        let pool = match parts[0].parse::<Address>() {
            Ok(a) => a,
            Err(_) => continue,
        };
        let pool_type: u8 = parts[2].parse().unwrap_or(0);
        let mut tokens: Vec<Address> = Vec::new();
        let mut extra_data = ExtraData::default();
        for s in &parts[4..] {
            if let Some(ed) = s.strip_prefix('@') {
                extra_data = parse_extra_data_str(ed);
            } else if let Ok(addr) = s.parse::<Address>() {
                tokens.push(addr);
            }
        }

        for i in 0..tokens.len() {
            for j in 0..tokens.len() {
                if i != j {
                    edges.push(PoolEdge {
                        pool,
                        pool_type,
                        token_in: tokens[i],
                        token_out: tokens[j],
                        extra_data,
                        rank: count,
                    });
                }
            }
        }

        count += 1;
    }

    edges
}

pub fn build_pools_by_token(edges: &[PoolEdge]) -> PoolsByToken {
    let mut map: PoolsByToken = HashMap::new();
    for e in edges {
        map.entry(e.token_in).or_default().push(*e);
    }
    map
}

// ---- ABI Encoding ----

/// Encode a call to swap(address[],uint8[],address[],uint256).
/// Used for both single-hop (1-element arrays) and multi-hop (N-element arrays).
fn encode_swap(pools: &[Address], pool_types: &[u8], tokens: &[Address], amount_in: U256) -> Bytes {
    let selector = &alloy_primitives::keccak256(b"swap(address[],uint8[],address[],uint256)")[..4];
    let n_pools = pools.len();
    let n_types = pool_types.len();
    let n_tokens = tokens.len();

    let mut data = Vec::with_capacity(4 + 32 * 32);
    data.extend_from_slice(selector);

    // Head: 4 params → 3 dynamic offsets + 1 value
    let head_size = 4 * 32;
    let pools_body = 32 + n_pools * 32;
    let types_body = 32 + n_types * 32;

    let pools_offset = head_size;
    let types_offset = pools_offset + pools_body;
    let tokens_offset = types_offset + types_body;

    data.extend_from_slice(&U256::from(pools_offset).to_be_bytes::<32>());
    data.extend_from_slice(&U256::from(types_offset).to_be_bytes::<32>());
    data.extend_from_slice(&U256::from(tokens_offset).to_be_bytes::<32>());
    data.extend_from_slice(&amount_in.to_be_bytes::<32>());

    // Pools array
    data.extend_from_slice(&U256::from(n_pools).to_be_bytes::<32>());
    for p in pools {
        let mut buf = [0u8; 32];
        buf[12..].copy_from_slice(p.as_slice());
        data.extend_from_slice(&buf);
    }

    // Pool types array
    data.extend_from_slice(&U256::from(n_types).to_be_bytes::<32>());
    for &t in pool_types {
        let mut buf = [0u8; 32];
        buf[31] = t;
        data.extend_from_slice(&buf);
    }

    // Tokens array
    data.extend_from_slice(&U256::from(n_tokens).to_be_bytes::<32>());
    for t in tokens {
        let mut buf = [0u8; 32];
        buf[12..].copy_from_slice(t.as_slice());
        data.extend_from_slice(&buf);
    }

    Bytes::from(data)
}

// ---- Pool quoting ----

struct ThreadLocal {
    contract_hash: alloy_primitives::B256,
    contract_bytecode: revm::bytecode::Bytecode,
    huge_balance: U256,
    whale_eth: U256,
}

fn execute_quote_job_fast(
    snapshot: &Arc<StateSnapshot>,
    rpc_fetcher: &Arc<RpcFetcher>,
    tl: &ThreadLocal,
    token_overrides: &HashMap<Address, overrides::TokenOverride>,
    job: &QuoteJob,
    block_info: &BlockInfo,
    gas_limit: u64,
) -> (QuoteResult, Vec<(Address, B256, U256)>, Vec<(Address, U256, B256, revm::bytecode::Bytecode)>) {
    let ovrs = overrides::get_override(job.token_in, QUOTER_ADDR, tl.huge_balance, QUOTER_ADDR, token_overrides);

    let mut db = SnapDB::new(Arc::clone(snapshot), Arc::clone(rpc_fetcher));
    db.set_code_override_precomputed(QUOTER_ADDR, tl.contract_hash, tl.contract_bytecode.clone());
    db.set_balance_override(DUMMY_CALLER, tl.whale_eth);
    for (addr, slot, value) in &ovrs {
        db.set_storage_override(*addr, *slot, *value);
    }

    // Single-hop swap call
    let calldata = encode_swap(
        &[job.pool], &[job.pool_type],
        &[job.token_in, job.token_out], job.amount,
    );

    let tx = TxEnv::builder()
        .caller(DUMMY_CALLER)
        .kind(TxKind::Call(QUOTER_ADDR))
        .data(calldata)
        .value(U256::ZERO)
        .gas_limit(gas_limit)
        .gas_price(0u128)
        .build()
        .expect("valid tx");

    let mut ctx = Context::mainnet().with_db(db);
    ctx.cfg.set_spec_and_mainnet_gas_params(SpecId::CANCUN);
    ctx.cfg.disable_base_fee = true;
    ctx.block.number = U256::from(block_info.number);
    ctx.block.timestamp = U256::from(block_info.timestamp);
    ctx.block.basefee = block_info.basefee;
    ctx.block.gas_limit = block_info.gas_limit;
    ctx.block.difficulty = block_info.difficulty;
    ctx.block.beneficiary = block_info.coinbase;
    let mut evm = ctx.build_mainnet();

    let (amount_out, gas_used) = match evm.transact_one(tx) {
        Ok(exec_result) => {
            let gas = exec_result.gas_used();
            if exec_result.is_success() {
                let output = exec_result.output().cloned().unwrap_or_default();
                if output.len() >= 32 {
                    (U256::from_be_slice(&output[..32]), gas)
                } else {
                    (U256::ZERO, gas)
                }
            } else {
                (U256::ZERO, gas)
            }
        }
        Err(_) => (U256::ZERO, 0),
    };

    let cold_misses = evm.ctx.journaled_state.database.cold_miss_count;
    let (fetched_storage, fetched_accounts) = if cold_misses > 0 {
        let db = evm.ctx.journaled_state.database;
        (db.fetched_storage, db.fetched_accounts)
    } else {
        (Vec::new(), Vec::new())
    };

    let result = QuoteResult {
        pool: job.pool,
        pool_type: job.pool_type,
        token_in: job.token_in,
        token_out: job.token_out,
        amount: job.amount,
        amount_out,
        gas_used,
        extra_data: job.extra_data,
    };

    (result, fetched_storage, fetched_accounts)
}

pub fn quote_all(
    pool: &rayon::ThreadPool,
    snapshot: &Arc<StateSnapshot>,
    rpc_fetcher: &Arc<RpcFetcher>,
    contract_code: &[u8],
    token_overrides: &HashMap<Address, overrides::TokenOverride>,
    jobs: &[QuoteJob],
    block_info: &BlockInfo,
    gas_limit: u64,
) -> (HashMap<QuoteKey, QuoteResult>, usize, FetchedKeys) {
    let contract_hash = alloy_primitives::keccak256(contract_code);
    let raw_bytes: revm::primitives::Bytes = revm::primitives::Bytes::copy_from_slice(contract_code);

    let results: Vec<(QuoteResult, Vec<(Address, B256, U256)>, Vec<(Address, U256, B256, revm::bytecode::Bytecode)>)> = pool.install(|| {
        jobs.par_iter()
            .map_init(
                || {
                    ThreadLocal {
                        contract_hash,
                        contract_bytecode: revm::bytecode::Bytecode::new_raw(raw_bytes.clone()),
                        huge_balance: U256::from(10u64).pow(U256::from(30)),
                        whale_eth: U256::from(1_000_000_000_000_000_000_000u128),
                    }
                },
                |tl, job| {
                    execute_quote_job_fast(snapshot, rpc_fetcher, tl, token_overrides, job, block_info, gas_limit)
                },
            )
            .collect()
    });

    let total = results.len();
    let mut map = HashMap::with_capacity(total);
    let mut all_fetched_storage = Vec::new();
    let mut all_fetched_accounts = Vec::new();

    for (r, fs, fa) in results {
        all_fetched_storage.extend(fs);
        all_fetched_accounts.extend(fa);
        if !r.amount_out.is_zero() {
            let key = QuoteKey {
                pool: r.pool,
                token_in: r.token_in,
                token_out: r.token_out,
            };
            map.insert(key, r);
        }
    }

    (map, total, FetchedKeys { storage: all_fetched_storage, accounts: all_fetched_accounts })
}

// ---- Gas-aware comparison ----

pub fn is_better_net(
    new_amount: U256, new_gas: u64,
    old_amount: U256, old_gas: u64,
    input_avax: U256, basefee: u64,
) -> bool {
    let max_amt = std::cmp::max(new_amount, old_amount);
    let basefee_u = U256::from(basefee);
    let val_new = new_amount * input_avax;
    let cost_new = U256::from(new_gas) * basefee_u * max_amt;
    let val_old = old_amount * input_avax;
    let cost_old = U256::from(old_gas) * basefee_u * max_amt;
    val_new.saturating_sub(cost_new) > val_old.saturating_sub(cost_old)
}

// ---- Full route execution ----

fn execute_route_with_fetched(
    snapshot: &Arc<StateSnapshot>,
    rpc_fetcher: &Arc<RpcFetcher>,
    tl: &ThreadLocal,
    token_overrides: &HashMap<Address, overrides::TokenOverride>,
    route: &[RouteStep],
    amount_in: U256,
    block_info: &BlockInfo,
    gas_limit: u64,
) -> (U256, u64, Vec<(Address, B256, U256)>, Vec<(Address, U256, B256, revm::bytecode::Bytecode)>) {
    if route.is_empty() {
        return (U256::ZERO, 0, vec![], vec![]);
    }

    let token_in = route[0].token_in;
    let ovrs = overrides::get_override(token_in, QUOTER_ADDR, tl.huge_balance, QUOTER_ADDR, token_overrides);

    let mut db = SnapDB::new(Arc::clone(snapshot), Arc::clone(rpc_fetcher));
    db.set_code_override_precomputed(QUOTER_ADDR, tl.contract_hash, tl.contract_bytecode.clone());
    db.set_balance_override(DUMMY_CALLER, tl.whale_eth);
    for (addr, slot, value) in &ovrs {
        db.set_storage_override(*addr, *slot, *value);
    }

    let pools: Vec<Address> = route.iter().map(|s| s.pool).collect();
    let pool_types_vec: Vec<u8> = route.iter().map(|s| s.pool_type).collect();
    let mut tokens: Vec<Address> = Vec::with_capacity(route.len() + 1);
    for s in route {
        tokens.push(s.token_in);
    }
    tokens.push(route.last().unwrap().token_out);

    let calldata = encode_swap(&pools, &pool_types_vec, &tokens, amount_in);

    let tx = TxEnv::builder()
        .caller(DUMMY_CALLER)
        .kind(TxKind::Call(QUOTER_ADDR))
        .data(calldata)
        .value(U256::ZERO)
        .gas_limit(gas_limit)
        .gas_price(0u128)
        .build()
        .expect("valid tx");

    let mut ctx = Context::mainnet().with_db(db);
    ctx.cfg.set_spec_and_mainnet_gas_params(SpecId::CANCUN);
    ctx.cfg.disable_base_fee = true;
    ctx.block.number = U256::from(block_info.number);
    ctx.block.timestamp = U256::from(block_info.timestamp);
    ctx.block.basefee = block_info.basefee;
    ctx.block.gas_limit = block_info.gas_limit;
    ctx.block.difficulty = block_info.difficulty;
    ctx.block.beneficiary = block_info.coinbase;
    let mut evm = ctx.build_mainnet();

    let (amount_out, gas) = match evm.transact_one(tx) {
        Ok(exec_result) => {
            let gas = exec_result.gas_used();
            if exec_result.is_success() {
                let output = exec_result.output().cloned().unwrap_or_default();
                if output.len() >= 32 {
                    let amt = U256::from_be_slice(&output[..32]);
                    (amt, gas)
                } else {
                    (U256::ZERO, gas)
                }
            } else {
                (U256::ZERO, gas)
            }
        }
        Err(e) => {
                eprintln!("[quoter] transact error: {:?}", e);
                (U256::ZERO, 0)
            }
    };

    let cold_misses = evm.ctx.journaled_state.database.cold_miss_count;
    let (fetched_storage, fetched_accounts) = if cold_misses > 0 {
        let db = evm.ctx.journaled_state.database;
        (db.fetched_storage, db.fetched_accounts)
    } else {
        (Vec::new(), Vec::new())
    };

    (amount_out, gas, fetched_storage, fetched_accounts)
}

pub fn execute_route_at_amount(
    snapshot: &Arc<StateSnapshot>,
    rpc_fetcher: &Arc<RpcFetcher>,
    tl: &ThreadLocal,
    token_overrides: &HashMap<Address, overrides::TokenOverride>,
    route: &[RouteStep],
    amount_in: U256,
    block_info: &BlockInfo,
    gas_limit: u64,
) -> (U256, u64) {
    let (amount_out, gas, _, _) = execute_route_with_fetched(
        snapshot, rpc_fetcher, tl, token_overrides, route, amount_in, block_info, gas_limit,
    );
    (amount_out, gas)
}

pub fn quote_routes_parallel(
    pool: &rayon::ThreadPool,
    snapshot: &Arc<StateSnapshot>,
    rpc_fetcher: &Arc<RpcFetcher>,
    contract_code: &[u8],
    token_overrides: &HashMap<Address, overrides::TokenOverride>,
    routes: &[(Vec<RouteStep>, U256)],
    block_info: &BlockInfo,
    gas_limit: u64,
) -> (Vec<(U256, u64)>, FetchedKeys) {
    let contract_hash = alloy_primitives::keccak256(contract_code);
    let raw_bytes: revm::primitives::Bytes = revm::primitives::Bytes::copy_from_slice(contract_code);

    let results: Vec<_> = pool.install(|| {
        routes.par_iter()
            .map_init(
                || ThreadLocal {
                    contract_hash,
                    contract_bytecode: revm::bytecode::Bytecode::new_raw(raw_bytes.clone()),
                    huge_balance: U256::from(10u64).pow(U256::from(30)),
                    whale_eth: U256::from(1_000_000_000_000_000_000_000u128),
                },
                |tl, (route, amount_in)| {
                    execute_route_with_fetched(
                        snapshot, rpc_fetcher, tl, token_overrides, route, *amount_in, block_info, gas_limit,
                    )
                },
            )
            .collect()
    });

    let mut all_storage = Vec::new();
    let mut all_accounts = Vec::new();
    let mut quote_results = Vec::with_capacity(results.len());
    for (amount_out, gas, fs, fa) in results {
        all_storage.extend(fs);
        all_accounts.extend(fa);
        quote_results.push((amount_out, gas));
    }
    (quote_results, FetchedKeys { storage: all_storage, accounts: all_accounts })
}
