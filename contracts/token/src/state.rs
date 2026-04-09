use cosmwasm_std::{from_json, to_json_vec, StdResult, Storage, Uint128};
use serde::{Deserialize, Serialize};

#[derive(Serialize, Deserialize, Clone, Debug)]
pub struct TokenInfo {
    pub name: String,
    pub symbol: String,
    pub decimals: u8,
    pub total_supply: Uint128,
}

const TOKEN_INFO_KEY: &[u8] = b"token_info";
const BALANCE_PREFIX: &[u8] = b"bal:";

pub fn save_token_info(storage: &mut dyn Storage, info: &TokenInfo) -> StdResult<()> {
    storage.set(TOKEN_INFO_KEY, &to_json_vec(info)?);
    Ok(())
}

pub fn load_token_info(storage: &dyn Storage) -> StdResult<TokenInfo> {
    let data = storage
        .get(TOKEN_INFO_KEY)
        .ok_or_else(|| cosmwasm_std::StdError::not_found("token_info"))?;
    from_json(data)
}

fn balance_key(address: &str) -> Vec<u8> {
    let mut key = BALANCE_PREFIX.to_vec();
    key.extend_from_slice(address.as_bytes());
    key
}

pub fn save_balance(storage: &mut dyn Storage, address: &str, amount: Uint128) -> StdResult<()> {
    storage.set(&balance_key(address), &to_json_vec(&amount)?);
    Ok(())
}

pub fn load_balance(storage: &dyn Storage, address: &str) -> StdResult<Uint128> {
    match storage.get(&balance_key(address)) {
        Some(data) => from_json(data),
        None => Ok(Uint128::zero()),
    }
}
