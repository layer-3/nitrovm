use cosmwasm_std::StdError;
use thiserror::Error;

#[derive(Error, Debug)]
pub enum ContractError {
    #[error("{0}")]
    Std(#[from] StdError),

    #[error("insufficient funds: have {available}, need {required}")]
    InsufficientFunds { available: String, required: String },

    #[error("invalid zero amount")]
    InvalidZeroAmount {},
}
