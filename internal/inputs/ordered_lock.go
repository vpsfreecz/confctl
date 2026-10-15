package inputs

import (
	"encoding/json"
	"fmt"
	"math/big"
)

// This private codec preserves JSON2.21.2's ordered, byte-string lock values.
// It is deliberately separate from the existing read-only Lock decoder. No
// filesystem or setter operations use it until their own integration is accepted.
type orderedLockDocument struct{ root *orderedLockValue }

type orderedLockKind uint8

const (
	orderedNull orderedLockKind = iota
	orderedBool
	orderedString
	orderedInteger
	orderedFloat
	orderedObject
	orderedArray
)

type orderedLockMember struct {
	key   string
	value *orderedLockValue
}

type orderedLockValue struct {
	kind     orderedLockKind
	boolean  bool
	text     string // bytes, including strings the original parser accepts as invalid UTF-8
	integer  *big.Int
	float    float64
	members  []orderedLockMember
	index    map[string]int
	elements []*orderedLockValue
}

type orderedLockError struct {
	phase   string
	kind    string
	message string
	cursor  int // byte offset; -1 for diagnostics without a source cursor
	line    int
	column  int
}

func (e *orderedLockError) Error() string { return e.message }

func (d *orderedLockDocument) clone() *orderedLockDocument {
	return &orderedLockDocument{root: d.root.clone()}
}

func (v *orderedLockValue) clone() *orderedLockValue {
	if v == nil {
		return nil
	}
	c := *v
	if v.integer != nil {
		c.integer = new(big.Int).Set(v.integer)
	}
	if v.kind == orderedObject {
		c.members = make([]orderedLockMember, len(v.members))
		c.index = make(map[string]int, len(v.members))
		for i, m := range v.members {
			c.members[i] = orderedLockMember{m.key, m.value.clone()}
			c.index[m.key] = i
		}
	}
	if v.kind == orderedArray {
		c.elements = make([]*orderedLockValue, len(v.elements))
		for i, e := range v.elements {
			c.elements[i] = e.clone()
		}
	}
	return &c
}

func (v *orderedLockValue) lookup(key string) (*orderedLockValue, bool) {
	i, ok := v.index[key]
	if !ok {
		return nil, false
	}
	return v.members[i].value, true
}

// set owns a detached copy. Replacing a duplicate retains the first position.
func (v *orderedLockValue) set(key string, value *orderedLockValue) {
	v.setOwned(key, value.clone())
}

// The parser alone transfers newly constructed values without another clone.
func (v *orderedLockValue) setOwned(key string, value *orderedLockValue) {
	if v.kind != orderedObject {
		panic("ordered lock member on nonobject")
	}
	if v.index == nil {
		v.index = make(map[string]int)
	}
	if i, ok := v.index[key]; ok {
		v.members[i].value = value
		return
	}
	v.index[key] = len(v.members)
	v.members = append(v.members, orderedLockMember{key, value})
}

func (v *orderedLockValue) delete(key string) bool {
	i, ok := v.index[key]
	if !ok {
		return false
	}
	copy(v.members[i:], v.members[i+1:])
	v.members[len(v.members)-1] = orderedLockMember{}
	v.members = v.members[:len(v.members)-1]
	delete(v.index, key)
	for j := i; j < len(v.members); j++ {
		v.index[v.members[j].key] = j
	}
	return true
}

// lockView is a detached read projection, never a serialization round trip.
// Scalar roots are accepted by parsing and rejected only at this consumer.
func (d *orderedLockDocument) lockView() (*Lock, error) {
	if d.root.kind != orderedObject {
		return nil, fmt.Errorf("expected lock object")
	}
	return &Lock{data: d.root.view().(map[string]any)}, nil
}

func (v *orderedLockValue) view() any {
	switch v.kind {
	case orderedNull:
		return nil
	case orderedBool:
		return v.boolean
	case orderedString:
		return v.text
	case orderedInteger:
		return json.Number(v.integer.String())
	case orderedFloat:
		return v.float
	case orderedObject:
		out := make(map[string]any, len(v.members))
		for _, m := range v.members {
			out[m.key] = m.value.view()
		}
		return out
	case orderedArray:
		out := make([]any, len(v.elements))
		for i, e := range v.elements {
			out[i] = e.view()
		}
		return out
	default:
		panic("invalid ordered lock tag")
	}
}
