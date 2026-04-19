package main

import (
	"fmt"

	"github.com/pierrec/lz4/v4"
)

// compressLZ4 compresses data using raw LZ4 block format.
// NX readers use LZ4_decompress_fast which expects raw block data,
// NOT the LZ4 frame format that lz4.NewWriter produces.
func compressLZ4(data []byte) ([]byte, error) {
	dst := make([]byte, lz4.CompressBlockBound(len(data)))
	n, err := lz4.CompressBlock(data, dst, nil)
	if err != nil {
		return nil, fmt.Errorf("lz4 block compress: %w", err)
	}
	if n == 0 {
		// Data is incompressible; store as-is — the reader will still
		// decompress correctly because LZ4_decompress_fast handles this.
		return data, nil
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
		return data, nil
	}
	return dst[:n], nil
}

// compressData compresses using the appropriate LZ4 variant.
func (c *Converter) compressData(data []byte) ([]byte, error) {
	if c.hc {
		return compressLZ4HC(data)
	}
	return compressLZ4(data)
}
