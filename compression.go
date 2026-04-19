package main

import (
	"fmt"

	"github.com/pierrec/lz4/v4"
)

// compressLZ4 compresses data using raw LZ4 block format.
// NX readers call LZ4_decompress_fast which expects raw block data.
func compressLZ4(data []byte) ([]byte, error) {
	dst := make([]byte, lz4.CompressBlockBound(len(data)))
	n, err := lz4.CompressBlock(data, dst, nil)
	if err != nil {
		return nil, fmt.Errorf("lz4 block compress: %w", err)
	}
	if n == 0 {
		// CompressBlock returns 0 for incompressible data.
		// Build a valid LZ4 literal-only block so the reader can decompress it.
		return lz4LiteralBlock(data), nil
	}
	return dst[:n], nil
}

// compressLZ4HC compresses data using raw LZ4 block format at high compression.
func compressLZ4HC(data []byte) ([]byte, error) {
	dst := make([]byte, lz4.CompressBlockBound(len(data)))
	n, err := lz4.CompressBlockHC(data, dst, lz4.CompressionLevel(9), nil, nil)
	if err != nil {
		return nil, fmt.Errorf("lz4hc block compress: %w", err)
	}
	if n == 0 {
		return lz4LiteralBlock(data), nil
	}
	return dst[:n], nil
}

// lz4LiteralBlock creates a valid LZ4 block that stores data as literals only.
// Used when CompressBlock returns 0 (data is incompressible).
func lz4LiteralBlock(data []byte) []byte {
	n := len(data)
	if n == 0 {
		return []byte{0}
	}

	// LZ4 block format for a literal-only last sequence:
	// token byte (high nibble = literal length, capped at 15)
	// optional extra length bytes (255 each until remainder < 255)
	// literal bytes
	var out []byte
	if n < 15 {
		out = make([]byte, 0, 1+n)
		out = append(out, byte(n<<4))
	} else {
		remaining := n - 15
		extraBytes := remaining/255 + 1
		out = make([]byte, 0, 1+extraBytes+n)
		out = append(out, 0xF0)
		for remaining >= 255 {
			out = append(out, 255)
			remaining -= 255
		}
		out = append(out, byte(remaining))
	}
	out = append(out, data...)
	return out
}

// compressData compresses using the appropriate LZ4 variant.
func (c *Converter) compressData(data []byte) ([]byte, error) {
	if c.hc {
		return compressLZ4HC(data)
	}
	return compressLZ4(data)
}
