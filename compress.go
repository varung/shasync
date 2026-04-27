package main

import (
	"github.com/klauspost/compress/zstd"
)

// zstd magic number: 0xFD2FB528 (little-endian in the first 4 bytes).
var zstdMagic = []byte{0x28, 0xB5, 0x2F, 0xFD}

func isZstd(data []byte) bool {
	return len(data) >= 4 &&
		data[0] == zstdMagic[0] && data[1] == zstdMagic[1] &&
		data[2] == zstdMagic[2] && data[3] == zstdMagic[3]
}

func compressZstd(data []byte) ([]byte, error) {
	enc, err := zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedDefault))
	if err != nil {
		return nil, err
	}
	defer enc.Close()
	return enc.EncodeAll(data, nil), nil
}

func decompressZstd(data []byte) ([]byte, error) {
	dec, err := zstd.NewReader(nil)
	if err != nil {
		return nil, err
	}
	defer dec.Close()
	return dec.DecodeAll(data, nil)
}
