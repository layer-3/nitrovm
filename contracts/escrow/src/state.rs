use cosmwasm_std::{from_json, to_json_vec, StdResult, Storage, Uint128};
use serde::{Deserialize, Serialize};

#[derive(Serialize, Deserialize, Clone, Debug, PartialEq)]
pub enum Status {
    Open,
    Released,
    Refunded,
}

impl std::fmt::Display for Status {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            Status::Open => write!(f, "open"),
            Status::Released => write!(f, "released"),
            Status::Refunded => write!(f, "refunded"),
        }
    }
}

#[derive(Serialize, Deserialize, Clone, Debug)]
pub struct Escrow {
    pub token_contract: String,
    pub buyer: Option<String>,
    pub seller: String,
    pub arbiter: String,
    pub amount: Uint128,
    pub deposited: Uint128,
    pub expiry: Option<u64>,
    pub status: Status,
    pub escrow_code_id: Option<u64>,
}

const ESCROW_KEY: &[u8] = b"escrow";

pub fn save_escrow(storage: &mut dyn Storage, escrow: &Escrow) -> StdResult<()> {
    storage.set(ESCROW_KEY, &to_json_vec(escrow)?);
    Ok(())
}

pub fn load_escrow(storage: &dyn Storage) -> StdResult<Escrow> {
    let data = storage
        .get(ESCROW_KEY)
        .ok_or_else(|| cosmwasm_std::StdError::not_found("escrow"))?;
    from_json(data)
}
