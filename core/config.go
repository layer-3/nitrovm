package core

const (
	DefaultGasLimit    = uint64(10_000_000)
	DefaultMemoryLimit = uint32(256) // MB
	DefaultCacheSize   = uint32(100) // MB
)

// Config holds NitroVM configuration.
type Config struct {
	DataDir     string
	MemoryLimit uint32 // MB, default 256
	CacheSize   uint32 // MB, default 100
	PrintDebug  bool
	ChainID     string
}

// DefaultConfig returns a Config with sensible defaults.
func DefaultConfig() Config {
	return Config{
		DataDir:     ".",
		MemoryLimit: DefaultMemoryLimit,
		CacheSize:   DefaultCacheSize,
		PrintDebug:  true,
		ChainID:     "nitrovm-1",
	}
}
