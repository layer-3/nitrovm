use cosmwasm_std::{
    entry_point, to_json_binary, Binary, Deps, DepsMut, Env, MessageInfo, Response, StdResult,
    Uint128,
};

use crate::error::ContractError;
use crate::msg::{
    BalanceResponse, ExecuteMsg, InstantiateMsg, QueryMsg, TokenInfoResponse,
};
use crate::state::{load_balance, load_token_info, save_balance, save_token_info, TokenInfo};

#[entry_point]
pub fn instantiate(
    deps: DepsMut,
    _env: Env,
    _info: MessageInfo,
    msg: InstantiateMsg,
) -> Result<Response, ContractError> {
    let mut total_supply = Uint128::zero();

    for bal in &msg.initial_balances {
        save_balance(deps.storage, &bal.address, bal.amount)?;
        total_supply += bal.amount;
    }

    save_token_info(
        deps.storage,
        &TokenInfo {
            name: msg.name,
            symbol: msg.symbol,
            decimals: msg.decimals,
            total_supply,
        },
    )?;

    Ok(Response::new()
        .add_attribute("action", "instantiate")
        .add_attribute("total_supply", total_supply))
}

#[entry_point]
pub fn execute(
    deps: DepsMut,
    _env: Env,
    info: MessageInfo,
    msg: ExecuteMsg,
) -> Result<Response, ContractError> {
    match msg {
        ExecuteMsg::Transfer { recipient, amount } => {
            execute_transfer(deps, info, recipient, amount)
        }
    }
}

fn execute_transfer(
    deps: DepsMut,
    info: MessageInfo,
    recipient: String,
    amount: Uint128,
) -> Result<Response, ContractError> {
    if amount.is_zero() {
        return Err(ContractError::InvalidZeroAmount {});
    }

    let sender = info.sender.to_string();

    let sender_balance = load_balance(deps.storage, &sender)?;
    if sender_balance < amount {
        return Err(ContractError::InsufficientFunds {
            available: sender_balance.to_string(),
            required: amount.to_string(),
        });
    }
    save_balance(deps.storage, &sender, sender_balance - amount)?;

    let recipient_balance = load_balance(deps.storage, &recipient)?;
    save_balance(deps.storage, &recipient, recipient_balance + amount)?;

    Ok(Response::new()
        .add_attribute("action", "transfer")
        .add_attribute("from", sender)
        .add_attribute("to", &recipient)
        .add_attribute("amount", amount))
}

#[entry_point]
pub fn query(deps: Deps, _env: Env, msg: QueryMsg) -> StdResult<Binary> {
    match msg {
        QueryMsg::Balance { address } => {
            let balance = load_balance(deps.storage, &address)?;
            to_json_binary(&BalanceResponse { balance })
        }
        QueryMsg::TokenInfo {} => {
            let info = load_token_info(deps.storage)?;
            to_json_binary(&TokenInfoResponse {
                name: info.name,
                symbol: info.symbol,
                decimals: info.decimals,
                total_supply: info.total_supply,
            })
        }
    }
}
