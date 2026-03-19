package packet

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"io"
	"math"
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
	FlagDegraded  uint8 = 0x01 // Bit 0: packet took a suboptimal path
	FlagRerouted  uint8 = 0x02 // Bit 1: path was recalculated mid-flight
	FlagLightMode  uint8 = 0x04 // Bit 2: no MiniMap — routers use local gossip
	FlagCompactIDs      uint8 = 0x08 // Bit 3: node names encoded as uint16 IDs
	FlagCompactMetrics  uint8 = 0x10 // Bit 4: metrics as uint16 fixed-point (×100) instead of float64
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
	// Estimate total size to do a single allocation.
	// Fixed header(16) + intent(4) + meta(5) + dest/current(4+2*~10)
	// + path(2+N*~12) + hopidx(2) + map(2+N*~34) + log(2+N*~26)
	// + visited(2+N*~12) + timestamp(8) + payload
	est := WireHeaderSize + 32
	for _, node := range p.PlannedPath {
		est += 2 + len(node)
	}
	for _, link := range p.MiniMap {
		est += 2 + len(link.From) + 2 + len(link.To) + 24
	}
	for _, hop := range p.CongestionLog {
		est += 2 + len(hop.NodeName) + 16
	}
	for node := range p.VisitedNodes {
		est += 2 + len(node)
	}
	est += 2 + len(p.Destination) + 2 + len(p.CurrentNode) + 2 + len(p.SourceNode)
	est += len(p.Payload) + 10

	buf := make([]byte, 0, est)

	// Reserve space for fixed header (filled at the end).
	buf = buf[:WireHeaderSize]

	// -- Variable header (appended directly) --
	hdrStart := WireHeaderSize

	// Intent (4 bytes).
	buf = append(buf, p.Intent.Reliability, p.Intent.Latency, p.Intent.Ordering, p.Intent.Priority)

	// Routing metadata (5 bytes).
	degradedByte := byte(0)
	if p.Degraded {
		degradedByte = 1
	}
	buf = append(buf, p.Version, p.PacketType, p.MaxHops, p.HopCount, degradedByte)

	// Destination + current node.
	buf = appendString(buf, p.Destination)
	buf = appendString(buf, p.CurrentNode)

	// Planned path.
	buf = binary.BigEndian.AppendUint16(buf, uint16(len(p.PlannedPath)))
	for _, node := range p.PlannedPath {
		buf = appendString(buf, node)
	}

	// Hop index.
	buf = binary.BigEndian.AppendUint16(buf, uint16(p.HopIndex))

	// Mini map.
	buf = binary.BigEndian.AppendUint16(buf, uint16(len(p.MiniMap)))
	for _, link := range p.MiniMap {
		buf = appendString(buf, link.From)
		buf = appendString(buf, link.To)
		buf = binary.BigEndian.AppendUint64(buf, math.Float64bits(link.LatencyMs))
		buf = binary.BigEndian.AppendUint64(buf, math.Float64bits(link.LoadPct))
		buf = binary.BigEndian.AppendUint64(buf, math.Float64bits(link.LossPct))
	}

	// Congestion log.
	buf = binary.BigEndian.AppendUint16(buf, uint16(len(p.CongestionLog)))
	for _, hop := range p.CongestionLog {
		buf = appendString(buf, hop.NodeName)
		buf = binary.BigEndian.AppendUint64(buf, math.Float64bits(hop.LoadPct))
		buf = binary.BigEndian.AppendUint64(buf, math.Float64bits(hop.LatencyMs))
	}

	// Visited nodes.
	buf = binary.BigEndian.AppendUint16(buf, uint16(len(p.VisitedNodes)))
	for node := range p.VisitedNodes {
		buf = appendString(buf, node)
	}

	// Timestamp (8 bytes).
	buf = binary.BigEndian.AppendUint64(buf, uint64(p.CreatedAtNs))

	headerData := buf[hdrStart:]

	// -- Flags --
	var flags uint8
	if p.Degraded {
		flags |= FlagDegraded
	}
	if p.Rerouted {
		flags |= FlagRerouted
	}
	if p.LightMode {
		flags |= FlagLightMode
	}

	// -- CRC32 over (variable header + payload) --
	h := crc32.NewIEEE()
	h.Write(headerData)
	h.Write(p.Payload)
	checksum := h.Sum32()

	// -- Fill fixed header --
	buf[0] = MagicByte1
	buf[1] = MagicByte2
	buf[2] = MagicByte3
	buf[3] = ProtocolVersion
	buf[4] = p.PacketType
	buf[5] = flags
	binary.BigEndian.PutUint16(buf[6:8], uint16(len(headerData)))
	binary.BigEndian.PutUint32(buf[8:12], uint32(len(p.Payload)))
	binary.BigEndian.PutUint32(buf[12:16], checksum)

	// Append payload.
	buf = append(buf, p.Payload...)

	return buf, nil
}

// appendString appends a uint16-length-prefixed string to buf.
func appendString(buf []byte, s string) []byte {
	buf = binary.BigEndian.AppendUint16(buf, uint16(len(s)))
	return append(buf, s...)
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
	headerLen := int(binary.BigEndian.Uint16(data[6:8]))
	payloadLen := int(binary.BigEndian.Uint32(data[8:12]))
	checksum := binary.BigEndian.Uint32(data[12:16])

	// -- Size safety checks --
	if headerLen > MaxHeaderSize {
		return nil, fmt.Errorf("header too large: %d bytes (max %d)", headerLen, MaxHeaderSize)
	}
	if payloadLen > MaxPayloadSize {
		return nil, fmt.Errorf("payload too large: %d bytes (max %d)", payloadLen, MaxPayloadSize)
	}

	totalExpected := WireHeaderSize + headerLen + payloadLen
	if len(data) < totalExpected {
		return nil, fmt.Errorf("packet truncated: got %d bytes, expected %d", len(data), totalExpected)
	}

	headerData := data[WireHeaderSize : WireHeaderSize+headerLen]
	payloadData := data[WireHeaderSize+headerLen : totalExpected]

	// -- CRC32 integrity check --
	h := crc32.NewIEEE()
	h.Write(headerData)
	h.Write(payloadData)
	if h.Sum32() != checksum {
		return nil, fmt.Errorf("CRC32 checksum mismatch: frame corrupted")
	}

	// -- Parse variable header using direct offset access --
	d := headerData
	off := 0

	need := func(n int) error {
		if off+n > len(d) {
			return fmt.Errorf("header truncated at offset %d (need %d more bytes)", off, n)
		}
		return nil
	}

	readU16 := func() (uint16, error) {
		if err := need(2); err != nil {
			return 0, err
		}
		v := binary.BigEndian.Uint16(d[off : off+2])
		off += 2
		return v, nil
	}

	readU64 := func() (uint64, error) {
		if err := need(8); err != nil {
			return 0, err
		}
		v := binary.BigEndian.Uint64(d[off : off+8])
		off += 8
		return v, nil
	}

	readFloat64 := func() (float64, error) {
		v, err := readU64()
		return math.Float64frombits(v), err
	}

	readStr := func() (string, error) {
		slen, err := readU16()
		if err != nil {
			return "", err
		}
		if int(slen) > MaxStringLen {
			return "", fmt.Errorf("string length %d exceeds max %d", slen, MaxStringLen)
		}
		if err := need(int(slen)); err != nil {
			return "", err
		}
		s := string(d[off : off+int(slen)])
		off += int(slen)
		return s, nil
	}

	p := &SmartPacket{}

	// Intent (4 bytes).
	if err := need(4); err != nil {
		return nil, fmt.Errorf("read intent: %w", err)
	}
	p.Intent = IntentHeader{d[off], d[off+1], d[off+2], d[off+3]}
	off += 4

	// Routing metadata (5 bytes).
	if err := need(5); err != nil {
		return nil, fmt.Errorf("read routing metadata: %w", err)
	}
	p.Version = d[off]
	p.PacketType = d[off+1]
	p.MaxHops = d[off+2]
	p.HopCount = d[off+3]
	p.Degraded = d[off+4] == 1
	off += 5

	// Destination + current node.
	var err error
	if p.Destination, err = readStr(); err != nil {
		return nil, fmt.Errorf("read destination: %w", err)
	}
	if p.CurrentNode, err = readStr(); err != nil {
		return nil, fmt.Errorf("read current_node: %w", err)
	}

	// Planned path.
	pathLen, err := readU16()
	if err != nil {
		return nil, fmt.Errorf("read path_len: %w", err)
	}
	if int(pathLen) > MaxPathLen {
		return nil, fmt.Errorf("path too long: %d (max %d)", pathLen, MaxPathLen)
	}
	p.PlannedPath = make([]string, pathLen)
	for i := range p.PlannedPath {
		if p.PlannedPath[i], err = readStr(); err != nil {
			return nil, fmt.Errorf("read path[%d]: %w", i, err)
		}
	}

	// Hop index.
	hopIdx, err := readU16()
	if err != nil {
		return nil, fmt.Errorf("read hop_index: %w", err)
	}
	p.HopIndex = int(hopIdx)

	// Mini map.
	mapLen, err := readU16()
	if err != nil {
		return nil, fmt.Errorf("read map_len: %w", err)
	}
	if int(mapLen) > MaxMiniMapSize {
		return nil, fmt.Errorf("mini map too large: %d (max %d)", mapLen, MaxMiniMapSize)
	}
	p.MiniMap = make([]Link, mapLen)
	for i := range p.MiniMap {
		if p.MiniMap[i].From, err = readStr(); err != nil {
			return nil, fmt.Errorf("read map[%d].from: %w", i, err)
		}
		if p.MiniMap[i].To, err = readStr(); err != nil {
			return nil, fmt.Errorf("read map[%d].to: %w", i, err)
		}
		if p.MiniMap[i].LatencyMs, err = readFloat64(); err != nil {
			return nil, fmt.Errorf("read map[%d].latency: %w", i, err)
		}
		if p.MiniMap[i].LoadPct, err = readFloat64(); err != nil {
			return nil, fmt.Errorf("read map[%d].load: %w", i, err)
		}
		if p.MiniMap[i].LossPct, err = readFloat64(); err != nil {
			return nil, fmt.Errorf("read map[%d].loss: %w", i, err)
		}
	}

	// Congestion log.
	logLen, err := readU16()
	if err != nil {
		return nil, fmt.Errorf("read log_len: %w", err)
	}
	if int(logLen) > MaxCongestionLogSize {
		return nil, fmt.Errorf("congestion log too large: %d (max %d)", logLen, MaxCongestionLogSize)
	}
	p.CongestionLog = make([]HopRecord, logLen)
	for i := range p.CongestionLog {
		if p.CongestionLog[i].NodeName, err = readStr(); err != nil {
			return nil, fmt.Errorf("read log[%d].name: %w", i, err)
		}
		if p.CongestionLog[i].LoadPct, err = readFloat64(); err != nil {
			return nil, fmt.Errorf("read log[%d].load: %w", i, err)
		}
		if p.CongestionLog[i].LatencyMs, err = readFloat64(); err != nil {
			return nil, fmt.Errorf("read log[%d].latency: %w", i, err)
		}
	}

	// Visited nodes.
	visitedLen, err := readU16()
	if err != nil {
		return nil, fmt.Errorf("read visited_len: %w", err)
	}
	if int(visitedLen) > MaxVisitedNodes {
		return nil, fmt.Errorf("visited nodes too large: %d (max %d)", visitedLen, MaxVisitedNodes)
	}
	p.VisitedNodes = make(map[string]bool, visitedLen)
	for i := 0; i < int(visitedLen); i++ {
		node, err := readStr()
		if err != nil {
			return nil, fmt.Errorf("read visited[%d]: %w", i, err)
		}
		p.VisitedNodes[node] = true
	}

	// Timestamp (8 bytes).
	tsVal, err := readU64()
	if err != nil {
		return nil, fmt.Errorf("read created_at: %w", err)
	}
	p.CreatedAtNs = int64(tsVal)

	// -- Apply fixed header fields --
	p.PacketType = packetType
	if flags&FlagDegraded != 0 {
		p.Degraded = true
	}
	if flags&FlagRerouted != 0 {
		p.Rerouted = true
	}
	if flags&FlagLightMode != 0 {
		p.LightMode = true
	}

	p.Payload = payloadData
	return p, nil
}

// ──────────────────────────────────────────────────────────────
// Compact ID encode / decode
// ──────────────────────────────────────────────────────────────

// ──────────────────────────────────────────────────────────────
// Compact metric helpers — float64 ↔ uint16 fixed-point (×100)
//
// Latency: 0.01ms precision, max 655.35ms (sufficient for LAN/DC)
// Load:    0.01% precision,  max 655.35%
// Loss:    0.01% precision,  max 655.35%
// ──────────────────────────────────────────────────────────────

func encodeMetricU16(w *bytes.Buffer, val float64) error {
	scaled := uint16(val * 100)
	if val*100 > 65535 {
		scaled = 65535
	}
	return binary.Write(w, binary.BigEndian, scaled)
}

func decodeMetricU16(r *bytes.Reader) (float64, error) {
	var scaled uint16
	if err := binary.Read(r, binary.BigEndian, &scaled); err != nil {
		return 0, err
	}
	return float64(scaled) / 100.0, nil
}

// binWriteNodeID writes a node name as a uint16 compact ID.
// ID 0 is reserved as a sentinel for empty strings (e.g., CurrentNode
// before the first hop). User-assigned IDs must start at 1.
func binWriteNodeID(w *bytes.Buffer, name string, nit *NodeIDTable) error {
	if name == "" {
		return binary.Write(w, binary.BigEndian, uint16(0))
	}
	id, ok := nit.ToID(name)
	if !ok {
		return fmt.Errorf("no compact ID for node %q", name)
	}
	return binary.Write(w, binary.BigEndian, id)
}

// binReadNodeID reads a uint16 compact ID and resolves it to a node name.
// ID 0 is the sentinel for an empty string.
func binReadNodeID(r *bytes.Reader, nit *NodeIDTable) (string, error) {
	var id uint16
	if err := binary.Read(r, binary.BigEndian, &id); err != nil {
		return "", fmt.Errorf("read node ID: %w", err)
	}
	if id == 0 {
		return "", nil
	}
	name, ok := nit.ToName(id)
	if !ok {
		return "", fmt.Errorf("unknown compact ID %d", id)
	}
	return name, nil
}

// EncodeWireWithIDs serializes a SmartPacket using compact uint16 IDs for
// all node name fields. If nit is nil, falls back to EncodeWire().
func (p *SmartPacket) EncodeWireWithIDs(nit *NodeIDTable) ([]byte, error) {
	if nit == nil {
		return p.EncodeWire()
	}

	// -- Variable header --
	var hdr bytes.Buffer

	// Intent (4 bytes, fixed).
	hdr.WriteByte(p.Intent.Reliability)
	hdr.WriteByte(p.Intent.Latency)
	hdr.WriteByte(p.Intent.Ordering)
	hdr.WriteByte(p.Intent.Priority)

	// Routing metadata (4 bytes — Degraded is in the flags byte, not repeated here).
	hdr.WriteByte(p.Version)
	hdr.WriteByte(p.PacketType)
	hdr.WriteByte(p.MaxHops)
	hdr.WriteByte(p.HopCount)

	// Destination + current node (compact IDs).
	if err := binWriteNodeID(&hdr, p.Destination, nit); err != nil {
		return nil, fmt.Errorf("encode destination: %w", err)
	}
	if err := binWriteNodeID(&hdr, p.CurrentNode, nit); err != nil {
		return nil, fmt.Errorf("encode current_node: %w", err)
	}

	// Planned path.
	if err := binary.Write(&hdr, binary.BigEndian, uint16(len(p.PlannedPath))); err != nil {
		return nil, fmt.Errorf("encode path_len: %w", err)
	}
	for i, node := range p.PlannedPath {
		if err := binWriteNodeID(&hdr, node, nit); err != nil {
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
		if err := binWriteNodeID(&hdr, link.From, nit); err != nil {
			return nil, fmt.Errorf("encode map[%d].from: %w", i, err)
		}
		if err := binWriteNodeID(&hdr, link.To, nit); err != nil {
			return nil, fmt.Errorf("encode map[%d].to: %w", i, err)
		}
		if err := encodeMetricU16(&hdr, link.LatencyMs); err != nil {
			return nil, fmt.Errorf("encode map[%d].latency: %w", i, err)
		}
		if err := encodeMetricU16(&hdr, link.LoadPct); err != nil {
			return nil, fmt.Errorf("encode map[%d].load: %w", i, err)
		}
		if err := encodeMetricU16(&hdr, link.LossPct); err != nil {
			return nil, fmt.Errorf("encode map[%d].loss: %w", i, err)
		}
	}

	// Congestion log.
	if err := binary.Write(&hdr, binary.BigEndian, uint16(len(p.CongestionLog))); err != nil {
		return nil, fmt.Errorf("encode log_len: %w", err)
	}
	for i, hop := range p.CongestionLog {
		if err := binWriteNodeID(&hdr, hop.NodeName, nit); err != nil {
			return nil, fmt.Errorf("encode log[%d].name: %w", i, err)
		}
		if err := encodeMetricU16(&hdr, hop.LoadPct); err != nil {
			return nil, fmt.Errorf("encode log[%d].load: %w", i, err)
		}
		if err := encodeMetricU16(&hdr, hop.LatencyMs); err != nil {
			return nil, fmt.Errorf("encode log[%d].latency: %w", i, err)
		}
	}

	// Visited nodes.
	if err := binary.Write(&hdr, binary.BigEndian, uint16(len(p.VisitedNodes))); err != nil {
		return nil, fmt.Errorf("encode visited_len: %w", err)
	}
	for node := range p.VisitedNodes {
		if err := binWriteNodeID(&hdr, node, nit); err != nil {
			return nil, fmt.Errorf("encode visited node: %w", err)
		}
	}

	// Timestamp (8 bytes).
	if err := binary.Write(&hdr, binary.BigEndian, p.CreatedAtNs); err != nil {
		return nil, fmt.Errorf("encode created_at: %w", err)
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
	if p.LightMode {
		flags |= FlagLightMode
	}
	flags |= FlagCompactIDs
	flags |= FlagCompactMetrics

	// -- CRC32 over (variable header + payload) -- streaming, no extra alloc
	h := crc32.NewIEEE()
	h.Write(headerData)
	h.Write(p.Payload)
	checksum := h.Sum32()

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

// DecodeWireWithIDs decodes an SPP wire frame, automatically handling both
// compact ID and string-encoded packets based on the FlagCompactIDs flag.
// If nit is nil and the packet has compact IDs, returns an error.
func DecodeWireWithIDs(data []byte, nit *NodeIDTable) (*SmartPacket, error) {
	if len(data) < WireHeaderSize {
		return nil, fmt.Errorf("packet too short: %d bytes (minimum %d)", len(data), WireHeaderSize)
	}

	// Check if compact IDs flag is set.
	flags := data[5]
	if flags&FlagCompactIDs == 0 {
		// No compact IDs — fall back to standard decode.
		return DecodeWire(data)
	}

	if nit == nil {
		return nil, fmt.Errorf("packet has compact IDs but no NodeIDTable provided")
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
	headerLen := binary.BigEndian.Uint16(data[6:8])
	payloadLen := binary.BigEndian.Uint32(data[8:12])
	checksum := binary.BigEndian.Uint32(data[12:16])

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

	// -- CRC32 integrity check -- streaming, no extra alloc
	h := crc32.NewIEEE()
	h.Write(headerData)
	h.Write(payloadData)
	if h.Sum32() != checksum {
		return nil, fmt.Errorf("CRC32 checksum mismatch: frame corrupted")
	}

	compactMetrics := flags&FlagCompactMetrics != 0

	// -- Parse variable header with compact IDs --
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

	// Routing metadata (4 bytes — Degraded is in the flags byte when compact metrics are used).
	var metaBytes [4]byte
	if _, err := io.ReadFull(r, metaBytes[:]); err != nil {
		return nil, fmt.Errorf("read routing metadata: %w", err)
	}
	p.Version = metaBytes[0]
	p.PacketType = metaBytes[1]
	p.MaxHops = metaBytes[2]
	p.HopCount = metaBytes[3]

	// Destination + current node (compact IDs).
	var err error
	if p.Destination, err = binReadNodeID(r, nit); err != nil {
		return nil, fmt.Errorf("read destination: %w", err)
	}
	if p.CurrentNode, err = binReadNodeID(r, nit); err != nil {
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
		if p.PlannedPath[i], err = binReadNodeID(r, nit); err != nil {
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
		if p.MiniMap[i].From, err = binReadNodeID(r, nit); err != nil {
			return nil, fmt.Errorf("read map[%d].from: %w", i, err)
		}
		if p.MiniMap[i].To, err = binReadNodeID(r, nit); err != nil {
			return nil, fmt.Errorf("read map[%d].to: %w", i, err)
		}
		if compactMetrics {
			if p.MiniMap[i].LatencyMs, err = decodeMetricU16(r); err != nil {
				return nil, fmt.Errorf("read map[%d].latency: %w", i, err)
			}
			if p.MiniMap[i].LoadPct, err = decodeMetricU16(r); err != nil {
				return nil, fmt.Errorf("read map[%d].load: %w", i, err)
			}
			if p.MiniMap[i].LossPct, err = decodeMetricU16(r); err != nil {
				return nil, fmt.Errorf("read map[%d].loss: %w", i, err)
			}
		} else {
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
		if p.CongestionLog[i].NodeName, err = binReadNodeID(r, nit); err != nil {
			return nil, fmt.Errorf("read log[%d].name: %w", i, err)
		}
		if compactMetrics {
			if p.CongestionLog[i].LoadPct, err = decodeMetricU16(r); err != nil {
				return nil, fmt.Errorf("read log[%d].load: %w", i, err)
			}
			if p.CongestionLog[i].LatencyMs, err = decodeMetricU16(r); err != nil {
				return nil, fmt.Errorf("read log[%d].latency: %w", i, err)
			}
		} else {
			if err := binary.Read(r, binary.BigEndian, &p.CongestionLog[i].LoadPct); err != nil {
				return nil, fmt.Errorf("read log[%d].load: %w", i, err)
			}
			if err := binary.Read(r, binary.BigEndian, &p.CongestionLog[i].LatencyMs); err != nil {
				return nil, fmt.Errorf("read log[%d].latency: %w", i, err)
			}
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
		node, err := binReadNodeID(r, nit)
		if err != nil {
			return nil, fmt.Errorf("read visited[%d]: %w", i, err)
		}
		p.VisitedNodes[node] = true
	}

	// Timestamp (8 bytes).
	if err := binary.Read(r, binary.BigEndian, &p.CreatedAtNs); err != nil {
		return nil, fmt.Errorf("read created_at: %w", err)
	}

	// -- Apply fixed header fields --
	p.PacketType = packetType
	if flags&FlagDegraded != 0 {
		p.Degraded = true
	}
	if flags&FlagRerouted != 0 {
		p.Rerouted = true
	}
	if flags&FlagLightMode != 0 {
		p.LightMode = true
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
