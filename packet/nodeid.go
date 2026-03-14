package packet

import (
	"encoding/json"
	"fmt"
)

// NodeIDTable provides bidirectional mapping between node name strings
// and compact uint16 IDs for wire-level compression.
type NodeIDTable struct {
	nameToID map[string]uint16
	idToName map[uint16]string
}

// NewNodeIDTable creates a NodeIDTable from a name→ID mapping.
// ID 0 is reserved as a sentinel for empty strings — user IDs must start at 1.
func NewNodeIDTable(mapping map[string]uint16) *NodeIDTable {
	t := &NodeIDTable{
		nameToID: make(map[string]uint16, len(mapping)),
		idToName: make(map[uint16]string, len(mapping)),
	}
	for name, id := range mapping {
		if id == 0 {
			continue // ID 0 is reserved for empty-string sentinel
		}
		t.nameToID[name] = id
		t.idToName[id] = name
	}
	return t
}

// ToID returns the compact ID for a node name.
func (t *NodeIDTable) ToID(name string) (uint16, bool) {
	if t == nil {
		return 0, false
	}
	id, ok := t.nameToID[name]
	return id, ok
}

// ToName returns the node name for a compact ID.
func (t *NodeIDTable) ToName(id uint16) (string, bool) {
	if t == nil {
		return "", false
	}
	name, ok := t.idToName[id]
	return name, ok
}

// LoadNodeIDTableFromJSON parses a JSON object like {"sender": 1, "router_a": 2, ...}
// into a NodeIDTable.
func LoadNodeIDTableFromJSON(data []byte) (*NodeIDTable, error) {
	var mapping map[string]uint16
	if err := json.Unmarshal(data, &mapping); err != nil {
		return nil, fmt.Errorf("parse node ID map: %w", err)
	}
	return NewNodeIDTable(mapping), nil
}
