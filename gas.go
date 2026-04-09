package nitrovm

// Gas cost constants per the NitroVM spec.
const (
	GasCostInstruction  = 1
	GasCostStorageRead  = 200
	GasCostStorageWrite = 5000
	GasCostHashPerByte  = 3
	GasCostSigVerify        = 3000
	GasCostStoreCodePerByte = 420_000

	DefaultGasLimit    = uint64(10_000_000)
	DefaultMemoryLimit = uint32(256) // MB
	DefaultCacheSize   = uint32(100) // MB
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
	g.consumed += amount
	if g.consumed > g.limit {
		return ErrOutOfGas
	}
	return nil
}
