package netboot

import (
	"fmt"
	"math/big"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"
)

type Machine struct {
	Name, Spin, FQDN, Label, ShortLabel, URL string
	Generations                              []*Generation
	Current                                  *Generation
}
type Generation struct {
	ID                                                       *big.Int
	Link, Store, Toplevel, TimeS, URL                        string
	Time                                                     time.Time
	Current                                                  bool
	Version, Revision, ShortRev, KernelVersion, MACs, Inputs any
	Params                                                   []string
	Boot                                                     []BootFile
	Variants                                                 []Variant
	metadata                                                 object
}
type BootFile struct{ Name, Path, URL string }
type Variant struct {
	Name, Label string
	Params      []string
}

var profilePattern = regexp.MustCompile(`^confctl-(.+)-([0-9]+)-link$`)
var kernelPattern = regexp.MustCompile(`^/nix/store/[^-]+-linux-([^/]+)`)

func LoadMachines(c Config, d Dependencies) ([]*Machine, error) {
	names, err := d.FS.Entries(d.Profiles)
	if err != nil {
		return nil, err
	}
	machines := []*Machine{}
	byName := map[string]*Machine{}
	for _, name := range names {
		match := profilePattern.FindStringSubmatch(name)
		if match == nil {
			continue
		}
		m := byName[match[1]]
		if m == nil {
			m = &Machine{Name: match[1]}
			byName[m.Name] = m
			machines = append(machines, m)
		}
		id, _ := new(big.Int).SetString(match[2], 10)
		link := join(d.Profiles, name)
		g, err := loadGeneration(link, id, d)
		if err != nil {
			return nil, fmt.Errorf("generation %s: %w", name, err)
		}
		m.Generations = append(m.Generations, g)
	}
	// Mark the first enumerated target match before sorting, including duplicates.
	for _, m := range machines {
		current, err := d.FS.Realpath(join(d.Profiles, "confctl-"+m.Name))
		if err != nil {
			return nil, err
		}
		for _, g := range m.Generations {
			if g.Store == current {
				g.Current = true
				m.Current = g
				break
			}
		}
	}
	for _, m := range machines {
		if m.Current == nil {
			return nil, fmt.Errorf("Unable to find current generation of machine %s", m.Name)
		}
		sort.SliceStable(m.Generations, func(i, j int) bool { return m.Generations[i].ID.Cmp(m.Generations[j].ID) > 0 })
		meta := m.Current.metadata
		m.Spin = "nixos"
		if v, ok := meta.get("spin"); ok {
			s, ok := v.(string)
			if !ok {
				return nil, fmt.Errorf("spin must be a string")
			}
			m.Spin = s
		}
		m.FQDN = m.Name
		if _, ok := meta.get("fqdn"); ok {
			m.FQDN, err = requiredString(meta, "fqdn")
			if err != nil {
				return nil, err
			}
		}
		label, _ := meta.get("label")
		if !truthy(label) {
			label, _ = meta.get("fqdn")
		}
		if !truthy(label) {
			label = m.Name
		}
		var ok bool
		m.Label, ok = label.(string)
		if !ok {
			return nil, fmt.Errorf("label must be a string")
		}
		bits := strings.Split(strings.TrimRight(m.Label, "."), ".")
		if len(bits) > 2 {
			bits = bits[:2]
		}
		m.ShortLabel = strings.Join(bits, ".")
		m.URL = join(c.HTTPURL, m.FQDN)
		for _, g := range m.Generations {
			if err := resolveGeneration(m, g, d); err != nil {
				return nil, err
			}
		}
	}
	return machines, nil
}

func loadGeneration(link string, id *big.Int, d Dependencies) (*Generation, error) {
	g := &Generation{ID: id, Link: link, MACs: []any{}}
	var err error
	g.Store, err = d.FS.Realpath(link)
	if err != nil {
		return nil, err
	}
	info, err := d.FS.Lstat(link)
	if err != nil {
		return nil, err
	}
	g.Time = info.ModTime()
	g.TimeS = d.LocalTime(g.Time)
	b, err := d.FS.Read(join(link, "machine.json"))
	if err != nil {
		return nil, err
	}
	v, err := parseJSON(b)
	if err != nil {
		return nil, err
	}
	var ok bool
	g.metadata, ok = v.(object)
	if !ok {
		return nil, fmt.Errorf("machine.json must be an object")
	}
	g.Toplevel, err = requiredString(g.metadata, "toplevel")
	if err != nil {
		return nil, err
	}
	g.Version, _ = g.metadata.get("version")
	g.Revision, _ = g.metadata.get("revision")
	g.Inputs, _ = g.metadata.get("inputs-info")
	if truthy(g.Revision) {
		s, ok := g.Revision.(string)
		if !ok {
			return nil, fmt.Errorf("revision must be a string")
		}
		r := []rune(s)
		if len(r) > 9 {
			r = r[:9]
		}
		g.ShortRev = string(r)
	} else if truthy(g.Version) {
		s, ok := g.Version.(string)
		if !ok {
			return nil, fmt.Errorf("version must be a string")
		}
		s = strings.TrimRight(s, ".")
		if s != "" {
			bits := strings.Split(s, ".")
			g.ShortRev = bits[len(bits)-1]
		}
	}
	if v, ok := g.metadata.get("macs"); ok {
		g.MACs = v
	}
	kernel, err := d.FS.Readlink(join(g.Toplevel, "kernel"))
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	if err == nil {
		if m := kernelPattern.FindStringSubmatch(kernel); m != nil {
			g.KernelVersion = m[1]
		}
	}
	paramsFile := join(g.Store, "kernel-params")
	if _, err := d.FS.Stat(paramsFile); err == nil {
		b, err := d.FS.Read(paramsFile)
		if err != nil {
			return nil, err
		}
		s := strip(string(b))
		g.Params = strings.FieldsFunc(s, func(r rune) bool { return r == ' ' || r == '\t' || r == '\n' || r == '\r' || r == '\v' || r == '\f' })
	} else {
		v, ok := g.metadata.get("kernelParams")
		if !ok {
			v = []any{}
		}
		g.Params, err = stringArray(v, "kernelParams")
		if err != nil {
			return nil, err
		}
	}
	return g, nil
}

func resolveGeneration(m *Machine, g *Generation, d Dependencies) error {
	g.URL = join(m.URL, g.ID.String())
	for _, name := range []string{"bzImage", "initrd", "root.squashfs"} {
		path, err := d.FS.Realpath(join(g.Link, name))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return err
		}
		g.Boot = append(g.Boot, BootFile{name, path, join(g.URL, name)})
	}
	if m.Spin == "vpsadminos" {
		g.Variants = []Variant{{"default", "Default runlevel", []string{"runlevel=default"}}, {"nopools", "Default runlevel without container imports", []string{"runlevel=default", "osctl.pools=0"}}, {"nostart", "Default runlevel without container autostart", []string{"runlevel=default", "osctl.autostart=0"}}, {"rescue", "Rescue runlevel (network and sshd)", []string{"runlevel=rescue"}}, {"single", "Single-user runlevel (console only)", []string{"runlevel=single"}}}
	}
	params := []string{}
	hasInit := false
	for _, p := range g.Params {
		if strings.HasPrefix(p, "httproot=") {
			continue
		}
		params = append(params, p)
		hasInit = hasInit || strings.HasPrefix(p, "init=")
	}
	if m.Spin == "nixos" && !hasInit {
		params = append([]string{"init=" + join(g.Toplevel, "init")}, params...)
	}
	preferred := []string{"initrd", "bzImage"}
	if m.Spin == "vpsadminos" {
		preferred = []string{"root.squashfs"}
	}
	url := ""
	for _, name := range preferred {
		for _, b := range g.Boot {
			if b.Name == name {
				url = b.URL
				break
			}
		}
		if url != "" {
			break
		}
	}
	if url != "" {
		params = append([]string{"httproot=" + url}, params...)
	}
	g.Params = params
	return nil
}
func pxe(g *Generation) []string {
	if len(g.Params) > 0 {
		return g.Params
	}
	return []string{"init=" + join(g.Toplevel, "init"), "loglevel=7"}
}
