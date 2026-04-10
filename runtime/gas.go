package runtime

import "github.com/layer-3/nitrovm/core"

// Gas cost constants per the NitroVM spec.
const (
	GasCostInstruction      = 1
	GasCostStorageRead      = 200
	GasCostStorageWrite     = 5000
	GasCostHashPerByte      = 3
	GasCostSigVerify        = 3000
	GasCostStoreCodePerByte = 420_000
)

// GasMeter tracks gas consumption during contract execution.
// Implements wasmvmtypes.GasMeter.
type GasMeter struct {
	limit    uint64
	consumed uint64
}

// NewGasMeter creates a gas meter with the given limit.
func NewGasMeter(limit uint64) *GasMeter {
	return &GasMeter{limit: limit}
}

// GasConsumed returns total gas consumed so far.
func (g *GasMeter) GasConsumed() uint64 {
	return g.consumed
}

// GasRemaining returns gas remaining before the limit.
func (g *GasMeter) GasRemaining() uint64 {
	if g.consumed > g.limit {
		return 0
	}
	return g.limit - g.consumed
}

// ConsumeGas adds the given amount to consumed gas, returning ErrOutOfGas if exceeded.
func (g *GasMeter) ConsumeGas(amount uint64) error {
	next := g.consumed + amount
	if next < g.consumed { // uint64 overflow
		return core.ErrOutOfGas
	}
	g.consumed = next
	if g.consumed > g.limit {
		return core.ErrOutOfGas
	}
	return nil
}
