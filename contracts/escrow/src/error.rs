use cosmwasm_std::StdError;
use thiserror::Error;

#[derive(Error, Debug)]
pub enum ContractError {
    #[error("{0}")]
    Std(#[from] StdError),

    #[error("unauthorized: only {expected} can perform this action")]
    Unauthorized { expected: String },

    #[error("escrow already finalized with status: {status}")]
    AlreadyFinalized { status: String },

    #[error("escrow not funded: deposited {deposited}, required {required}")]
    NotFunded { deposited: String, required: String },

    #[error("escrow not expired: current {current}, expiry {expiry}")]
    NotExpired { current: String, expiry: String },

    #[error("invalid zero amount")]
    InvalidZeroAmount {},

    #[error("deposit exceeds required amount: would have {total}, max {required}")]
    DepositExceedsRequired { total: String, required: String },
}
