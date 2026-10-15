package gendata

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
)

// Only the consumed description members are interpreted, when their action or
// getter is reached. RawMessage keeps exact numbers and unconsumed fields lazy.
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

func descriptionString(obj map[string]json.RawMessage, key string) (string, error) {
	var value string
	if json.Unmarshal(obj[key], &value) != nil || absentOrNull(obj[key]) {
		return "", errors.New("malformed consumed description string")
	}
	return value, nil
}

type apiAction struct {
	method, path  string
	input, output map[string]json.RawMessage
}

func parseAction(raw json.RawMessage) (apiAction, error) {
	obj, err := rawObject(raw)
	if err != nil {
		return apiAction{}, errors.New("missing or malformed consumed action")
	}
	a := apiAction{}
	if a.method, err = descriptionString(obj, "method"); err != nil {
		return a, err
	}
	switch a.method {
	case "GET", "DELETE", "POST", "PUT":
	default:
		return a, errors.New("unsupported consumed HTTP method")
	}
	if a.path, err = descriptionString(obj, "path"); err != nil {
		return a, err
	}
	if !absentOrNull(obj["input"]) {
		if a.input, err = rawObject(obj["input"]); err != nil {
			return a, errors.New("malformed consumed action input")
		}
	}
	if a.output, err = rawObject(obj["output"]); err != nil {
		return a, errors.New("missing or malformed consumed action output")
	}
	layout, err := descriptionString(a.output, "layout")
	if err != nil {
		return a, errors.New("missing consumed output layout")
	}
	switch layout {
	case "object", "object_list", "hash", "hash_list":
	default:
		return a, errors.New("unsupported consumed output layout")
	}
	return a, nil
}

func actionLayout(action apiAction, allowed ...string) error {
	layout, err := descriptionString(action.output, "layout")
	if err != nil {
		return err
	}
	for _, name := range allowed {
		if layout == name {
			return nil
		}
	}
	return errors.New("unexpected consumed action output layout")
}

func actionFrom(resource json.RawMessage, name string) (apiAction, error) {
	r, err := rawObject(resource)
	if err != nil {
		return apiAction{}, errors.New("missing consumed resource")
	}
	actions, err := objectMembers(r["actions"])
	if err != nil {
		return apiAction{}, errors.New("malformed consumed resource actions")
	}
	for _, action := range actions {
		if action.name == name {
			return parseAction(action.raw)
		}
	}
	for _, action := range actions {
		obj, err := rawObject(action.raw)
		if err != nil {
			return apiAction{}, err
		}
		var aliases []string
		if !absentOrNull(obj["aliases"]) && json.Unmarshal(obj["aliases"], &aliases) != nil {
			return apiAction{}, errors.New("malformed action aliases")
		}
		for _, alias := range aliases {
			if alias == name {
				return parseAction(action.raw)
			}
		}
	}
	return apiAction{}, errors.New("consumed action is unavailable")
}

func resourceAt(description json.RawMessage, path []string) (json.RawMessage, error) {
	current := description
	for _, name := range path {
		obj, err := rawObject(current)
		if err != nil {
			return nil, errors.New("malformed resource description")
		}
		resources, err := rawObject(obj["resources"])
		if err != nil || len(resources[name]) == 0 {
			return nil, errors.New("consumed resource is unavailable")
		}
		current = resources[name]
	}
	return current, nil
}

// Protocol matching uses the source's major component, without fetching a
// version list or interpreting action paths as a hardcoded API version.
func (p *VPSAdmin) descriptionVersion(obj map[string]json.RawMessage) error {
	version, err := descriptionString(obj, "version")
	if err != nil || strings.Split(version, ".")[0] != "2" {
		return errors.New("incompatible HaveAPI protocol")
	}
	if version != "2.0" && p.warn != nil {
		p.warn("The client uses protocol v2.0 while the API server uses a different same-major version")
	}
	return nil
}
