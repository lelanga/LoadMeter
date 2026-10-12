package triage

import "strings"

// Drain-style online log template miner.
//
// This follows He et al., "Drain: An Online Log Parsing Approach with Fixed
// Depth Tree" (ICWS 2017). Messages arrive one at a time (streaming) and are
// routed through a tree of fixed depth:
//
//  1. layer 1: the number of tokens in the message,
//  2. layers 2..PrefixTokens+1: the first PrefixTokens tokens (a token that is
//     a placeholder such as <NUM>, or that cannot be matched because a node
//     already has MaxChildren children, is routed through the wildcard child),
//  3. leaf: a short list of template groups. The group with the highest
//     similarity (fraction of positions whose template token equals the message
//     token, wildcard positions not counted) wins if it reaches Sim; the group's
//     template then generalises every differing position to the wildcard <*>.
//     Otherwise a new group is created.
//
// Bounded memory: at most MaxChildren children per node and MaxGroups groups
// overall. Once MaxGroups is reached, messages that match no existing group are
// counted in a single overflow group ("<other>") instead of creating more.
// The parser is deterministic: for the same message sequence it always
// produces the same groups (ties go to the earliest group).

const wildcard = "<*>"

// OtherTemplate is the template of the overflow group.
const OtherTemplate = "<other: template limit reached>"

type dnode struct {
	kids   map[string]*dnode
	groups []*group
}

type group struct {
	id     int
	tokens []string
	count  uint64
}

// Template returns the group's template text.
func (g *group) Template() string { return strings.Join(g.tokens, " ") }

// Drain is an online template miner. The zero value is not usable; call NewDrain.
type Drain struct {
	PrefixTokens int
	MaxChildren  int
	MaxGroups    int
	Sim          float64

	roots  map[int]*dnode
	groups []*group
	other  *group
}

// NewDrain returns a parser with the defaults used by Analyze
// (2 prefix tokens, 64 children per node, 256 groups, similarity 0.7).
func NewDrain(maxGroups int, sim float64) *Drain {
	if maxGroups <= 0 {
		maxGroups = 256
	}
	if sim <= 0 || sim > 1 {
		sim = 0.7
	}
	d := &Drain{PrefixTokens: 2, MaxChildren: 64, MaxGroups: maxGroups, Sim: sim, roots: map[int]*dnode{}}
	d.other = &group{id: -1, tokens: []string{OtherTemplate}}
	return d
}

// Groups returns the mined groups in creation order (excluding the overflow group).
func (d *Drain) Groups() []*group { return d.groups }

// Other returns the overflow group.
func (d *Drain) Other() *group { return d.other }

func isVariable(tok string) bool {
	return tok == wildcard || (len(tok) > 2 && tok[0] == '<' && tok[len(tok)-1] == '>')
}

// Add routes a normalised message to its group, creating or generalising a
// template as needed, and increments the group's count.
func (d *Drain) Add(tokens []string) *group {
	if len(tokens) == 0 {
		tokens = []string{"(no message)"}
	}
	root := d.roots[len(tokens)]
	if root == nil {
		root = &dnode{kids: map[string]*dnode{}}
		d.roots[len(tokens)] = root
	}
	node := root
	for i := 0; i < d.PrefixTokens && i < len(tokens); i++ {
		key := tokens[i]
		if isVariable(key) {
			key = wildcard
		}
		next := node.kids[key]
		if next == nil {
			if key != wildcard && len(node.kids) >= d.MaxChildren {
				key = wildcard
				next = node.kids[key]
			}
			if next == nil {
				next = &dnode{kids: map[string]*dnode{}}
				node.kids[key] = next
			}
		}
		node = next
	}
	var best *group
	bestSim, bestPar := -1.0, -1
	for _, g := range node.groups {
		sim, par := similarity(g.tokens, tokens)
		if sim > bestSim || (sim == bestSim && par > bestPar) {
			best, bestSim, bestPar = g, sim, par
		}
	}
	if best != nil && bestSim >= d.Sim {
		for i, t := range best.tokens {
			if t != tokens[i] {
				best.tokens[i] = wildcard
			}
		}
		best.count++
		return best
	}
	if len(d.groups) >= d.MaxGroups {
		d.other.count++
		return d.other
	}
	g := &group{id: len(d.groups), tokens: append([]string(nil), tokens...), count: 1}
	d.groups = append(d.groups, g)
	node.groups = append(node.groups, g)
	return g
}

func similarity(tmpl, msg []string) (float64, int) {
	same, par := 0, 0
	for i, t := range tmpl {
		if t == wildcard {
			par++
			continue
		}
		if t == msg[i] {
			same++
		}
	}
	return float64(same) / float64(len(tmpl)), par
}
