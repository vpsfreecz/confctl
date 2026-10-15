package build

import (
	"math/big"
	"regexp"
	"strings"
	"sync"
)

// Event owns snapshots, so a later line cannot alter an earlier observation.
type Event struct {
	Type            string
	Progress, Total *big.Int
	Path            string
}

// Progress consumes the separate streams in source arrival order. Raw logging
// and collection belong to the caller; stdout lines never drive progress.
type Progress struct {
	mu                                                   sync.Mutex
	out, err                                             string
	buildProgress, buildTotal, fetchProgress, fetchTotal big.Int
	derivations, paths                                   bool
	emit                                                 func(Event)
}

func NewProgress(emit func(Event)) *Progress { return &Progress{emit: emit} }

func readLine(buffer *string) (string, bool) {
	i := strings.IndexByte(*buffer, '\n')
	if i < 0 {
		return "", false
	}
	line := (*buffer)[:i+1]
	*buffer = (*buffer)[i+1:]
	return line, true
}

func (p *Progress) Feed(stdout, stderr []byte) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.out += string(stdout)
	p.err += string(stderr)
	for {
		_, out := readLine(&p.out)
		line, err := readLine(&p.err)
		if !out && !err {
			return
		}
		if err && p.emit != nil {
			p.parse(line)
		}
	}
}

// Finish is called only after a successful child return, before JSON decoding.
// An error skips the explicit source flush, including its progress remainder.
func (p *Progress) Finish() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.emit != nil {
		p.parse(p.err)
	}
	p.out, p.err = "", ""
}

var (
	modernBuild = regexp.MustCompile(`^these (\d+) derivations will be built:`)
	modernFetch = regexp.MustCompile(`^these (\d+) paths will be fetched `)
	storeLine   = regexp.MustCompile(`^[\t\n\f\r \x0b]+/nix/store/`)
	building    = regexp.MustCompile(`^building '([^']+)`)
	copying     = regexp.MustCompile(`^copying path '([^']+)`)
)

func (p *Progress) parse(line string) {
	switch {
	case strings.HasPrefix(line, "this derivation will be built:"):
		p.buildTotal.SetInt64(1)
		return
	case modernBuild.MatchString(line):
		p.buildTotal.SetString(modernBuild.FindStringSubmatch(line)[1], 10)
		return
	case strings.HasPrefix(line, "this path will be fetched "):
		p.fetchTotal.SetInt64(1)
		return
	case modernFetch.MatchString(line):
		p.fetchTotal.SetString(modernFetch.FindStringSubmatch(line)[1], 10)
		return
	case strings.HasPrefix(line, "these derivations will be built:"):
		p.derivations, p.paths = true, false
		return
	case strings.HasPrefix(line, "these paths will be fetched "):
		p.derivations, p.paths = false, true
		return
	}
	if p.derivations {
		if storeLine.MatchString(line) {
			p.buildTotal.Add(&p.buildTotal, big.NewInt(1))
			return
		}
		p.derivations = false
		p.buildTotal.Add(&p.buildTotal, &p.buildProgress)
	} else if p.paths {
		if storeLine.MatchString(line) {
			p.fetchTotal.Add(&p.fetchTotal, big.NewInt(1))
			return
		}
		p.paths = false
		p.fetchTotal.Add(&p.fetchTotal, &p.fetchProgress)
	}
	if m := building.FindStringSubmatch(line); m != nil {
		p.buildProgress.Add(&p.buildProgress, big.NewInt(1))
		p.emit(Event{"build", new(big.Int).Set(&p.buildProgress), new(big.Int).Set(&p.buildTotal), m[1]})
	} else if m := copying.FindStringSubmatch(line); m != nil {
		p.fetchProgress.Add(&p.fetchProgress, big.NewInt(1))
		p.emit(Event{"fetch", new(big.Int).Set(&p.fetchProgress), new(big.Int).Set(&p.fetchTotal), m[1]})
	}
}
