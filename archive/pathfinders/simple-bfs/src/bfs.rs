use alloy_primitives::{Address, B256, U256};
use std::collections::{HashMap, HashSet};
use std::sync::Arc;

use crate::overrides;
use crate::quoter::{
    self, BlockInfo, ExtraData, PoolsByToken, QuoteJob, RouteStep,
};
use crate::state::{RpcFetcher, StateSnapshot};

pub struct BfsResult {
    pub amount_out: U256,
    pub route: Vec<RouteStep>,
    pub gas_used: u64,
    pub evm_calls: usize,
    pub requote_calls: usize,
    pub fetched_storage: Vec<(Address, B256, U256)>,
    pub fetched_accounts: Vec<(Address, U256, B256, revm::bytecode::Bytecode)>,
}

pub fn bfs_route(
    rayon_pool: &rayon::ThreadPool,
    snapshot: &Arc<StateSnapshot>,
    rpc_fetcher: &Arc<RpcFetcher>,
    contract_code: &[u8],
    token_overrides: &HashMap<Address, overrides::TokenOverride>,
    pools_by_token: &PoolsByToken,
    token_in: Address,
    token_out: Address,
    input_amount: U256,
    input_avax: U256,
    max_hops: usize,
    block_info: &BlockInfo,
    gas_limit: u64,
    gas_aware: bool,
    requote: bool,
) -> BfsResult {
    let gas_aware = gas_aware && !input_avax.is_zero();
    let basefee = block_info.basefee;

    let mut frontier: HashMap<Address, (U256, u64)> = HashMap::new();
    frontier.insert(token_in, (input_amount, 0u64));

    let mut total_evm_calls = 0usize;
    let mut total_requote_calls = 0usize;
    let mut all_fetched_storage = Vec::new();
    let mut all_fetched_accounts = Vec::new();

    let mut backtrack: Vec<HashMap<Address, (Address, u8, Address, U256, U256, ExtraData)>> =
        Vec::with_capacity(max_hops);

    // (amount_out, gas_total, full_route)
    let mut best_at_layer: Vec<Option<(U256, u64, Vec<RouteStep>)>> =
        vec![None; max_hops];

    let reachable = build_reachability(pools_by_token, token_out, max_hops);

    for layer in 0..max_hops {
        let is_last_layer = layer == max_hops - 1;
        let remaining = max_hops - layer - 1;

        // Build jobs
        let mut jobs: Vec<QuoteJob> = Vec::new();
        for (token, (amount, _)) in &frontier {
            if let Some(edges) = pools_by_token.get(token) {
                for edge in edges {
                    // Same-pool reversal pruning
                    if layer > 0 {
                        if let Some((parent_pool, _, parent_token, _, _, _)) =
                            backtrack[layer - 1].get(token)
                        {
                            if edge.token_out == *parent_token && edge.pool == *parent_pool {
                                continue;
                            }
                        }
                    }
                    if edge.token_out == token_in && token_in != token_out {
                        continue;
                    }
                    if is_last_layer && edge.token_out != token_out {
                        continue;
                    }
                    if !is_last_layer && remaining > 0 {
                        if let Some(reachable_set) = reachable.get(remaining - 1) {
                            if !reachable_set.contains(&edge.token_out) {
                                continue;
                            }
                        }
                    }
                    jobs.push(QuoteJob {
                        pool: edge.pool,
                        pool_type: edge.pool_type,
                        token_in: edge.token_in,
                        token_out: edge.token_out,
                        amount: *amount,
                        extra_data: edge.extra_data,
                    });
                }
            }
        }

        if jobs.is_empty() {
            backtrack.push(HashMap::new());
            continue;
        }

        let mut next_frontier: HashMap<Address, (U256, u64)> = HashMap::new();
        let mut layer_backtrack: HashMap<Address, (Address, u8, Address, U256, U256, ExtraData)> =
            HashMap::new();
        let mut token_out_candidates: Vec<(U256, u64, Address, u8, Address, ExtraData)> =
            Vec::new();

        // Quote all edges
        {
            let (results, num_calls, fetched) = quoter::quote_all(
                rayon_pool, snapshot, rpc_fetcher, contract_code,
                token_overrides, &jobs, block_info, gas_limit,
            );
            total_evm_calls += num_calls;
            all_fetched_storage.extend(fetched.storage);
            all_fetched_accounts.extend(fetched.accounts);

            for (_, result) in &results {
                if result.amount_out.is_zero() {
                    continue;
                }
                let from_gas = frontier.get(&result.token_in).map_or(0u64, |&(_, g)| g);
                let path_gas = from_gas.saturating_add(result.gas_used);

                if result.token_out == token_out {
                    if layer == 0 {
                        // Layer 0: single-hop quotes are exact, score directly
                        let better = is_better(
                            &best_at_layer[layer], result.amount_out, path_gas,
                            gas_aware, input_avax, basefee,
                        );
                        if better {
                            best_at_layer[layer] = Some((
                                result.amount_out, path_gas,
                                vec![RouteStep {
                                    pool: result.pool, pool_type: result.pool_type,
                                    token_in: result.token_in, token_out,
                                    amount_in: result.amount, amount_out: result.amount_out,
                                    extra_data: result.extra_data,
                                }],
                            ));
                        }
                    } else {
                        // Layer >= 1: queue for full-path atomic quoting below
                        token_out_candidates.push((
                            result.amount_out, path_gas, result.pool,
                            result.pool_type, result.token_in, result.extra_data,
                        ));
                    }
                } else {
                    let better = match next_frontier.get(&result.token_out) {
                        None => true,
                        Some(&(old_amount, old_gas)) => {
                            if gas_aware {
                                quoter::is_better_net(
                                    result.amount_out, path_gas, old_amount, old_gas,
                                    input_avax, basefee,
                                )
                            } else {
                                result.amount_out > old_amount
                            }
                        }
                    };
                    if better {
                        next_frontier.insert(result.token_out, (result.amount_out, path_gas));
                        layer_backtrack.insert(
                            result.token_out,
                            (result.pool, result.pool_type, result.token_in,
                             result.amount, result.amount_out, result.extra_data),
                        );
                    }
                }
            }
        }

        backtrack.push(layer_backtrack);

        // FULL-PATH ATOMIC QUOTING (layer >= 1)
        //
        // For multi-hop routes, we quote the entire accumulated path atomically
        // via quoteRoute() instead of relying on single-hop estimates.
        // Single-hop quoteMulti above is used only as a FILTER to identify
        // which edges are worth exploring. The actual amounts come from here.
        //
        // Why: executing hops atomically means hop1 mutates pool state that
        // hop2 reads. Single-hop estimates ignore this, causing ~0.01% drift.
        //
        // TODO: This is a correctness-first approach. The proper fix is to
        // propagate state diffs through the route during BFS expansion,
        // avoiding the need to re-execute the full prefix for every new edge.
        // Current approach roughly doubles execution time.
        if layer > 0 {
            // Build all route jobs: both frontier entries and terminal candidates
            // are re-quoted in a single parallel batch.
            let mut all_route_jobs: Vec<(Vec<RouteStep>, U256)> = Vec::new();
            // Track which jobs are frontier vs terminal so we can dispatch results
            let mut frontier_tokens: Vec<Address> = Vec::new();
            let mut terminal_infos: Vec<(Address, u8, Address, ExtraData)> = Vec::new();

            // Frontier routes: reconstruct full path from tokenIn to each intermediate token
            for token in next_frontier.keys() {
                if let Some(route) = reconstruct_path(&backtrack, *token, token_in, input_amount) {
                    frontier_tokens.push(*token);
                    all_route_jobs.push((route, input_amount));
                }
            }

            let terminal_start = all_route_jobs.len();

            // Terminal routes: reconstruct full path from tokenIn through to token_out
            for &(_estimated_amount, _est_gas, pool, pool_type, from_token, extra_data) in
                &token_out_candidates
            {
                let prefix = if from_token == token_in {
                    Some(Vec::new())
                } else {
                    reconstruct_path(&backtrack, from_token, token_in, input_amount)
                };
                let route = match prefix {
                    Some(mut r) => {
                        r.push(RouteStep {
                            pool,
                            pool_type,
                            token_in: from_token,
                            token_out,
                            amount_in: U256::ZERO,
                            amount_out: U256::ZERO,
                            extra_data,
                        });
                        if let Some(first) = r.first_mut() {
                            first.amount_in = input_amount;
                        }
                        r
                    }
                    None => continue,
                };
                terminal_infos.push((pool, pool_type, from_token, extra_data));
                all_route_jobs.push((route, input_amount));
            }

            if !all_route_jobs.is_empty() {
                let (path_results, route_fetched) = quoter::quote_routes_parallel(
                    rayon_pool, snapshot, rpc_fetcher, contract_code,
                    token_overrides, &all_route_jobs, block_info, gas_limit,
                );
                total_requote_calls += path_results.len();
                all_fetched_storage.extend(route_fetched.storage);
                all_fetched_accounts.extend(route_fetched.accounts);

                // Apply frontier results: replace single-hop estimates with atomic amounts
                for (i, &(real_amount, real_gas)) in path_results[..terminal_start].iter().enumerate() {
                    let token = frontier_tokens[i];
                    if !real_amount.is_zero() {
                        next_frontier.insert(token, (real_amount, real_gas));
                    }
                }

                // Apply terminal results: score against best_at_layer using atomic amounts
                for (i, &(real_amount, real_gas)) in path_results[terminal_start..].iter().enumerate() {
                    if real_amount.is_zero() {
                        continue;
                    }
                    let better = is_better(
                        &best_at_layer[layer], real_amount, real_gas,
                        gas_aware, input_avax, basefee,
                    );
                    if better {
                        // Store the full route that was atomically executed
                        let full_route = all_route_jobs[terminal_start + i].0.clone();
                        best_at_layer[layer] = Some((real_amount, real_gas, full_route));
                    }
                }
            }
        }

        frontier = next_frontier
            .into_iter()
            .filter(|(_, (a, _))| *a > U256::ZERO)
            .collect();
    }

    // Pick best across all layers
    let mut best_layer: Option<usize> = None;
    let mut best_amount = U256::ZERO;
    let mut best_gas = 0u64;

    for (layer, candidate) in best_at_layer.iter().enumerate() {
        if let Some((amount_out, gas_total, _)) = candidate {
            let better = match best_layer {
                None => true,
                _ => {
                    if gas_aware {
                        quoter::is_better_net(*amount_out, *gas_total, best_amount, best_gas, input_avax, basefee)
                    } else {
                        *amount_out > best_amount
                    }
                }
            };
            if better {
                best_layer = Some(layer);
                best_amount = *amount_out;
                best_gas = *gas_total;
            }
        }
    }

    // Use the stored route directly — no reconstruction needed
    let route = if let Some(final_layer) = best_layer {
        best_at_layer[final_layer].as_ref().unwrap().2.clone()
    } else {
        Vec::new()
    };

    BfsResult {
        amount_out: best_amount,
        route,
        gas_used: best_gas,
        evm_calls: total_evm_calls,
        requote_calls: total_requote_calls,
        fetched_storage: all_fetched_storage,
        fetched_accounts: all_fetched_accounts,
    }
}

// ---- Helpers ----

fn is_better(
    current: &Option<(U256, u64, Vec<RouteStep>)>,
    new_amount: U256,
    new_gas: u64,
    gas_aware: bool,
    input_avax: U256,
    basefee: u64,
) -> bool {
    match current {
        None => true,
        Some((old_out, old_gas, _)) => {
            if gas_aware {
                quoter::is_better_net(new_amount, new_gas, *old_out, *old_gas, input_avax, basefee)
            } else {
                new_amount > *old_out
            }
        }
    }
}

fn build_reachability(
    pools_by_token: &PoolsByToken,
    token_out: Address,
    max_hops: usize,
) -> Vec<HashSet<Address>> {
    let mut reverse: HashMap<Address, HashSet<Address>> = HashMap::new();
    for edges in pools_by_token.values() {
        for e in edges {
            reverse.entry(e.token_out).or_default().insert(e.token_in);
        }
    }
    let mut layers: Vec<HashSet<Address>> = Vec::with_capacity(max_hops);
    let mut current_set = HashSet::new();
    current_set.insert(token_out);
    if let Some(sources) = reverse.get(&token_out) {
        for &t in sources {
            current_set.insert(t);
        }
    }
    layers.push(current_set.clone());
    for _ in 1..max_hops {
        let mut next_set = current_set.clone();
        for &t in &current_set {
            if let Some(sources) = reverse.get(&t) {
                for &s in sources {
                    next_set.insert(s);
                }
            }
        }
        current_set = next_set;
        layers.push(current_set.clone());
    }
    layers
}

fn reconstruct_path(
    backtrack: &[HashMap<Address, (Address, u8, Address, U256, U256, ExtraData)>],
    to_token: Address,
    token_in: Address,
    input_amount: U256,
) -> Option<Vec<RouteStep>> {
    let last_layer = backtrack.len().checked_sub(1)?;
    let entry = backtrack[last_layer].get(&to_token)?;
    let (pool, pool_type, from_token, amount_in, amount_out, extra_data) = *entry;

    let mut steps = Vec::new();
    steps.push(RouteStep {
        pool,
        pool_type,
        token_in: from_token,
        token_out: to_token,
        amount_in,
        amount_out,
        extra_data,
    });

    let mut current = from_token;
    for layer in (0..last_layer).rev() {
        if current == token_in {
            break;
        }
        let e = backtrack[layer].get(&current)?;
        let (p, pt, bt, ba_in, ba_out, bed) = *e;
        steps.push(RouteStep {
            pool: p,
            pool_type: pt,
            token_in: bt,
            token_out: current,
            amount_in: ba_in,
            amount_out: ba_out,
            extra_data: bed,
        });
        current = bt;
    }

    if current != token_in {
        return None;
    }

    steps.reverse();
    if let Some(first) = steps.first_mut() {
        first.amount_in = input_amount;
    }
    Some(steps)
}
