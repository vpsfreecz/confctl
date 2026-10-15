package gendata

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
)

// Raw consumed members retain missing/null and exact numeric tokens. No runtime
// schema or descriptor is interpreted.
type namedJSON struct {
	name string
	raw  json.RawMessage
}

func objectMembers(raw json.RawMessage) ([]namedJSON, error) {
	d := json.NewDecoder(bytes.NewReader(raw))
	token, err := d.Token()
	if err != nil || token != json.Delim('{') {
		return nil, errors.New("expected JSON object")
	}
	var members []namedJSON
	positions := map[string]int{}
	for d.More() {
		key, err := d.Token()
		if err != nil {
			return nil, errors.New("invalid JSON object")
		}
		name, ok := key.(string)
		if !ok {
			return nil, errors.New("invalid JSON object key")
		}
		var value json.RawMessage
		if d.Decode(&value) != nil {
			return nil, errors.New("invalid JSON member")
		}
		if i, duplicate := positions[name]; duplicate {
			members[i].raw = value
		} else {
			positions[name] = len(members)
			members = append(members, namedJSON{name, value})
		}
	}
	if _, err := d.Token(); err != nil {
		return nil, errors.New("invalid JSON object")
	}
	if _, err := d.Token(); err != io.EOF {
		return nil, errors.New("trailing JSON data")
	}
	return members, nil
}

func rawObject(raw json.RawMessage) (map[string]json.RawMessage, error) {
	members, err := objectMembers(raw)
	if err != nil {
		return nil, err
	}
	ret := make(map[string]json.RawMessage, len(members))
	for _, m := range members {
		ret[m.name] = m.raw
	}
	return ret, nil
}

func jsonValue(raw json.RawMessage) (any, error) {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	var value any
	if d.Decode(&value) != nil {
		return nil, errors.New("invalid JSON value")
	}
	if _, err := d.Token(); err != io.EOF {
		return nil, errors.New("trailing JSON data")
	}
	return value, nil
}

func absentOrNull(raw json.RawMessage) bool {
	return len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
}

func responseString(obj map[string]json.RawMessage, key string) (string, error) {
	var value string
	if json.Unmarshal(obj[key], &value) != nil || absentOrNull(obj[key]) {
		return "", errors.New("malformed consumed response string")
	}
	return value, nil
}
