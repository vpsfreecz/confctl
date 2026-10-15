// Package gendata projects the fields consumed by gen-data vpsadmin.
//
// It consumes an ordered machine snapshot and an injected, already authenticated
// source. Description validation, association resolution, HTTP and CLI routing
// belong to callers. Generator preserves the fixed .new-and-rename workflow.
// A missing scalar projects as null; a missing association that the Ruby
// workflow dereferences is an error.
package gendata

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strings"
)

// Field preserves absent, explicit null and present values. Absent fields have
// Present=false and Value=nil; explicit null has Present=true and Value=nil.
// A value with Present=false is an invalid DTO construction.
type Field[T any] struct {
	Present bool
	Value   *T
	// read is private to the consumed provider. It runs at this field's source
	// access point, preserving pure fixture construction and lazy error order.
	read func() (*T, error)
	// scalar preserves a provider getter's decoded primitive without claiming
	// the already-decoded typed fixtures cover every source scalar domain.
	scalar func() (any, error)
}

type ContainerRef struct {
	ID Field[json.Number]
}

// MachineInput deliberately has no managed, runnable or carrier filter. The
// caller supplies all entries in inventory order, using structured attributes
// rather than the core Machine.Attr display accessor.
type MachineInput struct {
	Name         string
	Container    Field[ContainerRef]
	HostName     Field[string]
	HostLocation Field[string]
	HostDomain   Field[string]
}

type VPS struct {
	Node Field[Node]
}

type Node struct {
	ID         Field[json.Number]
	Name       Field[string]
	DomainName Field[string]
	Location   Field[Location]
}

type Location struct {
	Domain      Field[string]
	Environment Field[Environment]
}

type Environment struct {
	Domain Field[string]
}

type Network struct {
	// IPVersion retains the actual primitive: an integral json.Number, float64,
	// string or bool. Null is represented by Field, not by a pointer to nil any.
	// G2 owns decoding; nonnumeric primitives are unmatched, not coerced to zero.
	IPVersion Field[any]
	Address   Field[string]
	Prefix    Field[json.Number]
}

// Source resolves the consumed node/location/environment chain in ShowVPS,
// retaining per-entry includes and association behavior without cross-entry
// caching. ListNetworks returns exactly one response in server order.
type Source interface {
	ShowVPS(context.Context, Field[json.Number]) (VPS, error)
	ListNetworks(context.Context) ([]Network, error)
}

// Containers calls ShowVPS sequentially for each non-null container, projects
// that response before the next call, and lets the last duplicate FQDN win.
// It makes no file writes, and returns no partial projection on failure.
func Containers(ctx context.Context, source Source, machines []MachineInput) (map[string]any, error) {
	data := make(map[string]any)
	for _, machine := range machines {
		container, err := fieldValue(machine.Container, "container")
		if err != nil {
			return nil, fmt.Errorf("machine %q: %w", machine.Name, err)
		}
		if container == nil {
			continue
		}
		if _, err := numberValue(container.ID, "container.id"); err != nil {
			return nil, fmt.Errorf("machine %q: %w", machine.Name, err)
		}
		if source == nil {
			return nil, fmt.Errorf("machine %q: gen-data source is not configured", machine.Name)
		}
		vps, err := source.ShowVPS(ctx, container.ID)
		if err != nil {
			return nil, fmt.Errorf("machine %q: show VPS: %w", machine.Name, err)
		}
		key, row, err := ProjectContainer(machine, vps)
		if err != nil {
			return nil, fmt.Errorf("machine %q: %w", machine.Name, err)
		}
		data[key] = row
	}
	return data, nil
}

// ProjectContainer is the pure per-row projection. Host components omit only
// absent/null values. Empty strings remain, including an empty resulting key.
func ProjectContainer(machine MachineInput, vps VPS) (string, map[string]any, error) {
	parts := make([]string, 0, 3)
	for _, host := range []struct {
		path  string
		field Field[string]
	}{
		{"host.name", machine.HostName},
		{"host.location", machine.HostLocation},
		{"host.domain", machine.HostDomain},
	} {
		value, err := scalarValue(host.field, host.path)
		if err != nil {
			return "", nil, err
		}
		if value != nil {
			text, err := scalarText(value)
			if err != nil {
				return "", nil, err
			}
			parts = append(parts, text)
		}
	}

	node, err := association(vps.Node, "node")
	if err != nil {
		return "", nil, err
	}
	id, err := numberValue(node.ID, "node.id")
	if err != nil {
		return "", nil, err
	}
	name, err := scalarValue(node.Name, "node.name")
	if err != nil {
		return "", nil, err
	}
	location, err := association(node.Location, "node.location")
	if err != nil {
		return "", nil, err
	}
	locationDomain, err := scalarValue(location.Domain, "node.location.domain")
	if err != nil {
		return "", nil, err
	}
	environment, err := association(location.Environment, "node.location.environment")
	if err != nil {
		return "", nil, err
	}
	domain, err := scalarValue(environment.Domain, "node.location.environment.domain")
	if err != nil {
		return "", nil, err
	}
	domainName, err := scalarValue(node.DomainName, "node.domain_name")
	if err != nil {
		return "", nil, err
	}
	return strings.Join(parts, "."), map[string]any{
		"node": map[string]any{
			"id":       id,
			"name":     name,
			"location": locationDomain,
			"domain":   domain,
			"fqdn":     interpolation(domainName) + "." + interpolation(domain),
		},
	}, nil
}

// Networks obtains one unfiltered list and then applies the pure projection.
func Networks(ctx context.Context, source Source) (map[string]any, error) {
	if source == nil {
		return nil, fmt.Errorf("gen-data source is not configured")
	}
	networks, err := source.ListNetworks(ctx)
	if err != nil {
		return nil, fmt.Errorf("list networks: %w", err)
	}
	return ProjectNetworks(networks)
}

// ProjectNetworks partitions numeric 4/6 in that order. Rows remain in server
// order within each family; address/prefix of unselected rows are never read.
func ProjectNetworks(networks []Network) (map[string]any, error) {
	data := make(map[string]any, 2)
	for _, family := range []int{4, 6} {
		selected := make([]int, 0)
		for i, network := range networks {
			version, err := networkVersion(network.IPVersion)
			if err != nil {
				return nil, fmt.Errorf("network %d: %w", i, err)
			}
			if version == family {
				selected = append(selected, i)
			}
		}
		rows := make([]any, 0, len(selected))
		for _, i := range selected {
			network := networks[i]
			address, err := scalarValue(network.Address, "address")
			if err != nil {
				return nil, fmt.Errorf("network %d: %w", i, err)
			}
			prefix, err := numberValue(network.Prefix, "prefix")
			if err != nil {
				return nil, fmt.Errorf("network %d: %w", i, err)
			}
			rows = append(rows, map[string]any{"address": address, "prefix": prefix})
		}
		data[fmt.Sprintf("ipv%d", family)] = rows
	}
	return data, nil
}

func fieldValue[T any](field Field[T], path string) (*T, error) {
	if field.read != nil {
		return field.read()
	}
	if !field.Present && field.Value != nil {
		return nil, fmt.Errorf("%s: value supplied for absent field", path)
	}
	return field.Value, nil
}

func association[T any](field Field[T], path string) (*T, error) {
	value, err := fieldValue(field, path)
	if err != nil {
		return nil, err
	}
	if value == nil {
		return nil, fmt.Errorf("%s: missing association", path)
	}
	return value, nil
}

func scalarValue[T any](field Field[T], path string) (any, error) {
	if field.scalar != nil {
		return field.scalar()
	}
	value, err := fieldValue(field, path)
	if err != nil || value == nil {
		return nil, err
	}
	return *value, nil
}

func numberValue(field Field[json.Number], path string) (any, error) {
	if field.scalar != nil {
		return field.scalar()
	}
	value, err := fieldValue(field, path)
	if err != nil || value == nil {
		return nil, err
	}
	if !integralToken(string(*value)) {
		return nil, fmt.Errorf("%s: expected integral JSON number, got %q", path, *value)
	}
	return *value, nil
}

// DTO numeric fields carry integral JSON tokens, without native integer bounds
// or float conversion. This also prevents an invalid hand-constructed Number
// from reaching the existing literal formatter's raw-number branch.
func integralToken(token string) bool {
	if strings.HasPrefix(token, "-") {
		token = token[1:]
	}
	if token == "" || (len(token) > 1 && token[0] == '0') {
		return false
	}
	for _, c := range token {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

func interpolation(value any) string {
	if value == nil {
		return ""
	}
	text, _ := scalarText(value)
	return text
}

func networkVersion(field Field[any]) (int, error) {
	value, err := fieldValue(field, "ip_version")
	if err != nil || value == nil {
		return 0, err
	}
	switch version := (*value).(type) {
	case json.Number:
		if !integralToken(string(version)) {
			return 0, fmt.Errorf("ip_version: expected integral JSON token, got %q", version)
		}
		if version == "4" {
			return 4, nil
		}
		if version == "6" {
			return 6, nil
		}
	case float64:
		if math.IsNaN(version) || math.IsInf(version, 0) {
			return 0, fmt.Errorf("ip_version: non-finite float")
		}
		if version == 4 || version == 6 {
			return int(version), nil
		}
	case string, bool:
		// Preserve primitive identity: "4" and true are not the number 4.
	default:
		return 0, fmt.Errorf("ip_version: unsupported primitive %T", *value)
	}
	return 0, nil
}
