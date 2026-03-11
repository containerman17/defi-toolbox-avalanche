#[global_allocator]
static GLOBAL: tikv_jemallocator::Jemalloc = tikv_jemallocator::Jemalloc;

mod bfs;
mod overrides;
mod quoter;
mod state;

use alloy_primitives::{Address, U256};
use std::io::{self, BufRead, Write};
use std::sync::Arc;
use std::time::Instant;

fn parse_u256(s: &str) -> U256 {
    U256::from_str_radix(
        s.strip_prefix("0x").unwrap_or(s),
        if s.starts_with("0x") { 16 } else { 10 },
    )
    .unwrap_or(U256::ZERO)
}

fn main() {
    let args: Vec<String> = std::env::args().collect();
    if args.len() < 2 || args[1] != "quote" {
        eprintln!("usage: quote quote <token_overrides_path> [block_number]");
        std::process::exit(1);
    }

    let token_overrides_path = args.get(2)
        .expect("usage: quote quote <token_overrides_path> [block_number]");

    let ws_url = std::env::var("WS_RPC_URL")
        .unwrap_or_else(|_| "ws://127.0.0.1:9650/ext/bc/C/ws".to_string());

    let token_overrides = overrides::build_token_overrides(token_overrides_path);

    // Load quoter bytecode
    let manifest_dir = env!("CARGO_MANIFEST_DIR");
    let bytecode_path = format!("{}/data/quoter_bytecode.hex", manifest_dir);
    let bytecode_hex = std::fs::read_to_string(&bytecode_path)
        .unwrap_or_else(|e| panic!("read bytecode: {}", e));
    let bytecode_clean = bytecode_hex.trim().strip_prefix("0x").unwrap_or(bytecode_hex.trim());
    let contract_code = hex::decode(bytecode_clean)
        .unwrap_or_else(|e| panic!("bad bytecode hex: {}", e));
    eprintln!("loaded quoter bytecode ({} bytes)", contract_code.len());

    // Create RPC fetcher
    let rpc_fetcher = Arc::new(state::RpcFetcher::new(&ws_url, 0));

    // Determine block number
    let block_num = if let Some(bn_str) = args.get(3) {
        bn_str.parse::<u64>().expect("invalid block number")
    } else {
        rpc_fetcher.get_block_number()
    };
    rpc_fetcher.set_block_hex(block_num);

    // Fetch block info
    let (number, timestamp, basefee, gas_limit) = rpc_fetcher.get_block_info(block_num);
    let mut block_info = quoter::BlockInfo::new(number, timestamp, basefee, gas_limit);
    eprintln!("block {} (basefee: {} gwei, gas_limit: {})", number, basefee / 1_000_000_000, gas_limit);

    // Create rayon pool
    let rayon_pool = rayon::ThreadPoolBuilder::new()
        .build()
        .expect("failed to build rayon pool");

    // State cache: prefer PROXY_CACHE (proxy's JSON format), fall back to STATE_CACHE_DIR (binary)
    let proxy_cache_path = std::env::var("PROXY_CACHE").ok();
    let state_cache_dir = std::env::var("STATE_CACHE_DIR").ok();

    let mut snapshot = if let Some(ref path) = proxy_cache_path {
        if std::path::Path::new(path).exists() {
            let load_start = Instant::now();
            match state::StateSnapshot::load_from_proxy_cache(path) {
                Ok(mut snap) => {
                    snap.block_timestamp = timestamp;
                    snap.basefee = basefee;
                    eprintln!("[cache] loaded proxy cache from {} ({} accounts, {} storage addrs) in {:.0}ms",
                        path, snap.accounts.len(), snap.storage.len(), load_start.elapsed().as_millis());
                    Arc::new(snap)
                }
                Err(e) => {
                    eprintln!("[cache] failed to load proxy cache {}: {}, starting fresh", path, e);
                    Arc::new(state::StateSnapshot::new(number, timestamp, basefee))
                }
            }
        } else {
            eprintln!("[cache] proxy cache file not found: {}", path);
            Arc::new(state::StateSnapshot::new(number, timestamp, basefee))
        }
    } else if let Some(ref cache_dir) = state_cache_dir {
        let cache_path = format!("{}/{}.bin", cache_dir, number);
        if std::path::Path::new(&cache_path).exists() {
            match state::StateSnapshot::load_from_file(&cache_path) {
                Ok(snap) => {
                    eprintln!("[cache] loaded block {} from {} ({} accounts, {} storage addrs)",
                        number, cache_path, snap.accounts.len(), snap.storage.len());
                    Arc::new(snap)
                }
                Err(e) => {
                    eprintln!("[cache] failed to load {}: {}, starting fresh", cache_path, e);
                    Arc::new(state::StateSnapshot::new(number, timestamp, basefee))
                }
            }
        } else {
            eprintln!("[cache] no cache file for block {}", number);
            Arc::new(state::StateSnapshot::new(number, timestamp, basefee))
        }
    } else {
        Arc::new(state::StateSnapshot::new(number, timestamp, basefee))
    };
    let mut _last_block = number;

    // Send ready
    let ready = serde_json::json!({ "op": "ready", "block": number });
    println!("{}", ready);
    io::stdout().flush().unwrap();

    // Main loop
    let stdin = io::stdin();
    for line in stdin.lock().lines() {
        let line = match line {
            Ok(l) => l,
            Err(_) => break,
        };
        let line = line.trim().to_string();
        if line.is_empty() {
            continue;
        }

        let msg: serde_json::Value = match serde_json::from_str(&line) {
            Ok(v) => v,
            Err(e) => {
                let err = serde_json::json!({ "op": "error", "msg": format!("invalid json: {}", e) });
                println!("{}", err);
                io::stdout().flush().unwrap();
                continue;
            }
        };

        let op = msg["op"].as_str().unwrap_or("");

        match op {
            "quote_with_pools" => {
                let id = msg["id"].as_u64().unwrap_or(0);
                let start = Instant::now();

                // Parse pools
                let pools_str = msg["pools"].as_str().unwrap_or("");
                let edges = quoter::parse_pools(pools_str);
                let pools_by_token = quoter::build_pools_by_token(&edges);
                let parse_ms = start.elapsed().as_micros() as f64 / 1000.0;

                // Parse tokens and amount
                let token_in: Address = match msg["token_in"].as_str().and_then(|s| s.parse().ok()) {
                    Some(a) => a,
                    None => {
                        let err = serde_json::json!({ "op": "error", "id": id, "msg": "invalid token_in" });
                        println!("{}", err);
                        io::stdout().flush().unwrap();
                        continue;
                    }
                };
                let token_out: Address = match msg["token_out"].as_str().and_then(|s| s.parse().ok()) {
                    Some(a) => a,
                    None => {
                        let err = serde_json::json!({ "op": "error", "id": id, "msg": "invalid token_out" });
                        println!("{}", err);
                        io::stdout().flush().unwrap();
                        continue;
                    }
                };
                let amount_in = parse_u256(msg["amount"].as_str().unwrap_or("0"));
                let input_avax = parse_u256(msg["input_avax"].as_str().unwrap_or("0"));
                let max_hops = msg["max_hops"].as_u64().unwrap_or(3) as usize;
                let quote_gas_limit = msg["gas_limit"].as_u64().unwrap_or(2_000_000);
                let gas_aware = !input_avax.is_zero();
                let requote = msg["requote"].as_bool().unwrap_or(true);

                let bfs_start = Instant::now();
                let result = bfs::bfs_route(
                    &rayon_pool, &snapshot, &rpc_fetcher, &contract_code,
                    &token_overrides, &pools_by_token, token_in, token_out,
                    amount_in, input_avax, max_hops, &block_info, quote_gas_limit, gas_aware, requote,
                );
                let bfs_ms = bfs_start.elapsed().as_micros() as f64 / 1000.0;

                let elapsed_ms = start.elapsed().as_millis();
                eprintln!("  timing: parse={:.1}ms bfs={:.1}ms total={}ms", parse_ms, bfs_ms, elapsed_ms);

                // Merge fetched data into snapshot
                if !result.fetched_storage.is_empty() || !result.fetched_accounts.is_empty() {
                    let mut new_snap = (*snapshot).clone();
                    new_snap.merge_fetched_storage(result.fetched_storage);
                    new_snap.merge_fetched_accounts(result.fetched_accounts);
                    snapshot = Arc::new(new_snap);

                    // Save to cache
                    if let Some(ref cache_dir) = state_cache_dir {
                        let cache_path = format!("{}/{}.bin", cache_dir, snapshot.block_num);
                        match snapshot.save_to_file(&cache_path) {
                            Ok(()) => eprintln!("[cache] saved block {} to {} ({} accounts, {} storage addrs)",
                                snapshot.block_num, cache_path, snapshot.accounts.len(), snapshot.storage.len()),
                            Err(e) => eprintln!("[cache] failed to save {}: {}", cache_path, e),
                        }
                    }
                }

                let route_json: Vec<serde_json::Value> = result.route.iter().map(|step| {
                    serde_json::json!({
                        "pool": format!("{:?}", step.pool),
                        "pool_type": step.pool_type,
                        "token_in": format!("{:?}", step.token_in),
                        "token_out": format!("{:?}", step.token_out),
                        "amount_in": step.amount_in.to_string(),
                        "amount_out": step.amount_out.to_string(),
                    })
                }).collect();

                let resp = serde_json::json!({
                    "op": "result",
                    "id": id,
                    "amount_out": result.amount_out.to_string(),
                    "gas": result.gas_used,
                    "evm_calls": result.evm_calls,
                    "requote_calls": result.requote_calls,
                    "hops": result.route.len(),
                    "route": route_json,
                    "elapsed_ms": elapsed_ms,
                });
                println!("{}", resp);
                io::stdout().flush().unwrap();
            }

            "set_block" => {
                let id = msg["id"].as_u64().unwrap_or(0);
                let num = msg["block"].as_u64().unwrap_or(0);
                if num > 0 {
                    rpc_fetcher.set_block_hex(num);

                    let (number, timestamp, basefee, gas_limit) = rpc_fetcher.get_block_info(num);
                    block_info = quoter::BlockInfo::new(number, timestamp, basefee, gas_limit);

                    // Try loading from cache, otherwise create empty snapshot
                    snapshot = if let Some(ref cache_dir) = state_cache_dir {
                        let cache_path = format!("{}/{}.bin", cache_dir, number);
                        if std::path::Path::new(&cache_path).exists() {
                            match state::StateSnapshot::load_from_file(&cache_path) {
                                Ok(snap) => {
                                    eprintln!("[cache] loaded block {} from {} ({} accounts, {} storage addrs)",
                                        number, cache_path, snap.accounts.len(), snap.storage.len());
                                    Arc::new(snap)
                                }
                                Err(e) => {
                                    eprintln!("[cache] failed to load {}: {}, starting fresh", cache_path, e);
                                    Arc::new(state::StateSnapshot::new(number, timestamp, basefee))
                                }
                            }
                        } else {
                            eprintln!("[cache] no cache file for block {}", number);
                            Arc::new(state::StateSnapshot::new(number, timestamp, basefee))
                        }
                    } else {
                        Arc::new(state::StateSnapshot::new(number, timestamp, basefee))
                    };
                    _last_block = num;

                    let resp = serde_json::json!({
                        "op": "result",
                        "id": id,
                        "number": number,
                        "basefee": basefee,
                    });
                    println!("{}", resp);
                    io::stdout().flush().unwrap();
                }
            }

            _ => {
                let err = serde_json::json!({ "op": "error", "msg": format!("unknown op: {}", op) });
                println!("{}", err);
                io::stdout().flush().unwrap();
            }
        }
    }
}
