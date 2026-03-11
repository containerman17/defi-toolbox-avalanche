use alloy_primitives::{Address, B256, U256, keccak256};
use revm::bytecode::Bytecode;
use revm::database_interface::Database;
use revm::state::AccountInfo;
use std::collections::HashMap;
use std::convert::Infallible;
use std::sync::atomic::{AtomicU64, Ordering};
use std::sync::{Arc, Mutex};
use std::thread::JoinHandle;

const WS_THREADS: usize = 64;

// ---- WS stats counters ----
static WS_SENT: AtomicU64 = AtomicU64::new(0);
static WS_RESOLVED: AtomicU64 = AtomicU64::new(0);

// ---- Immutable state snapshot (lock-free reads) ----

#[derive(Clone)]
pub struct StateSnapshot {
    pub storage: HashMap<Address, HashMap<B256, B256>>,
    pub accounts: HashMap<Address, (U256, B256, Bytecode)>,
    pub code_by_hash: HashMap<B256, Bytecode>,
    pub block_num: u64,
    pub block_timestamp: u64,
    pub basefee: u64,
}

impl StateSnapshot {
    pub fn new(block_num: u64, block_timestamp: u64, basefee: u64) -> Self {
        StateSnapshot {
            storage: HashMap::new(),
            accounts: HashMap::new(),
            code_by_hash: HashMap::new(),
            block_num,
            block_timestamp,
            basefee,
        }
    }

    pub fn merge_fetched_storage(&mut self, fetched: Vec<(Address, B256, U256)>) {
        for (addr, slot, value) in fetched {
            self.storage
                .entry(addr)
                .or_default()
                .insert(slot, B256::from(value));
        }
    }

    pub fn merge_fetched_accounts(&mut self, fetched: Vec<(Address, U256, B256, Bytecode)>) {
        for (addr, balance, code_hash, bytecode) in fetched {
            self.code_by_hash
                .entry(code_hash)
                .or_insert_with(|| bytecode.clone());
            self.accounts.insert(addr, (balance, code_hash, bytecode));
        }
    }

    /// Save snapshot state to a binary file.
    pub fn save_to_file(&self, path: &str) -> std::io::Result<()> {
        use std::io::{Write, BufWriter};

        if let Some(parent) = std::path::Path::new(path).parent() {
            std::fs::create_dir_all(parent)?;
        }

        let mut w = BufWriter::new(std::fs::File::create(path)?);

        w.write_all(&self.block_num.to_be_bytes())?;
        w.write_all(&self.block_timestamp.to_be_bytes())?;
        w.write_all(&self.basefee.to_be_bytes())?;

        // Accounts
        w.write_all(&(self.accounts.len() as u32).to_be_bytes())?;
        for (addr, (balance, code_hash, bytecode)) in &self.accounts {
            w.write_all(addr.as_slice())?;
            w.write_all(&balance.to_be_bytes::<32>())?;
            w.write_all(code_hash.as_slice())?;
            let code = bytecode.original_bytes();
            w.write_all(&(code.len() as u32).to_be_bytes())?;
            w.write_all(&code)?;
        }

        // Storage
        w.write_all(&(self.storage.len() as u32).to_be_bytes())?;
        for (addr, slots) in &self.storage {
            w.write_all(addr.as_slice())?;
            w.write_all(&(slots.len() as u32).to_be_bytes())?;
            for (slot, value) in slots {
                w.write_all(slot.as_slice())?;
                w.write_all(value.as_slice())?;
            }
        }

        w.flush()?;
        Ok(())
    }

    /// Load snapshot state from a binary file.
    pub fn load_from_file(path: &str) -> std::io::Result<Self> {
        use std::io::{Read, BufReader};

        let mut r = BufReader::new(std::fs::File::open(path)?);

        let mut buf8 = [0u8; 8];
        let mut buf4 = [0u8; 4];
        let mut buf20 = [0u8; 20];
        let mut buf32 = [0u8; 32];

        r.read_exact(&mut buf8)?;
        let block_num = u64::from_be_bytes(buf8);
        r.read_exact(&mut buf8)?;
        let block_timestamp = u64::from_be_bytes(buf8);
        r.read_exact(&mut buf8)?;
        let basefee = u64::from_be_bytes(buf8);

        // Accounts
        r.read_exact(&mut buf4)?;
        let num_accounts = u32::from_be_bytes(buf4) as usize;
        let mut accounts = HashMap::with_capacity(num_accounts);
        let mut code_by_hash = HashMap::new();

        for _ in 0..num_accounts {
            r.read_exact(&mut buf20)?;
            let addr = Address::from(buf20);
            r.read_exact(&mut buf32)?;
            let balance = U256::from_be_bytes(buf32);
            r.read_exact(&mut buf32)?;
            let code_hash = B256::from(buf32);
            r.read_exact(&mut buf4)?;
            let code_len = u32::from_be_bytes(buf4) as usize;
            let mut code_bytes = vec![0u8; code_len];
            r.read_exact(&mut code_bytes)?;
            let bytecode = Bytecode::new_raw(revm::primitives::Bytes::from(code_bytes));
            code_by_hash.entry(code_hash).or_insert_with(|| bytecode.clone());
            accounts.insert(addr, (balance, code_hash, bytecode));
        }

        // Storage
        r.read_exact(&mut buf4)?;
        let num_storage_addrs = u32::from_be_bytes(buf4) as usize;
        let mut storage = HashMap::with_capacity(num_storage_addrs);

        for _ in 0..num_storage_addrs {
            r.read_exact(&mut buf20)?;
            let addr = Address::from(buf20);
            r.read_exact(&mut buf4)?;
            let num_slots = u32::from_be_bytes(buf4) as usize;
            let mut slots = HashMap::with_capacity(num_slots);
            for _ in 0..num_slots {
                r.read_exact(&mut buf32)?;
                let slot = B256::from(buf32);
                r.read_exact(&mut buf32)?;
                let value = B256::from(buf32);
                slots.insert(slot, value);
            }
            storage.insert(addr, slots);
        }

        Ok(StateSnapshot {
            storage,
            accounts,
            code_by_hash,
            block_num,
            block_timestamp,
            basefee,
        })
    }

    /// Load from the proxy's JSON cache format:
    /// {"block":"0x...", "contracts":{"0xaddr":{"code":"0x...","balance":"0x...","storage":{"0xslot":"0xval",...}}}}
    pub fn load_from_proxy_cache(path: &str) -> std::io::Result<Self> {
        use std::io::Read;
        let mut file = std::fs::File::open(path)?;
        let mut data = String::new();
        file.read_to_string(&mut data)?;

        let json: serde_json::Value = serde_json::from_str(&data)
            .map_err(|e| std::io::Error::new(std::io::ErrorKind::InvalidData, e))?;

        let block_hex = json["block"].as_str().unwrap_or("0x0");
        let block_num = u64::from_str_radix(block_hex.strip_prefix("0x").unwrap_or(block_hex), 16).unwrap_or(0);

        let mut accounts = HashMap::new();
        let mut code_by_hash = HashMap::new();
        let mut storage = HashMap::new();

        if let Some(contracts) = json["contracts"].as_object() {
            for (addr_str, contract) in contracts {
                let addr: Address = addr_str.parse().unwrap_or_default();

                // Code + balance → account
                let balance = contract["balance"].as_str()
                    .and_then(|s| {
                        let clean = s.strip_prefix("0x").unwrap_or(s);
                        if clean.is_empty() { Some(U256::ZERO) } else { U256::from_str_radix(clean, 16).ok() }
                    })
                    .unwrap_or(U256::ZERO);

                let code_hex = contract["code"].as_str().unwrap_or("0x");
                let code_clean = code_hex.strip_prefix("0x").unwrap_or(code_hex);
                let code_bytes = hex::decode(code_clean).unwrap_or_default();
                let code_hash = keccak256(&code_bytes);
                let bytecode = Bytecode::new_raw(revm::primitives::Bytes::from(code_bytes));

                code_by_hash.entry(code_hash).or_insert_with(|| bytecode.clone());
                accounts.insert(addr, (balance, code_hash, bytecode));

                // Storage slots
                if let Some(slots) = contract["storage"].as_object() {
                    let mut slot_map = HashMap::with_capacity(slots.len());
                    for (slot_str, val) in slots {
                        let slot_clean = slot_str.strip_prefix("0x").unwrap_or(slot_str);
                        let val_str = val.as_str().unwrap_or("0x0");
                        let val_clean = val_str.strip_prefix("0x").unwrap_or(val_str);

                        if let (Ok(slot_bytes), Ok(val_bytes)) = (
                            hex::decode(format!("{:0>64}", slot_clean)),
                            hex::decode(format!("{:0>64}", val_clean)),
                        ) {
                            let slot = B256::from_slice(&slot_bytes);
                            let value = B256::from_slice(&val_bytes);
                            slot_map.insert(slot, value);
                        }
                    }
                    if !slot_map.is_empty() {
                        storage.insert(addr, slot_map);
                    }
                }
            }
        }

        Ok(StateSnapshot {
            storage,
            accounts,
            code_by_hash,
            block_num,
            block_timestamp: 0,
            basefee: 0,
        })
    }

    pub fn apply_storage_diffs(&mut self, diffs: HashMap<Address, HashMap<B256, B256>>) -> usize {
        let mut changed = 0;
        for (addr, slots) in diffs {
            let entry = self.storage.entry(addr).or_default();
            for (slot, value) in slots {
                entry.insert(slot, value);
                changed += 1;
            }
        }
        changed
    }
}

// ---- WebSocket RPC transport ----

struct WsRequest {
    body: serde_json::Value,
    reply_tx: std::sync::mpsc::SyncSender<serde_json::Value>,
}

fn ws_connect(ws_url: &str) -> tungstenite::WebSocket<tungstenite::stream::MaybeTlsStream<std::net::TcpStream>> {
    use tungstenite::client::IntoClientRequest;
    let backoffs = [100, 500, 1000, 2000, 5000];
    for (attempt, &ms) in backoffs.iter().enumerate() {
        match tungstenite::connect(ws_url.into_client_request().unwrap()) {
            Ok((ws, _)) => return ws,
            Err(e) => {
                if attempt == backoffs.len() - 1 {
                    panic!("WS connect failed after {} attempts: {}", attempt + 1, e);
                }
                eprintln!("WS connect attempt {} failed: {}, retrying in {}ms", attempt + 1, e, ms);
                std::thread::sleep(std::time::Duration::from_millis(ms));
            }
        }
    }
    unreachable!()
}

fn ws_thread_main(ws_url: String, rx: Arc<std::sync::Mutex<std::sync::mpsc::Receiver<WsRequest>>>) {
    use tungstenite::Message;

    let mut ws = ws_connect(&ws_url);

    loop {
        let req = {
            let guard = rx.lock().unwrap();
            match guard.recv() {
                Ok(r) => r,
                Err(_) => return,
            }
        };

        loop {
            match ws.send(Message::Text(req.body.to_string().into())) {
                Ok(_) => {
                    WS_SENT.fetch_add(1, Ordering::Relaxed);
                    break;
                }
                Err(e) => {
                    eprintln!("WS send error: {}, reconnecting", e);
                    ws = ws_connect(&ws_url);
                }
            }
        }

        loop {
            match ws.read() {
                Ok(Message::Text(text)) => {
                    if let Ok(json) = serde_json::from_str::<serde_json::Value>(&text) {
                        let _ = req.reply_tx.send(json);
                        WS_RESOLVED.fetch_add(1, Ordering::Relaxed);
                    }
                    break;
                }
                Ok(_) => {}
                Err(e) => {
                    eprintln!("WS read error: {}, reconnecting and resending", e);
                    ws = ws_connect(&ws_url);
                    let _ = ws.send(Message::Text(req.body.to_string().into()));
                }
            }
        }
    }
}

// ---- RPC fetcher (cold path only) ----

pub struct RpcFetcher {
    request_tx: std::sync::mpsc::SyncSender<WsRequest>,
    _ws_threads: Vec<JoinHandle<()>>,
    block_hex: Mutex<String>,
}

impl RpcFetcher {
    pub fn new(ws_url: &str, block_num: u64) -> Self {
        let (tx, rx) = std::sync::mpsc::sync_channel::<WsRequest>(8192);
        let rx = Arc::new(std::sync::Mutex::new(rx));

        let mut handles = Vec::with_capacity(WS_THREADS);
        for i in 0..WS_THREADS {
            let rx = Arc::clone(&rx);
            let url = ws_url.to_string();
            let h = std::thread::Builder::new()
                .name(format!("ws-rpc-{}", i))
                .spawn(move || ws_thread_main(url, rx))
                .expect("failed to spawn WS thread");
            handles.push(h);
        }

        // Stats thread
        std::thread::Builder::new()
            .name("ws-stats".into())
            .spawn(|| {
                let mut prev_sent = 0u64;
                let mut prev_resolved = 0u64;
                loop {
                    std::thread::sleep(std::time::Duration::from_secs(10));
                    let sent = WS_SENT.load(Ordering::Relaxed);
                    let resolved = WS_RESOLVED.load(Ordering::Relaxed);
                    let ds = sent - prev_sent;
                    let dr = resolved - prev_resolved;
                    let inflight = sent.saturating_sub(resolved);
                    if ds > 0 || dr > 0 || inflight > 0 {
                        eprintln!("[WS] sent={}/s  resolved={}/s  inflight={}  (total sent={}  resolved={})", ds / 10, dr / 10, inflight, sent, resolved);
                    }
                    prev_sent = sent;
                    prev_resolved = resolved;
                }
            })
            .expect("failed to spawn WS stats thread");

        RpcFetcher {
            request_tx: tx,
            _ws_threads: handles,
            block_hex: Mutex::new(format!("0x{:x}", block_num)),
        }
    }

    pub fn set_block_hex(&self, block_num: u64) {
        *self.block_hex.lock().unwrap() = format!("0x{:x}", block_num);
    }

    fn rpc_call(&self, body: serde_json::Value, label: &str) -> serde_json::Value {
        let delays_ms = [10, 20, 50, 100, 200, 300, 320];
        for attempt in 0..=delays_ms.len() {
            let (reply_tx, reply_rx) = std::sync::mpsc::sync_channel(1);
            let req = WsRequest {
                body: body.clone(),
                reply_tx,
            };

            self.request_tx
                .send(req)
                .unwrap_or_else(|_| panic!("WS thread died while sending {}", label));

            let resp = match reply_rx.recv_timeout(std::time::Duration::from_millis(100)) {
                Ok(r) => r,
                Err(_) => {
                    eprintln!("[RPC] {} timeout (100ms) attempt {}/{}", label, attempt + 1, delays_ms.len());
                    if attempt == delays_ms.len() {
                        panic!("[RPC] {} timed out after all retries", label);
                    }
                    std::thread::sleep(std::time::Duration::from_millis(delays_ms[attempt]));
                    continue;
                }
            };

            if resp.get("error").is_none() || attempt == delays_ms.len() {
                return resp;
            }

            eprintln!("[RPC] {} retry {}/{}: {}", label, attempt + 1, delays_ms.len(), resp["error"]);
            std::thread::sleep(std::time::Duration::from_millis(delays_ms[attempt]));
        }
        unreachable!()
    }

    pub fn fetch_storage(&self, addr: Address, slot: B256) -> U256 {
        let block_hex = self.block_hex.lock().unwrap().clone();

        let body = serde_json::json!({
            "jsonrpc": "2.0",
            "method": "eth_getStorageAt",
            "params": [format!("{:?}", addr), format!("0x{}", hex::encode(slot.as_slice())), &block_hex],
            "id": 1
        });

        let resp = self.rpc_call(body, "eth_getStorageAt");
        let hex_str = resp["result"]
            .as_str()
            .unwrap_or_else(|| panic!("RPC eth_getStorageAt missing result: {}", resp));
        parse_hex_u256(hex_str)
            .unwrap_or_else(|| panic!("RPC eth_getStorageAt bad hex: {}", hex_str))
    }

    pub fn fetch_account(&self, addr: Address) -> (U256, B256, Bytecode) {
        let block_hex = self.block_hex.lock().unwrap().clone();

        let bal_body = serde_json::json!({
            "jsonrpc": "2.0",
            "method": "eth_getBalance",
            "params": [format!("{:?}", addr), &block_hex],
            "id": 1
        });
        let bal_resp = self.rpc_call(bal_body, "eth_getBalance");
        let bal_hex_str = bal_resp["result"]
            .as_str()
            .unwrap_or_else(|| panic!("RPC eth_getBalance missing result: {}", bal_resp));
        let balance = parse_hex_u256(bal_hex_str)
            .unwrap_or_else(|| panic!("RPC eth_getBalance bad hex: {}", bal_hex_str));

        let code_body = serde_json::json!({
            "jsonrpc": "2.0",
            "method": "eth_getCode",
            "params": [format!("{:?}", addr), &block_hex],
            "id": 2
        });
        let code_resp = self.rpc_call(code_body, "eth_getCode");
        let code_hex = code_resp["result"]
            .as_str()
            .unwrap_or_else(|| panic!("RPC eth_getCode missing result: {}", code_resp));
        let code_clean = code_hex.strip_prefix("0x").unwrap_or(code_hex);
        let code_bytes = hex::decode(code_clean)
            .unwrap_or_else(|e| panic!("RPC eth_getCode bad hex for {:?}: {}", addr, e));
        let code_hash = keccak256(&code_bytes);
        let bytecode = Bytecode::new_raw(revm::primitives::Bytes::from(code_bytes));

        (balance, code_hash, bytecode)
    }

    pub fn get_block_number(&self) -> u64 {
        let body = serde_json::json!({
            "jsonrpc": "2.0",
            "method": "eth_blockNumber",
            "params": [],
            "id": 1
        });
        let resp = self.rpc_call(body, "eth_blockNumber");
        let hex_str = resp["result"]
            .as_str()
            .unwrap_or_else(|| panic!("eth_blockNumber missing result: {}", resp));
        let clean = hex_str.strip_prefix("0x").unwrap_or(hex_str);
        u64::from_str_radix(clean, 16).unwrap_or(0)
    }

    pub fn get_block_info(&self, block_num: u64) -> (u64, u64, u64, u64) {
        let body = serde_json::json!({
            "jsonrpc": "2.0",
            "method": "eth_getBlockByNumber",
            "params": [format!("0x{:x}", block_num), false],
            "id": 1
        });
        let resp = self.rpc_call(body, "eth_getBlockByNumber");
        let block = &resp["result"];

        let number = parse_hex_u64(block["number"].as_str().unwrap_or("0x0"));
        let timestamp = parse_hex_u64(block["timestamp"].as_str().unwrap_or("0x0"));
        let basefee = parse_hex_u64(block["baseFeePerGas"].as_str().unwrap_or("0x0"));
        let gas_limit = parse_hex_u64(block["gasLimit"].as_str().unwrap_or("0x0"));

        (number, timestamp, basefee, gas_limit)
    }
}

fn parse_hex_u256(hex_str: &str) -> Option<U256> {
    let hex_clean = hex_str.strip_prefix("0x").unwrap_or(hex_str);
    if hex_clean.is_empty() || hex_clean == "0" {
        return Some(U256::ZERO);
    }
    U256::from_str_radix(hex_clean, 16).ok()
}

fn parse_hex_u64(hex_str: &str) -> u64 {
    let clean = hex_str.strip_prefix("0x").unwrap_or(hex_str);
    u64::from_str_radix(clean, 16).unwrap_or(0)
}

// ---- Per-thread EVM database ----

pub struct SnapDB {
    pub snapshot: Arc<StateSnapshot>,
    pub rpc_fetcher: Arc<RpcFetcher>,
    pub storage_overrides: HashMap<Address, HashMap<B256, B256>>,
    pub balance_overrides: HashMap<Address, U256>,
    pub code_overrides: HashMap<Address, (B256, Bytecode)>,
    pub fetched_storage: Vec<(Address, B256, U256)>,
    pub fetched_accounts: Vec<(Address, U256, B256, Bytecode)>,
    pub cold_miss_count: u32,
}

impl SnapDB {
    pub fn new(snapshot: Arc<StateSnapshot>, rpc_fetcher: Arc<RpcFetcher>) -> Self {
        SnapDB {
            snapshot,
            rpc_fetcher,
            storage_overrides: HashMap::new(),
            balance_overrides: HashMap::new(),
            code_overrides: HashMap::new(),
            fetched_storage: Vec::new(),
            fetched_accounts: Vec::new(),
            cold_miss_count: 0,
        }
    }

    pub fn set_storage_override(&mut self, addr: Address, slot: B256, value: B256) {
        self.storage_overrides
            .entry(addr)
            .or_default()
            .insert(slot, value);
    }

    pub fn set_balance_override(&mut self, addr: Address, balance: U256) {
        self.balance_overrides.insert(addr, balance);
    }

    pub fn set_code_override_precomputed(
        &mut self,
        addr: Address,
        hash: B256,
        bytecode: Bytecode,
    ) {
        self.code_overrides.insert(addr, (hash, bytecode));
    }
}

impl Database for SnapDB {
    type Error = Infallible;

    fn basic(&mut self, address: Address) -> Result<Option<AccountInfo>, Self::Error> {
        if let Some((hash, bytecode)) = self.code_overrides.get(&address) {
            let balance = self
                .balance_overrides
                .get(&address)
                .copied()
                .unwrap_or(U256::ZERO);
            return Ok(Some(AccountInfo::new(balance, 0, *hash, bytecode.clone())));
        }

        if let Some((balance, code_hash, bytecode)) = self.snapshot.accounts.get(&address) {
            let balance = self
                .balance_overrides
                .get(&address)
                .copied()
                .unwrap_or(*balance);
            return Ok(Some(AccountInfo::new(
                balance,
                0,
                *code_hash,
                bytecode.clone(),
            )));
        }

        let (rpc_balance, code_hash, bytecode) = self.rpc_fetcher.fetch_account(address);
        self.cold_miss_count += 1;
        self.fetched_accounts
            .push((address, rpc_balance, code_hash, bytecode.clone()));
        let balance = self
            .balance_overrides
            .get(&address)
            .copied()
            .unwrap_or(rpc_balance);
        Ok(Some(AccountInfo::new(balance, 0, code_hash, bytecode)))
    }

    fn code_by_hash(&mut self, code_hash: B256) -> Result<Bytecode, Self::Error> {
        if let Some((_, bytecode)) = self
            .code_overrides
            .values()
            .find(|(h, _)| *h == code_hash)
        {
            return Ok(bytecode.clone());
        }
        Ok(self
            .snapshot
            .code_by_hash
            .get(&code_hash)
            .cloned()
            .unwrap_or_default())
    }

    fn storage(&mut self, address: Address, index: U256) -> Result<U256, Self::Error> {
        let slot = B256::from(index);

        if let Some(slots) = self.storage_overrides.get(&address) {
            if let Some(value) = slots.get(&slot) {
                return Ok(U256::from_be_bytes(value.0));
            }
        }

        if let Some(slots) = self.snapshot.storage.get(&address) {
            if let Some(value) = slots.get(&slot) {
                return Ok(U256::from_be_bytes(value.0));
            }
        }

        let value = self.rpc_fetcher.fetch_storage(address, slot);
        self.cold_miss_count += 1;
        self.fetched_storage.push((address, slot, value));
        Ok(value)
    }

    fn block_hash(&mut self, _number: u64) -> Result<B256, Self::Error> {
        Ok(B256::ZERO)
    }
}
