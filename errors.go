package nitrovm

import "errors"

var (
	ErrOutOfGas          = errors.New("out of gas")
	ErrCodeNotFound      = errors.New("code not found")
	ErrContractNotFound  = errors.New("contract not found")
	ErrAccountNotFound   = errors.New("account not found")
	ErrInsufficientFunds = errors.New("insufficient funds")
	ErrInvalidAddress    = errors.New("invalid address")
	ErrContractError     = errors.New("contract error")
)
