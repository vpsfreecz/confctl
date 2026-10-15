package gendata

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"reflect"
	"strings"
	"testing"
)

// These literal expectations are source-derived from cli/gen_data.rb and the
// declared DTO contract. They are not captured original-runtime observations.
func some[T any](value T) Field[T] { return Field[T]{Present: true, Value: &value} }
func null[T any]() Field[T]        { return Field[T]{Present: true} }

func resolvedVPS(id, name, domainName, location, domain string) VPS {
	return VPS{Node: some(Node{
		ID: some(json.Number(id)), Name: some(name), DomainName: some(domainName),
		Location: some(Location{Domain: some(location), Environment: some(Environment{Domain: some(domain)})}),
	})}
}

type recordingSource struct {
	ids       []Field[json.Number]
	responses []VPS
	failAt    int
	failure   error
	listCalls int
	networks  []Network
	listErr   error
	ctx       context.Context
}

func (s *recordingSource) ShowVPS(ctx context.Context, id Field[json.Number]) (VPS, error) {
	if ctx != s.ctx {
		return VPS{}, errors.New("wrong show context")
	}
	s.ids = append(s.ids, id)
	if s.failAt == len(s.ids) {
		return VPS{}, s.failure
	}
	return s.responses[len(s.ids)-1], nil
}

func (s *recordingSource) ListNetworks(ctx context.Context) ([]Network, error) {
	if ctx != s.ctx {
		return nil, errors.New("wrong list context")
	}
	s.listCalls++
	return s.networks, s.listErr
}

func TestGendataContainersOrderedRequestsAndDuplicateOverwrite(t *testing.T) {
	ctx := context.WithValue(context.Background(), struct{}{}, "invocation")
	id := some(json.Number("31"))
	machines := []MachineInput{
		{Name: "absent"},
		{Name: "null", Container: null[ContainerRef]()},
		{Name: "unmanaged", Container: some(ContainerRef{ID: id}), HostName: some("z"), HostLocation: some("loc"), HostDomain: some("test")},
		{Name: "carried", Container: some(ContainerRef{ID: id}), HostName: some("z"), HostLocation: some("loc"), HostDomain: some("test")},
		{Name: "empty-container", Container: some(ContainerRef{}), HostName: some("alpha")},
		{Name: "null-id", Container: some(ContainerRef{ID: null[json.Number]()}), HostName: some("zero")},
	}
	source := &recordingSource{ctx: ctx, responses: []VPS{
		resolvedVPS("7", "first", "n1", "old-loc", "old-env"),
		resolvedVPS("8", "final", "n2", "new-loc", "new-env"),
		{Node: some(Node{ID: some(json.Number("9007199254740993")), Name: some(""), Location: some(Location{Domain: null[string](), Environment: some(Environment{})})})},
		resolvedVPS("0", "zero", "", "", ""),
	}}
	beforeMachines, _ := json.Marshal(machines)
	beforeResponses, _ := json.Marshal(source.responses)
	got, err := Containers(ctx, source, machines)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"z.loc.test": map[string]any{"node": map[string]any{"id": json.Number("8"), "name": "final", "location": "new-loc", "domain": "new-env", "fqdn": "n2.new-env"}},
		"alpha":      map[string]any{"node": map[string]any{"id": json.Number("9007199254740993"), "name": "", "location": nil, "domain": nil, "fqdn": "."}},
		"zero":       map[string]any{"node": map[string]any{"id": json.Number("0"), "name": "zero", "location": "", "domain": "", "fqdn": "."}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("projection = %#v, want %#v", got, want)
	}
	wantIDs := []Field[json.Number]{id, id, {}, null[json.Number]()}
	if !reflect.DeepEqual(source.ids, wantIDs) || source.listCalls != 0 {
		t.Fatalf("requests: ids=%#v list=%d", source.ids, source.listCalls)
	}
	afterMachines, _ := json.Marshal(machines)
	afterResponses, _ := json.Marshal(source.responses)
	if string(beforeMachines) != string(afterMachines) || string(beforeResponses) != string(afterResponses) {
		t.Fatal("projection changed input DTOs")
	}
}

func TestGendataContainerHostComponents(t *testing.T) {
	for _, test := range []struct {
		name string
		m    MachineInput
		key  string
	}{
		{"absent", MachineInput{}, ""},
		{"null", MachineInput{HostName: null[string](), HostLocation: null[string](), HostDomain: null[string]()}, ""},
		{"empty", MachineInput{HostName: some(""), HostLocation: some(""), HostDomain: some("")}, ".."},
		{"null-middle", MachineInput{HostName: some("n"), HostLocation: null[string](), HostDomain: some("")}, "n."},
		{"missing-name", MachineInput{Name: "no-fallback", HostLocation: some("x"), HostDomain: some("d")}, "x.d"},
	} {
		t.Run(test.name, func(t *testing.T) {
			key, row, err := ProjectContainer(test.m, resolvedVPS("1", "node", "n", "l", "d"))
			if err != nil || key != test.key {
				t.Fatalf("key=%q err=%v, want %q", key, err, test.key)
			}
			want := map[string]any{"node": map[string]any{"id": json.Number("1"), "name": "node", "location": "l", "domain": "d", "fqdn": "n.d"}}
			if !reflect.DeepEqual(row, want) {
				t.Fatalf("row=%#v", row)
			}
		})
	}
}

func TestGendataContainerMissingAndNullScalars(t *testing.T) {
	for _, explicitNull := range []bool{false, true} {
		var id Field[json.Number]
		var text Field[string]
		if explicitNull {
			id, text = null[json.Number](), null[string]()
		}
		vps := VPS{Node: some(Node{ID: id, Name: text, DomainName: text, Location: some(Location{Domain: text, Environment: some(Environment{Domain: text})})})}
		_, got, err := ProjectContainer(MachineInput{}, vps)
		if err != nil {
			t.Fatal(err)
		}
		want := map[string]any{"node": map[string]any{"id": nil, "name": nil, "location": nil, "domain": nil, "fqdn": "."}}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("explicit-null=%t: got %#v", explicitNull, got)
		}
	}
}

func TestGendataContainerNullableFQDNComponents(t *testing.T) {
	for _, test := range []struct {
		name       string
		domainName Field[string]
		domain     Field[string]
		wantDomain any
		wantFQDN   string
	}{
		{"absent-node-domain", Field[string]{}, some("d"), "d", ".d"},
		{"null-node-domain", null[string](), some("d"), "d", ".d"},
		{"absent-environment-domain", some("n"), Field[string]{}, nil, "n."},
		{"null-environment-domain", some("n"), null[string](), nil, "n."},
		{"empty-environment-domain", some("n"), some(""), "", "n."},
	} {
		t.Run(test.name, func(t *testing.T) {
			vps := resolvedVPS("1", "name", "unused", "location", "unused")
			vps.Node.Value.DomainName = test.domainName
			vps.Node.Value.Location.Value.Environment.Value.Domain = test.domain
			_, got, err := ProjectContainer(MachineInput{}, vps)
			want := map[string]any{"node": map[string]any{
				"id": json.Number("1"), "name": "name", "location": "location",
				"domain": test.wantDomain, "fqdn": test.wantFQDN,
			}}
			if err != nil || !reflect.DeepEqual(got, want) {
				t.Fatalf("got=%#v err=%v, want=%#v", got, err, want)
			}
		})
	}
}

func TestGendataContainerMissingAssociationsAndAccessOrder(t *testing.T) {
	for _, test := range []struct {
		name string
		vps  VPS
		path string
	}{
		{"absent-node", VPS{}, "node: missing association"},
		{"null-node", VPS{Node: null[Node]()}, "node: missing association"},
		{"absent-location", VPS{Node: some(Node{})}, "node.location: missing association"},
		{"null-location", VPS{Node: some(Node{Location: null[Location]()})}, "node.location: missing association"},
		{"absent-environment", VPS{Node: some(Node{Location: some(Location{})})}, "node.location.environment: missing association"},
		{"null-environment", VPS{Node: some(Node{Location: some(Location{Environment: null[Environment]()})})}, "node.location.environment: missing association"},
		{"id-before-location", VPS{Node: some(Node{ID: some(json.Number("bad"))})}, "node.id:"},
		{"name-before-location", VPS{Node: some(Node{Name: Field[string]{Value: some("bad").Value}})}, "node.name:"},
		{"location-before-domain-name", VPS{Node: some(Node{DomainName: Field[string]{Value: some("bad").Value}})}, "node.location: missing association"},
		{"location-domain-before-environment", VPS{Node: some(Node{Location: some(Location{Domain: Field[string]{Value: some("bad").Value}})})}, "node.location.domain:"},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, row, err := ProjectContainer(MachineInput{}, test.vps)
			if err == nil || !strings.Contains(err.Error(), test.path) || row != nil {
				t.Fatalf("row=%#v err=%v, want %s", row, err, test.path)
			}
		})
	}
}

func TestGendataContainersStopOnReachedFailure(t *testing.T) {
	ctx := context.Background()
	failure := errors.New("show failed")
	machines := []MachineInput{
		{Name: "first", Container: some(ContainerRef{ID: some(json.Number("1"))})},
		{Name: "second", Container: some(ContainerRef{ID: some(json.Number("2"))})},
		{Name: "third", Container: some(ContainerRef{ID: some(json.Number("3"))})},
	}
	for _, failAt := range []int{1, 2} {
		source := &recordingSource{ctx: ctx, responses: []VPS{resolvedVPS("1", "n", "n", "l", "d")}, failAt: failAt, failure: failure}
		got, err := Containers(ctx, source, machines)
		if got != nil || !errors.Is(err, failure) || len(source.ids) != failAt {
			t.Fatalf("failAt=%d: got=%#v err=%v calls=%d", failAt, got, err, len(source.ids))
		}
	}
	// Even a later duplicate must not be fetched after an earlier projection
	// error. No map containing an earlier successful row is returned on failure.
	source := &recordingSource{ctx: ctx, responses: []VPS{resolvedVPS("1", "n", "n", "l", "d"), {}}}
	got, err := Containers(ctx, source, machines)
	if got != nil || err == nil || len(source.ids) != 2 {
		t.Fatalf("projection failure: got=%#v err=%v calls=%d", got, err, len(source.ids))
	}
	// host conversion is reached after the source call, not as an inventory
	// preflight that suppresses an earlier request.
	machines[0].HostName = Field[string]{Value: some("invalid").Value}
	source = &recordingSource{ctx: ctx, responses: []VPS{resolvedVPS("1", "n", "n", "l", "d")}}
	if _, err := Containers(ctx, source, machines); err == nil || len(source.ids) != 1 {
		t.Fatalf("host failure: err=%v calls=%d", err, len(source.ids))
	}
}

func TestGendataExactNumbersAndInvalidConstruction(t *testing.T) {
	for _, token := range []string{"0", "-0", "-1", "9007199254740993", "184467440737095516160000000000000000000"} {
		vps := resolvedVPS(token, "n", "n", "l", "d")
		_, got, err := ProjectContainer(MachineInput{}, vps)
		if err != nil || got["node"].(map[string]any)["id"] != json.Number(token) {
			t.Fatalf("token=%q: got=%#v err=%v", token, got, err)
		}
	}
	for _, token := range []string{"", "-", "+1", "01", "1.0", "1e0", "NaN", "4; builtins.abort \"injection\""} {
		ctx := context.Background()
		machine := MachineInput{Container: some(ContainerRef{ID: some(json.Number(token))})}
		source := &recordingSource{ctx: ctx}
		if _, err := Containers(ctx, source, []MachineInput{machine}); err == nil || len(source.ids) != 0 {
			t.Fatalf("invalid token=%q: err=%v source calls=%d", token, err, len(source.ids))
		}
	}
	ctx := context.Background()
	for _, machine := range []MachineInput{
		{Container: Field[ContainerRef]{Value: some(ContainerRef{}).Value}},
		{Container: some(ContainerRef{ID: Field[json.Number]{Value: some(json.Number("1")).Value}})},
	} {
		source := &recordingSource{ctx: ctx}
		if _, err := Containers(ctx, source, []MachineInput{machine}); err == nil || len(source.ids) != 0 {
			t.Fatalf("invalid field: err=%v source calls=%d", err, len(source.ids))
		}
	}
}

func TestGendataNetworksPrimitiveIdentityOrderAndNullableFields(t *testing.T) {
	rows := []Network{
		{IPVersion: some[any](json.Number("6")), Address: some("2001:db8:2::"), Prefix: some(json.Number("64"))},
		{IPVersion: some[any]("4"), Prefix: some(json.Number("invalid-unselected"))},
		{IPVersion: some[any](json.Number("4")), Address: some("192.0.2.0"), Prefix: some(json.Number("24"))},
		{IPVersion: some[any](float64(4)), Prefix: null[json.Number]()},
		{IPVersion: some[any](float64(6)), Address: some("2001:db8:1::"), Prefix: some(json.Number("0"))},
		{Address: Field[string]{Value: some("invalid-unselected").Value}},
		{IPVersion: null[any](), Prefix: some(json.Number("invalid-unselected"))},
		{IPVersion: some[any](json.Number("5")), Prefix: some(json.Number("invalid-unselected"))},
		{IPVersion: some[any](json.Number("4")), Address: some(""), Prefix: some(json.Number("0"))},
		{IPVersion: some[any]("6"), Prefix: some(json.Number("invalid-unselected"))},
		{IPVersion: some[any](false), Prefix: some(json.Number("invalid-unselected"))},
		{IPVersion: some[any](json.Number("6")), Address: some("2001:db8:2::"), Prefix: some(json.Number("64"))},
	}
	if _, ok := (*rows[0].IPVersion.Value).(json.Number); !ok {
		t.Fatal("integer primitive lost")
	}
	if _, ok := (*rows[3].IPVersion.Value).(float64); !ok {
		t.Fatal("float primitive lost")
	}
	if _, ok := (*rows[1].IPVersion.Value).(string); !ok {
		t.Fatal("string primitive lost")
	}
	if _, ok := (*rows[10].IPVersion.Value).(bool); !ok || rows[6].IPVersion.Value != nil || !rows[6].IPVersion.Present || rows[5].IPVersion.Present {
		t.Fatal("boolean/null/absent identity lost")
	}
	ctx := context.Background()
	source := &recordingSource{ctx: ctx, networks: rows}
	got, err := Networks(ctx, source)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"ipv4": []any{
			map[string]any{"address": "192.0.2.0", "prefix": json.Number("24")},
			map[string]any{"address": nil, "prefix": nil},
			map[string]any{"address": "", "prefix": json.Number("0")},
		},
		"ipv6": []any{
			map[string]any{"address": "2001:db8:2::", "prefix": json.Number("64")},
			map[string]any{"address": "2001:db8:1::", "prefix": json.Number("0")},
			map[string]any{"address": "2001:db8:2::", "prefix": json.Number("64")},
		},
	}
	if !reflect.DeepEqual(got, want) || source.listCalls != 1 || len(source.ids) != 0 {
		t.Fatalf("got=%#v calls=%d show=%d", got, source.listCalls, len(source.ids))
	}
}

func TestGendataNetworkConstructionAndFamilyAccessOrder(t *testing.T) {
	for _, version := range []any{nil, 4, int64(4), json.Number("4.0"), json.Number("4e0"), json.Number("bad"), math.NaN(), math.Inf(1), []any{}, map[string]any{}} {
		got, err := ProjectNetworks([]Network{{IPVersion: some(version)}})
		if err == nil || got != nil || !strings.Contains(err.Error(), "ip_version:") {
			t.Fatalf("invalid primitive %T(%v): got=%#v err=%v", version, version, got, err)
		}
	}
	if _, err := ProjectNetworks([]Network{{IPVersion: Field[any]{Value: some[any](json.Number("4")).Value}}}); err == nil {
		t.Fatal("value supplied for absent IPVersion accepted")
	}
	for _, prefix := range []Field[json.Number]{some(json.Number("1.0")), {Value: some(json.Number("1")).Value}} {
		if _, err := ProjectNetworks([]Network{{IPVersion: some[any](json.Number("4")), Prefix: prefix}}); err == nil || !strings.Contains(err.Error(), "prefix:") {
			t.Fatalf("invalid prefix: %v", err)
		}
	}
	// Source partitions all IPv4 before projecting IPv6. A malformed IPv6
	// address earlier in server order must not overtake an IPv4 prefix error.
	rows := []Network{
		{IPVersion: some[any](json.Number("6")), Address: Field[string]{Value: some("invalid").Value}},
		{IPVersion: some[any](json.Number("4")), Prefix: some(json.Number("bad"))},
	}
	if _, err := ProjectNetworks(rows); err == nil || !strings.Contains(err.Error(), "network 1: prefix:") {
		t.Fatalf("family access order: %v", err)
	}
	rows[1].Prefix = some(json.Number("184467440737095516160000"))
	rows[0].Address = null[string]()
	got, err := ProjectNetworks(rows)
	if err != nil || got["ipv4"].([]any)[0].(map[string]any)["prefix"] != *rows[1].Prefix.Value {
		t.Fatalf("exact prefix: got=%#v err=%v", got, err)
	}
	// select reads every version before map reads a selected row's fields.
	rows = []Network{
		{IPVersion: some[any](json.Number("4")), Prefix: some(json.Number("bad"))},
		{IPVersion: Field[any]{Value: some[any](json.Number("6")).Value}},
	}
	if _, err := ProjectNetworks(rows); err == nil || !strings.Contains(err.Error(), "network 1: ip_version:") {
		t.Fatalf("selection before row projection: %v", err)
	}
}

func TestGendataEmptyAndSourceErrors(t *testing.T) {
	ctx := context.Background()
	got, err := Containers(ctx, nil, []MachineInput{{}, {Container: null[ContainerRef]()}})
	if err != nil || !reflect.DeepEqual(got, map[string]any{}) {
		t.Fatalf("empty containers: got=%#v err=%v", got, err)
	}
	source := &recordingSource{ctx: ctx}
	got, err = Networks(ctx, source)
	if err != nil || !reflect.DeepEqual(got, map[string]any{"ipv4": []any{}, "ipv6": []any{}}) || source.listCalls != 1 {
		t.Fatalf("empty networks: got=%#v err=%v calls=%d", got, err, source.listCalls)
	}
	failure := errors.New("list failed")
	source.listErr = failure
	got, err = Networks(ctx, source)
	if got != nil || !errors.Is(err, failure) || source.listCalls != 2 {
		t.Fatalf("list failure: got=%#v err=%v calls=%d", got, err, source.listCalls)
	}
	if _, err := Containers(ctx, nil, []MachineInput{{Container: some(ContainerRef{})}}); err == nil {
		t.Fatal("missing source accepted")
	}
	if _, err := Networks(ctx, nil); err == nil {
		t.Fatal("missing source accepted")
	}
}
