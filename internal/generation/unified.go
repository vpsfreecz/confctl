package generation

import "math/big"

// Unified keeps the first attachment's toplevel/kernel. Replacements and sticky
// Current deliberately follow the original setters rather than recomputing it.
type Unified struct {
	Host, Name      string
	ID              *big.Int
	Toplevel        *string
	Date            Timestamp
	KernelVersion   *string
	Current         bool
	BuildGeneration *Build
	HostGeneration  *Host
}

func (g *Unified) attachBuild(build *Build) {
	g.Name, g.Date, g.BuildGeneration = build.Name, build.Date, build
	g.Current = g.Current || build.Current
}
func (g *Unified) attachHost(host *Host) {
	g.ID, g.HostGeneration = new(big.Int).Set(host.ID), host
	g.Current = g.Current || host.Current
}

func (g *Unified) Inputs() Object {
	if g.BuildGeneration == nil {
		return nil
	}
	return g.BuildGeneration.Inputs
}
func (g *Unified) InputsInfo() Object {
	if g.BuildGeneration == nil {
		return nil
	}
	return g.BuildGeneration.InputsInfo
}
func (g *Unified) CurrentString() string {
	build := g.BuildGeneration != nil && g.BuildGeneration.Current
	host := g.HostGeneration != nil && g.HostGeneration.Current
	return attachmentLabel(build, host)
}
func (g *Unified) PresenceString() string {
	return attachmentLabel(g.BuildGeneration != nil, g.HostGeneration != nil)
}
func attachmentLabel(build, host bool) string {
	if build && host {
		return "build+host"
	}
	if build {
		return "build"
	}
	if host {
		return "host"
	}
	return ""
}

// Input paths have already been validated by R1. Ruby Hash equality ignores
// member order, unlike Object's representation used for output metadata.
func sameInputs(a, b Object) bool {
	if len(a) != len(b) {
		return false
	}
	for _, member := range a {
		value, present := b.Lookup(member.Name)
		if !present || member.Value != value {
			return false
		}
	}
	return true
}

// Generations stay in insertion order. Core separately sorts displayed rows.
type UnifiedList struct{ Generations []*Unified }

func (l *UnifiedList) AddHost(host *Host) *Unified {
	for _, g := range l.Generations {
		if g.Host == host.Host && samePath(g.Toplevel, host.Toplevel) {
			g.attachHost(host)
			return g
		}
	}
	g := &Unified{Host: host.Host, Name: host.ApproxName(), Toplevel: host.Toplevel, Date: host.Date, KernelVersion: host.KernelVersion}
	g.attachHost(host)
	l.Generations = append(l.Generations, g)
	return g
}
func (l *UnifiedList) AddBuild(build *Build) *Unified {
	for _, g := range l.Generations {
		if g.Host != build.Host || g.Toplevel == nil || *g.Toplevel != build.Toplevel {
			continue
		}
		if existing := g.BuildGeneration; existing != nil && (existing.Name != build.Name || !sameInputs(existing.Inputs, build.Inputs)) {
			continue
		}
		g.attachBuild(build)
		return g
	}
	top := build.Toplevel
	g := &Unified{Host: build.Host, Toplevel: &top, KernelVersion: build.KernelVersion}
	g.attachBuild(build)
	l.Generations = append(l.Generations, g)
	return g
}
func (l *UnifiedList) AddHosts(hosts *HostList) {
	for _, g := range hosts.Generations {
		l.AddHost(g)
	}
}
func (l *UnifiedList) AddBuilds(builds *BuildList) {
	for _, g := range builds.Builds {
		l.AddBuild(g)
	}
}

// Zero is last, positive n is n-1, and -1 is penultimate. Remote IDs do not
// participate. Bound arbitrary integers before narrowing to a slice index.
func (l *UnifiedList) AtOffset(offset *big.Int) *Unified {
	n := big.NewInt(int64(len(l.Generations)))
	index := new(big.Int).Sub(offset, big.NewInt(1))
	if offset.Sign() == 0 {
		index.Sub(n, big.NewInt(1))
	} else if index.Sign() < 0 {
		index.Add(index, n)
	}
	if index.Sign() < 0 || index.Cmp(n) >= 0 {
		return nil
	}
	return l.Generations[int(index.Int64())]
}
