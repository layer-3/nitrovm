package core

import "errors"

var (
	ErrOutOfGas          = errors.New("out of gas")
	ErrCodeNotFound      = errors.New("code not found")
	ErrContractNotFound  = errors.New("contract not found")
	ErrAccountNotFound   = errors.New("account not found")
	ErrInsufficientFunds = errors.New("insufficient funds")
	ErrInvalidAddress    = errors.New("invalid address")
	ErrContractError     = errors.New("contract error")
	ErrMaxDispatchDepth  = errors.New("max sub-message dispatch depth exceeded")
	ErrUnsupportedMsg    = errors.New("unsupported message type")
	ErrInvalidNonce      = errors.New("invalid nonce")
	ErrInvalidSignature  = errors.New("invalid signature")
	ErrInvalidChainID    = errors.New("invalid chain ID")
	ErrInvalidTxType     = errors.New("invalid transaction type")
	ErrGasPriceTooLow    = errors.New("gas price below minimum")
	ErrReplyFailed       = errors.New("reply handler failed")
)
