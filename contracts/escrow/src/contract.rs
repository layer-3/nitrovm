use cosmwasm_std::{
    entry_point, to_json_binary, Binary, Deps, DepsMut, Env, MessageInfo,
    QueryRequest, Response, StdResult, Uint128, WasmMsg, WasmQuery,
};

use crate::error::ContractError;
use crate::msg::{
    EscrowInfoResponse, ExecuteMsg, InstantiateMsg, QueryMsg, StatusResponse,
    TokenBalanceResponse, TokenExecuteMsg, TokenQueryMsg,
};
use crate::state::{load_escrow, save_escrow, Escrow, Status};

#[entry_point]
pub fn instantiate(
    deps: DepsMut,
    env: Env,
    _info: MessageInfo,
    msg: InstantiateMsg,
) -> Result<Response, ContractError> {
    let expiry = msg.expiry_seconds.map(|s| {
        // env.block.time is in nanoseconds
        env.block.time.nanos() + s * 1_000_000_000
    });

    let escrow = Escrow {
        token_contract: msg.token_contract,
        buyer: None,
        seller: msg.seller,
        arbiter: msg.arbiter,
        amount: msg.amount,
        deposited: Uint128::zero(),
        expiry,
        status: Status::Open,
        escrow_code_id: msg.escrow_code_id,
    };
    save_escrow(deps.storage, &escrow)?;

    Ok(Response::new()
        .add_attribute("action", "instantiate_escrow")
        .add_attribute("amount", msg.amount)
        .add_attribute("expiry", expiry.map_or("none".to_string(), |e| e.to_string())))
}

#[entry_point]
pub fn execute(
    deps: DepsMut,
    env: Env,
    info: MessageInfo,
    msg: ExecuteMsg,
) -> Result<Response, ContractError> {
    match msg {
        ExecuteMsg::Deposit { amount } => execute_deposit(deps, info, amount),
        ExecuteMsg::Approve {} => execute_approve(deps, info),
        ExecuteMsg::Refund {} => execute_refund(deps, env, info),
        ExecuteMsg::CheckBalance {} => execute_check_balance(deps, env),
        ExecuteMsg::CloneEscrow {
            seller,
            arbiter,
            amount,
        } => execute_clone(deps, env, info, seller, arbiter, amount),
    }
}

fn execute_deposit(
    deps: DepsMut,
    info: MessageInfo,
    amount: Uint128,
) -> Result<Response, ContractError> {
    if amount.is_zero() {
        return Err(ContractError::InvalidZeroAmount {});
    }

    let mut escrow = load_escrow(deps.storage)?;
    if escrow.status != Status::Open {
        return Err(ContractError::AlreadyFinalized {
            status: escrow.status.to_string(),
        });
    }

    let new_total = escrow.deposited + amount;
    if new_total > escrow.amount {
        return Err(ContractError::DepositExceedsRequired {
            total: new_total.to_string(),
            required: escrow.amount.to_string(),
        });
    }

    escrow.deposited = new_total;
    // Record buyer on first deposit.
    if escrow.buyer.is_none() {
        escrow.buyer = Some(info.sender.to_string());
    }
    save_escrow(deps.storage, &escrow)?;

    Ok(Response::new()
        .add_attribute("action", "deposit")
        .add_attribute("buyer", info.sender)
        .add_attribute("amount", amount)
        .add_attribute("total_deposited", new_total))
}

fn execute_approve(
    deps: DepsMut,
    info: MessageInfo,
) -> Result<Response, ContractError> {
    let mut escrow = load_escrow(deps.storage)?;
    if escrow.status != Status::Open {
        return Err(ContractError::AlreadyFinalized {
            status: escrow.status.to_string(),
        });
    }

    // Only arbiter or buyer can approve.
    let sender = info.sender.to_string();
    let is_arbiter = sender == escrow.arbiter;
    let is_buyer = escrow.buyer.as_deref() == Some(&sender);
    if !is_arbiter && !is_buyer {
        return Err(ContractError::Unauthorized {
            expected: format!("arbiter({}) or buyer", escrow.arbiter),
        });
    }

    // Must be fully funded.
    if escrow.deposited < escrow.amount {
        return Err(ContractError::NotFunded {
            deposited: escrow.deposited.to_string(),
            required: escrow.amount.to_string(),
        });
    }

    escrow.status = Status::Released;
    save_escrow(deps.storage, &escrow)?;

    // Cross-contract execute: transfer tokens from escrow to seller via token contract.
    let transfer_msg = WasmMsg::Execute {
        contract_addr: escrow.token_contract.clone(),
        msg: to_json_binary(&TokenExecuteMsg::Transfer {
            recipient: escrow.seller.clone(),
            amount: escrow.deposited,
        })?,
        funds: vec![],
    };

    Ok(Response::new()
        .add_message(transfer_msg)
        .add_attribute("action", "approve")
        .add_attribute("seller", &escrow.seller)
        .add_attribute("amount", escrow.deposited))
}

fn execute_refund(
    deps: DepsMut,
    env: Env,
    info: MessageInfo,
) -> Result<Response, ContractError> {
    let mut escrow = load_escrow(deps.storage)?;
    if escrow.status != Status::Open {
        return Err(ContractError::AlreadyFinalized {
            status: escrow.status.to_string(),
        });
    }

    let sender = info.sender.to_string();
    let is_arbiter = sender == escrow.arbiter;

    // If not arbiter, must be expired for anyone to refund.
    if !is_arbiter {
        match escrow.expiry {
            Some(expiry) if env.block.time.nanos() >= expiry => {
                // Expired: anyone can trigger refund.
            }
            Some(expiry) => {
                return Err(ContractError::NotExpired {
                    current: env.block.time.nanos().to_string(),
                    expiry: expiry.to_string(),
                });
            }
            None => {
                return Err(ContractError::Unauthorized {
                    expected: format!("arbiter({})", escrow.arbiter),
                });
            }
        }
    }

    let buyer = match &escrow.buyer {
        Some(b) => b.clone(),
        None => {
            // No deposits made, just mark refunded.
            escrow.status = Status::Refunded;
            save_escrow(deps.storage, &escrow)?;
            return Ok(Response::new()
                .add_attribute("action", "refund")
                .add_attribute("amount", "0"));
        }
    };

    escrow.status = Status::Refunded;
    let refund_amount = escrow.deposited;
    save_escrow(deps.storage, &escrow)?;

    if refund_amount.is_zero() {
        return Ok(Response::new()
            .add_attribute("action", "refund")
            .add_attribute("amount", "0"));
    }

    // Cross-contract execute: transfer tokens back to buyer via token contract.
    let transfer_msg = WasmMsg::Execute {
        contract_addr: escrow.token_contract.clone(),
        msg: to_json_binary(&TokenExecuteMsg::Transfer {
            recipient: buyer.clone(),
            amount: refund_amount,
        })?,
        funds: vec![],
    };

    Ok(Response::new()
        .add_message(transfer_msg)
        .add_attribute("action", "refund")
        .add_attribute("buyer", buyer)
        .add_attribute("amount", refund_amount))
}

fn execute_check_balance(
    deps: DepsMut,
    env: Env,
) -> Result<Response, ContractError> {
    let escrow = load_escrow(deps.storage)?;

    // Cross-contract query: ask the token contract for this escrow's balance.
    let query_msg = QueryRequest::Wasm(WasmQuery::Smart {
        contract_addr: escrow.token_contract,
        msg: to_json_binary(&TokenQueryMsg::Balance {
            address: env.contract.address.to_string(),
        })?,
    });
    let balance_resp: TokenBalanceResponse = deps.querier.query(&query_msg)?;

    Ok(Response::new()
        .add_attribute("action", "check_balance")
        .add_attribute("token_balance", balance_resp.balance))
}

fn execute_clone(
    deps: DepsMut,
    _env: Env,
    _info: MessageInfo,
    seller: String,
    arbiter: String,
    amount: Uint128,
) -> Result<Response, ContractError> {
    let escrow = load_escrow(deps.storage)?;
    let code_id = escrow
        .escrow_code_id
        .ok_or_else(|| ContractError::Unauthorized {
            expected: "escrow_code_id must be set".to_string(),
        })?;

    // Sub-message: instantiate a new escrow contract.
    let init_msg = to_json_binary(&InstantiateMsg {
        token_contract: escrow.token_contract.clone(),
        seller,
        arbiter,
        amount,
        expiry_seconds: None,
        escrow_code_id: Some(code_id),
    })?;

    let instantiate_msg = WasmMsg::Instantiate {
        admin: None,
        code_id,
        msg: init_msg,
        funds: vec![],
        label: "escrow-clone".to_string(),
    };

    Ok(Response::new()
        .add_message(instantiate_msg)
        .add_attribute("action", "clone_escrow")
        .add_attribute("amount", amount))
}

#[entry_point]
pub fn query(deps: Deps, _env: Env, msg: QueryMsg) -> StdResult<Binary> {
    match msg {
        QueryMsg::EscrowInfo {} => {
            let escrow = load_escrow(deps.storage)?;
            to_json_binary(&EscrowInfoResponse {
                token_contract: escrow.token_contract,
                buyer: escrow.buyer,
                seller: escrow.seller,
                arbiter: escrow.arbiter,
                amount: escrow.amount,
                deposited: escrow.deposited,
                expiry: escrow.expiry,
                status: escrow.status.to_string(),
                escrow_code_id: escrow.escrow_code_id,
            })
        }
        QueryMsg::Status {} => {
            let escrow = load_escrow(deps.storage)?;
            to_json_binary(&StatusResponse {
                status: escrow.status.to_string(),
            })
        }
    }
}
