package kexecnetboot

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
)

type selector struct {
	in          *bufio.Reader
	out, err    io.Writer
	interactive bool
	defaultFQDN string
}

func (s selector) line() (string, bool, error) {
	line, err := s.in.ReadString('\n')
	if err == io.EOF && line != "" {
		return line, true, nil
	}
	if err == io.EOF {
		return "", false, nil
	}
	return line, err == nil, err
}
func (s selector) machine(index record, requested *string) (record, error) {
	ms, err := records(index["machines"], "machines", false)
	if err != nil {
		return nil, err
	}
	if len(ms) == 0 {
		return nil, nil
	}
	for _, m := range ms {
		if _, err := requiredString(m, "fqdn"); err != nil {
			return nil, err
		}
	}
	if requested == nil && s.interactive {
		var def record
		for _, m := range ms {
			if m["fqdn"] == s.defaultFQDN {
				def = m
				break
			}
		}
		for {
			fmt.Fprintf(s.out, "%5s  %-30s  %s\n", "", "FQDN", "SPIN")
			if def == nil {
				return nil, fmt.Errorf("compiled default machine is missing from index")
			}
			for i, m := range ms {
				mark := ""
				if m["fqdn"] == def["fqdn"] {
					mark = "*"
				}
				fmt.Fprintf(s.out, "%5s  %-30s  %s\n", fmt.Sprintf("[%d]%s", i+1, mark), display(m["fqdn"]), display(m["spin"]))
			}
			fmt.Fprint(s.out, "Select a machine by number: ")
			line, ok, err := s.line()
			if err != nil || !ok {
				return nil, err
			}
			i := toInt(strip(line))
			if i.Sign() == 0 {
				return def, nil
			}
			if i.Sign() > 0 && i.Cmp(big.NewInt(int64(len(ms)))) <= 0 {
				return ms[int(i.Int64())-1], nil
			}
			fmt.Fprintln(s.out, "Invalid selection. Please try again.")
		}
	}
	name := s.defaultFQDN
	if requested != nil {
		name = *requested
	}
	for _, m := range ms {
		if m["fqdn"] == name {
			return m, nil
		}
	}
	fmt.Fprintf(s.err, "Machine '%s' not found.\n", name)
	return nil, nil
}
func selectGeneration(input string, gens []record) record {
	n, ok := integer(input)
	if !ok {
		return nil
	}
	if n.Sign() >= 0 {
		for _, g := range gens {
			if numberEquals(g["generation"], n) {
				return g
			}
		}
		return nil
	}
	offset := new(big.Int).Neg(n)
	if offset.Cmp(big.NewInt(int64(len(gens)))) >= 0 {
		return nil
	}
	return gens[int(offset.Int64())]
}
func (s selector) generation(machine record, requested *string) (record, error) {
	gs, err := records(machine["generations"], "generations", true)
	if err != nil {
		return nil, err
	}
	if len(gs) == 0 {
		return nil, nil
	}
	for _, g := range gs {
		if _, ok := g["generation"].(json.Number); !ok {
			return nil, fmt.Errorf("generation must be a number")
		}
	}
	if requested != nil {
		g := selectGeneration(*requested, gs)
		if g == nil {
			fmt.Fprintf(s.err, "Requested generation '%s' not found (or invalid).\n", *requested)
		}
		return g, nil
	}
	var current record
	for _, g := range gs {
		if g["current"] == true {
			current = g
			break
		}
	}
	def := current
	if def == nil {
		def = gs[0]
	}
	if !s.interactive {
		return def, nil
	}
	for {
		label := fmt.Sprintf("(no current labeled, default is newest: %s)", display(gs[0]["generation"]))
		if current != nil {
			label = fmt.Sprintf("(default is %s - current)", display(current["generation"]))
		}
		fmt.Fprintf(s.out, "Available generations for %s: %s\n", display(machine["fqdn"]), label)
		fmt.Fprintf(s.out, "%5s  %-19s  %-10s  %s\n", "", "TIME", "REVISION", "KERNEL")
		for _, g := range gs {
			mark := ""
			if truthy(g["current"]) {
				mark = "*"
			}
			fmt.Fprintf(s.out, "%5s  %-19s  %-10s  %s\n", "["+display(g["generation"])+"]"+mark, display(g["time_s"]), display(g["shortrev"]), display(g["kernel_version"]))
		}
		fmt.Fprint(s.out, "Enter generation number (or negative offset) [ENTER for default]: ")
		line, ok, err := s.line()
		if err != nil || !ok {
			return nil, err
		}
		line = strip(line)
		if line == "" {
			return def, nil
		}
		if g := selectGeneration(line, gs); g != nil {
			return g, nil
		}
		fmt.Fprintf(s.out, "Invalid generation '%s'. Please try again.\n", line)
	}
}
func (s selector) variant(g record, requested *string) (record, error) {
	vs, err := records(g["variants"], "variants", true)
	if err != nil {
		return nil, err
	}
	if len(vs) == 0 {
		return nil, nil
	}
	for _, v := range vs {
		if _, err := requiredString(v, "name"); err != nil {
			return nil, err
		}
	}
	if requested != nil {
		for _, v := range vs {
			if v["name"] == *requested {
				return v, nil
			}
		}
		fmt.Fprintf(s.err, "Requested variant '%s' not found in generation %s.\n", *requested, display(g["generation"]))
		return nil, nil
	}
	if !s.interactive {
		return vs[0], nil
	}
	for {
		fmt.Fprintf(s.out, "Variants available for generation %s:\n", display(g["generation"]))
		fmt.Fprintf(s.out, "%5s  %s\n", "", "LABEL")
		for i, v := range vs {
			fmt.Fprintf(s.out, "%5s  %s\n", fmt.Sprintf("[%d]", i+1), display(v["label"]))
		}
		fmt.Fprintf(s.out, "Choose variant by number [ENTER for '%s']: ", display(vs[0]["name"]))
		line, ok, err := s.line()
		if err != nil || !ok {
			return nil, err
		}
		line = strip(line)
		if line == "" {
			return vs[0], nil
		}
		i := toInt(line)
		if i.Sign() > 0 && i.Cmp(big.NewInt(int64(len(vs)))) <= 0 {
			return vs[int(i.Int64())-1], nil
		}
		fmt.Fprintln(s.out, "Invalid selection. Please try again.")
	}
}
