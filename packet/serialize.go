package packet

import (
	"bytes"
	"encoding/gob"
	"fmt"
	"io"
)

// MaxGobDecodeSize is the maximum number of bytes Decode will read
// from the input buffer. This prevents memory exhaustion from
// untrusted or malformed gob-encoded payloads.
const MaxGobDecodeSize = 16 * 1024 * 1024 // 16 MiB

// Encode serializes a SmartPacket using Go's gob encoding.
//
// Deprecated: gob encoding is retained for backward compatibility
// with existing deployments. New code should use EncodeWire() which
// produces a more compact, self-describing binary format with CRC32
// integrity checking.
func (p *SmartPacket) Encode() ([]byte, error) {
	var buf bytes.Buffer
	if err := gob.NewEncoder(&buf).Encode(p); err != nil {
		return nil, fmt.Errorf("gob encode: %w", err)
	}
	return buf.Bytes(), nil
}

// Decode deserializes a gob-encoded byte slice into a SmartPacket.
//
// The input is capped at MaxGobDecodeSize to prevent memory exhaustion.
//
// Deprecated: gob decoding is retained for backward compatibility.
// New code should use DecodeWire() which validates CRC32 integrity
// and enforces field-level size limits.
func Decode(data []byte) (*SmartPacket, error) {
	if len(data) > MaxGobDecodeSize {
		return nil, fmt.Errorf("gob payload too large: %d bytes (max %d)", len(data), MaxGobDecodeSize)
	}

	r := io.LimitReader(bytes.NewReader(data), int64(MaxGobDecodeSize))
	var p SmartPacket
	if err := gob.NewDecoder(r).Decode(&p); err != nil {
		return nil, fmt.Errorf("gob decode: %w", err)
	}
	return &p, nil
}
