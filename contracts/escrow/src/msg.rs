use cosmwasm_schema::{cw_serde, QueryResponses};
use cosmwasm_std::Uint128;

#[cw_serde]
pub struct InstantiateMsg {
    /// Address of the deployed token contract for cross-contract calls.
    pub token_contract: String,
    /// Seller who receives tokens on approval.
    pub seller: String,
    /// Arbiter who can approve or refund.
    pub arbiter: String,
    /// Required deposit amount.
    pub amount: Uint128,
    /// Optional expiry in seconds from instantiation (uses env.block.time).
    pub expiry_seconds: Option<u64>,
    /// Code ID of this escrow contract (needed for CloneEscrow).
    pub escrow_code_id: Option<u64>,
}

#[cw_serde]
pub enum ExecuteMsg {
    /// Buyer calls to record a deposit. The buyer must first transfer
    /// tokens to the escrow contract address via the token contract,
    /// then call Deposit to record it.
    Deposit { amount: Uint128 },
    /// Arbiter or buyer approves release to seller.
    /// Emits WasmMsg::Execute to token contract for the transfer.
    Approve {},
    /// Arbiter can refund anytime. Anyone can refund after expiry.
    /// Emits WasmMsg::Execute to token contract for the transfer back to buyer.
    Refund {},
    /// Query the token contract for this escrow's balance (tests WasmQuery::Smart).
    /// Emits the result as an event attribute.
    CheckBalance {},
    /// Spawn a new escrow via WasmMsg::Instantiate (tests sub-message instantiate).
    CloneEscrow {
        seller: String,
        arbiter: String,
        amount: Uint128,
    },
}

#[cw_serde]
#[derive(QueryResponses)]
pub enum QueryMsg {
    #[returns(EscrowInfoResponse)]
    EscrowInfo {},
    #[returns(StatusResponse)]
    Status {},
}

#[cw_serde]
pub struct EscrowInfoResponse {
    pub token_contract: String,
    pub buyer: Option<String>,
    pub seller: String,
    pub arbiter: String,
    pub amount: Uint128,
    pub deposited: Uint128,
    pub expiry: Option<u64>,
    pub status: String,
    pub escrow_code_id: Option<u64>,
}

#[cw_serde]
pub struct StatusResponse {
    pub status: String,
}

/// Token contract execute message (matches the token contract's ExecuteMsg).
#[cw_serde]
pub enum TokenExecuteMsg {
    Transfer {
        recipient: String,
        amount: Uint128,
    },
}

/// Token contract query message (matches the token contract's QueryMsg).
#[cw_serde]
pub enum TokenQueryMsg {
    Balance { address: String },
}

/// Token contract balance response.
#[cw_serde]
pub struct TokenBalanceResponse {
    pub balance: Uint128,
}
