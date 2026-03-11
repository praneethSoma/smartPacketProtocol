package packet

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"io"
)

// ──────────────────────────────────────────────────────────────
// Protocol constants
// ──────────────────────────────────────────────────────────────

const (
	MagicByte1 byte = 0x53 // 'S'
	MagicByte2 byte = 0x50 // 'P'
	MagicByte3 byte = 0x50 // 'P'

	ProtocolVersion uint8 = 1

	// Packet types
	PacketTypeData   uint8 = 0
	PacketTypeGossip uint8 = 1
	PacketTypeProbe  uint8 = 2
	PacketTypeAck    uint8 = 3
	PacketTypeMapReq uint8 = 4 // Request topology map from router
	PacketTypeMapRes uint8 = 5 // Topology map response

	// Wire format header size (fixed portion):
	// Magic(3) + Version(1) + Type(1) + Flags(1) + HeaderLen(2) + PayloadLen(4) + CRC(4) = 16
	WireHeaderSize = 16
)

// ──────────────────────────────────────────────────────────────
// Safety limits — prevent memory exhaustion from malformed/malicious data.
// ──────────────────────────────────────────────────────────────

const (
	// MaxPathLen caps the number of hops in PlannedPath.
	// A 500-node network is well beyond any realistic SPP deployment.
	MaxPathLen = 512

	// MaxMiniMapSize caps the number of links in the MiniMap.
	MaxMiniMapSize = 4096

	// MaxCongestionLogSize caps the number of HopRecords.
	MaxCongestionLogSize = 512

	// MaxVisitedNodes caps the VisitedNodes set size.
	MaxVisitedNodes = 512

	// MaxStringLen caps the length of any length-prefixed string on the wire.
	// Node names exceeding 1 KiB are almost certainly malformed.
	MaxStringLen = 1024

	// MaxPayloadSize caps the payload to 16 MiB.
	MaxPayloadSize = 16 * 1024 * 1024

	// MaxHeaderSize caps the variable header to 1 MiB.
	MaxHeaderSize = 1 * 1024 * 1024
)

// ──────────────────────────────────────────────────────────────
// Flag bits in the fixed header Flags byte.
// ──────────────────────────────────────────────────────────────

const (
	FlagDegraded uint8 = 0x01 // Bit 0: packet took a suboptimal path
	FlagRerouted uint8 = 0x02 // Bit 1: path was recalculated mid-flight
)

// WireHeader is the fixed-size header at the start of every SPP frame.
type WireHeader struct {
	Magic      [3]byte
	Version    uint8
	PacketType uint8
	Flags      uint8  // FlagDegraded | FlagRerouted
	HeaderLen  uint16 // Length of the variable header
	PayloadLen uint32 // Length of the payload
	Checksum   uint32 // CRC32 of (variable header + payload)
}

// ──────────────────────────────────────────────────────────────
// Encode
// ──────────────────────────────────────────────────────────────

// EncodeWire serializes a SmartPacket into the SPP binary wire format.
// Returns the complete frame (fixed header + variable header + payload).
func (p *SmartPacket) EncodeWire() ([]byte, error) {
	// -- Variable header --
	var hdr bytes.Buffer

	// Intent (4 bytes, fixed).
	hdr.WriteByte(p.Intent.Reliability)
	hdr.WriteByte(p.Intent.Latency)
	hdr.WriteByte(p.Intent.Ordering)
	hdr.WriteByte(p.Intent.Priority)

	// Routing metadata (5 bytes, fixed).
	hdr.WriteByte(p.Version)
	hdr.WriteByte(p.PacketType)
	hdr.WriteByte(p.MaxHops)
	hdr.WriteByte(p.HopCount)
	if p.Degraded {
		hdr.WriteByte(1)
	} else {
		hdr.WriteByte(0)
	}

	// Destination + current node (length-prefixed strings).
	if err := binWriteString(&hdr, p.Destination); err != nil {
		return nil, fmt.Errorf("encode destination: %w", err)
	}
	if err := binWriteString(&hdr, p.CurrentNode); err != nil {
		return nil, fmt.Errorf("encode current_node: %w", err)
	}

	// Planned path.
	if err := binary.Write(&hdr, binary.BigEndian, uint16(len(p.PlannedPath))); err != nil {
		return nil, fmt.Errorf("encode path_len: %w", err)
	}
	for i, node := range p.PlannedPath {
		if err := binWriteString(&hdr, node); err != nil {
			return nil, fmt.Errorf("encode path[%d]: %w", i, err)
		}
	}

	// Hop index.
	if err := binary.Write(&hdr, binary.BigEndian, uint16(p.HopIndex)); err != nil {
		return nil, fmt.Errorf("encode hop_index: %w", err)
	}

	// Mini map.
	if err := binary.Write(&hdr, binary.BigEndian, uint16(len(p.MiniMap))); err != nil {
		return nil, fmt.Errorf("encode map_len: %w", err)
	}
	for i, link := range p.MiniMap {
		if err := binWriteString(&hdr, link.From); err != nil {
			return nil, fmt.Errorf("encode map[%d].from: %w", i, err)
		}
		if err := binWriteString(&hdr, link.To); err != nil {
			return nil, fmt.Errorf("encode map[%d].to: %w", i, err)
		}
		if err := binary.Write(&hdr, binary.BigEndian, link.LatencyMs); err != nil {
			return nil, fmt.Errorf("encode map[%d].latency: %w", i, err)
		}
		if err := binary.Write(&hdr, binary.BigEndian, link.LoadPct); err != nil {
			return nil, fmt.Errorf("encode map[%d].load: %w", i, err)
		}
		if err := binary.Write(&hdr, binary.BigEndian, link.LossPct); err != nil {
			return nil, fmt.Errorf("encode map[%d].loss: %w", i, err)
		}
	}

	// Congestion log.
	if err := binary.Write(&hdr, binary.BigEndian, uint16(len(p.CongestionLog))); err != nil {
		return nil, fmt.Errorf("encode log_len: %w", err)
	}
	for i, hop := range p.CongestionLog {
		if err := binWriteString(&hdr, hop.NodeName); err != nil {
			return nil, fmt.Errorf("encode log[%d].name: %w", i, err)
		}
		if err := binary.Write(&hdr, binary.BigEndian, hop.LoadPct); err != nil {
			return nil, fmt.Errorf("encode log[%d].load: %w", i, err)
		}
		if err := binary.Write(&hdr, binary.BigEndian, hop.LatencyMs); err != nil {
			return nil, fmt.Errorf("encode log[%d].latency: %w", i, err)
		}
	}

	// Visited nodes.
	if err := binary.Write(&hdr, binary.BigEndian, uint16(len(p.VisitedNodes))); err != nil {
		return nil, fmt.Errorf("encode visited_len: %w", err)
	}
	for node := range p.VisitedNodes {
		if err := binWriteString(&hdr, node); err != nil {
			return nil, fmt.Errorf("encode visited node: %w", err)
		}
	}

	headerData := hdr.Bytes()

	// -- Flags --
	var flags uint8
	if p.Degraded {
		flags |= FlagDegraded
	}
	if p.Rerouted {
		flags |= FlagRerouted
	}

	// -- CRC32 over (variable header + payload) --
	crcBuf := make([]byte, len(headerData)+len(p.Payload))
	copy(crcBuf, headerData)
	copy(crcBuf[len(headerData):], p.Payload)
	checksum := crc32.ChecksumIEEE(crcBuf)

	// -- Assemble fixed header + variable header + payload --
	var out bytes.Buffer
	out.Grow(WireHeaderSize + len(headerData) + len(p.Payload))

	out.Write([]byte{MagicByte1, MagicByte2, MagicByte3})
	out.WriteByte(ProtocolVersion)
	out.WriteByte(p.PacketType)
	out.WriteByte(flags)
	binary.Write(&out, binary.BigEndian, uint16(len(headerData)))
	binary.Write(&out, binary.BigEndian, uint32(len(p.Payload)))
	binary.Write(&out, binary.BigEndian, checksum)

	out.Write(headerData)
	out.Write(p.Payload)

	return out.Bytes(), nil
}

// ──────────────────────────────────────────────────────────────
// Decode
// ──────────────────────────────────────────────────────────────

// DecodeWire decodes an SPP binary wire frame into a SmartPacket.
// Returns a descriptive error if the frame is malformed, truncated,
// or fails the CRC32 integrity check.
func DecodeWire(data []byte) (*SmartPacket, error) {
	if len(data) < WireHeaderSize {
		return nil, fmt.Errorf("packet too short: %d bytes (minimum %d)", len(data), WireHeaderSize)
	}

	// -- Validate magic --
	if data[0] != MagicByte1 || data[1] != MagicByte2 || data[2] != MagicByte3 {
		return nil, fmt.Errorf("invalid magic bytes: %02x %02x %02x", data[0], data[1], data[2])
	}

	version := data[3]
	if version != ProtocolVersion {
		return nil, fmt.Errorf("unsupported protocol version: %d (expected %d)", version, ProtocolVersion)
	}

	packetType := data[4]
	flags := data[5]
	headerLen := binary.BigEndian.Uint16(data[6:8])
	payloadLen := binary.BigEndian.Uint32(data[8:12])
	checksum := binary.BigEndian.Uint32(data[12:16])

	// -- Size safety checks --
	if int(headerLen) > MaxHeaderSize {
		return nil, fmt.Errorf("header too large: %d bytes (max %d)", headerLen, MaxHeaderSize)
	}
	if int(payloadLen) > MaxPayloadSize {
		return nil, fmt.Errorf("payload too large: %d bytes (max %d)", payloadLen, MaxPayloadSize)
	}

	totalExpected := WireHeaderSize + int(headerLen) + int(payloadLen)
	if len(data) < totalExpected {
		return nil, fmt.Errorf("packet truncated: got %d bytes, expected %d", len(data), totalExpected)
	}

	headerData := data[WireHeaderSize : WireHeaderSize+int(headerLen)]
	payloadData := data[WireHeaderSize+int(headerLen) : totalExpected]

	// -- CRC32 integrity check --
	crcBuf := make([]byte, len(headerData)+len(payloadData))
	copy(crcBuf, headerData)
	copy(crcBuf[len(headerData):], payloadData)
	if crc32.ChecksumIEEE(crcBuf) != checksum {
		return nil, fmt.Errorf("CRC32 checksum mismatch: frame corrupted")
	}

	// -- Parse variable header --
	r := bytes.NewReader(headerData)
	p := &SmartPacket{}

	// Intent (4 bytes).
	var intentBytes [4]byte
	if _, err := io.ReadFull(r, intentBytes[:]); err != nil {
		return nil, fmt.Errorf("read intent: %w", err)
	}
	p.Intent = IntentHeader{
		Reliability: intentBytes[0],
		Latency:     intentBytes[1],
		Ordering:    intentBytes[2],
		Priority:    intentBytes[3],
	}

	// Routing metadata (5 bytes).
	var metaBytes [5]byte
	if _, err := io.ReadFull(r, metaBytes[:]); err != nil {
		return nil, fmt.Errorf("read routing metadata: %w", err)
	}
	p.Version = metaBytes[0]
	p.PacketType = metaBytes[1]
	p.MaxHops = metaBytes[2]
	p.HopCount = metaBytes[3]
	p.Degraded = metaBytes[4] == 1

	// Destination + current node.
	var err error
	if p.Destination, err = binReadString(r); err != nil {
		return nil, fmt.Errorf("read destination: %w", err)
	}
	if p.CurrentNode, err = binReadString(r); err != nil {
		return nil, fmt.Errorf("read current_node: %w", err)
	}

	// Planned path.
	var pathLen uint16
	if err := binary.Read(r, binary.BigEndian, &pathLen); err != nil {
		return nil, fmt.Errorf("read path_len: %w", err)
	}
	if int(pathLen) > MaxPathLen {
		return nil, fmt.Errorf("path too long: %d (max %d)", pathLen, MaxPathLen)
	}
	p.PlannedPath = make([]string, pathLen)
	for i := range p.PlannedPath {
		if p.PlannedPath[i], err = binReadString(r); err != nil {
			return nil, fmt.Errorf("read path[%d]: %w", i, err)
		}
	}

	// Hop index.
	var hopIdx uint16
	if err := binary.Read(r, binary.BigEndian, &hopIdx); err != nil {
		return nil, fmt.Errorf("read hop_index: %w", err)
	}
	p.HopIndex = int(hopIdx)

	// Mini map.
	var mapLen uint16
	if err := binary.Read(r, binary.BigEndian, &mapLen); err != nil {
		return nil, fmt.Errorf("read map_len: %w", err)
	}
	if int(mapLen) > MaxMiniMapSize {
		return nil, fmt.Errorf("mini map too large: %d (max %d)", mapLen, MaxMiniMapSize)
	}
	p.MiniMap = make([]Link, mapLen)
	for i := range p.MiniMap {
		if p.MiniMap[i].From, err = binReadString(r); err != nil {
			return nil, fmt.Errorf("read map[%d].from: %w", i, err)
		}
		if p.MiniMap[i].To, err = binReadString(r); err != nil {
			return nil, fmt.Errorf("read map[%d].to: %w", i, err)
		}
		if err := binary.Read(r, binary.BigEndian, &p.MiniMap[i].LatencyMs); err != nil {
			return nil, fmt.Errorf("read map[%d].latency: %w", i, err)
		}
		if err := binary.Read(r, binary.BigEndian, &p.MiniMap[i].LoadPct); err != nil {
			return nil, fmt.Errorf("read map[%d].load: %w", i, err)
		}
		if err := binary.Read(r, binary.BigEndian, &p.MiniMap[i].LossPct); err != nil {
			return nil, fmt.Errorf("read map[%d].loss: %w", i, err)
		}
	}

	// Congestion log.
	var logLen uint16
	if err := binary.Read(r, binary.BigEndian, &logLen); err != nil {
		return nil, fmt.Errorf("read log_len: %w", err)
	}
	if int(logLen) > MaxCongestionLogSize {
		return nil, fmt.Errorf("congestion log too large: %d (max %d)", logLen, MaxCongestionLogSize)
	}
	p.CongestionLog = make([]HopRecord, logLen)
	for i := range p.CongestionLog {
		if p.CongestionLog[i].NodeName, err = binReadString(r); err != nil {
			return nil, fmt.Errorf("read log[%d].name: %w", i, err)
		}
		if err := binary.Read(r, binary.BigEndian, &p.CongestionLog[i].LoadPct); err != nil {
			return nil, fmt.Errorf("read log[%d].load: %w", i, err)
		}
		if err := binary.Read(r, binary.BigEndian, &p.CongestionLog[i].LatencyMs); err != nil {
			return nil, fmt.Errorf("read log[%d].latency: %w", i, err)
		}
	}

	// Visited nodes.
	var visitedLen uint16
	if err := binary.Read(r, binary.BigEndian, &visitedLen); err != nil {
		return nil, fmt.Errorf("read visited_len: %w", err)
	}
	if int(visitedLen) > MaxVisitedNodes {
		return nil, fmt.Errorf("visited nodes too large: %d (max %d)", visitedLen, MaxVisitedNodes)
	}
	p.VisitedNodes = make(map[string]bool, visitedLen)
	for i := 0; i < int(visitedLen); i++ {
		node, err := binReadString(r)
		if err != nil {
			return nil, fmt.Errorf("read visited[%d]: %w", i, err)
		}
		p.VisitedNodes[node] = true
	}

	// -- Apply fixed header fields --
	p.PacketType = packetType
	if flags&FlagDegraded != 0 {
		p.Degraded = true
	}
	if flags&FlagRerouted != 0 {
		p.Rerouted = true
	}

	p.Payload = payloadData
	return p, nil
}

// ──────────────────────────────────────────────────────────────
// Wire I/O helpers — error-propagating, bounds-checked.
// ──────────────────────────────────────────────────────────────

// binWriteString writes a uint16-length-prefixed string to w.
func binWriteString(w *bytes.Buffer, s string) error {
	if len(s) > MaxStringLen {
		return fmt.Errorf("string too long: %d bytes (max %d)", len(s), MaxStringLen)
	}
	if err := binary.Write(w, binary.BigEndian, uint16(len(s))); err != nil {
		return err
	}
	_, err := w.WriteString(s)
	return err
}

// binReadString reads a uint16-length-prefixed string from r.
// Returns an error if the length exceeds MaxStringLen or the
// reader does not have enough bytes.
func binReadString(r *bytes.Reader) (string, error) {
	var length uint16
	if err := binary.Read(r, binary.BigEndian, &length); err != nil {
		return "", fmt.Errorf("read string length: %w", err)
	}
	if int(length) > MaxStringLen {
		return "", fmt.Errorf("string length %d exceeds max %d", length, MaxStringLen)
	}
	data := make([]byte, length)
	if _, err := io.ReadFull(r, data); err != nil {
		return "", fmt.Errorf("read string data (%d bytes): %w", length, err)
	}
	return string(data), nil
}

// ──────────────────────────────────────────────────────────────
// Backward-compatible aliases for the old helper names.
// These are unexported so they only affect in-package callers.
// ──────────────────────────────────────────────────────────────

// writeString is the legacy helper kept for any in-package callers.
// Deprecated: use binWriteString.
func writeString(buf *bytes.Buffer, s string) {
	_ = binWriteString(buf, s)
}

// readString is the legacy helper kept for any in-package callers.
// Deprecated: use binReadString.
func readString(r *bytes.Reader) string {
	s, _ := binReadString(r)
	return s
}
