// Package jmx imports Apache JMeter (.jmx, 5.x) test plans into the Load Meter
// scenario IR and exports Load Meter scenarios back to .jmx. Every source element
// is accounted for in a model.CompatReport; nothing is dropped silently.
package jmx

import (
	"bufio"
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"
)

// MaxInputBytes is the largest .jmx document Import accepts.
const MaxInputBytes = 50 << 20

// inputLimit is MaxInputBytes; a variable so tests can lower it.
var inputLimit int64 = MaxInputBytes

const (
	maxDepth = 256       // JMeter plans are rarely deeper than ~30
	maxNodes = 2_000_000 // element count cap (defence against pathological input)
)

// node is a generic, namespace-free XML element.
type node struct {
	Name     string
	Attrs    map[string]string
	Children []*node
	text     []byte
}

func (n *node) Text() string { return string(n.text) }

func (n *node) Attr(k string) string {
	if n == nil {
		return ""
	}
	return n.Attrs[k]
}

var simplePropTags = map[string]bool{"stringProp": true, "boolProp": true, "intProp": true,
	"longProp": true, "doubleProp": true, "floatProp": true}

// prop returns the text of a direct simple property child (stringProp, boolProp, ...).
func (n *node) prop(name string) (string, bool) {
	if n == nil {
		return "", false
	}
	for _, c := range n.Children {
		if simplePropTags[c.Name] && c.Attrs["name"] == name {
			return c.Text(), true
		}
	}
	return "", false
}

func (n *node) str(name string) string {
	v, _ := n.prop(name)
	return v
}

func (n *node) boolean(name string, def bool) bool {
	v, ok := n.prop(name)
	v = strings.TrimSpace(v)
	if !ok || v == "" {
		return def
	}
	return strings.EqualFold(v, "true")
}

func (n *node) elemProp(name string) *node {
	if n == nil {
		return nil
	}
	for _, c := range n.Children {
		if c.Name == "elementProp" && c.Attrs["name"] == name {
			return c
		}
	}
	return nil
}

// coll returns the children of a direct collectionProp child; ok reports whether it exists.
func (n *node) coll(name string) ([]*node, bool) {
	if n == nil {
		return nil, false
	}
	for _, c := range n.Children {
		if c.Name == "collectionProp" && c.Attrs["name"] == name {
			return c.Children, true
		}
	}
	return nil, false
}

func (n *node) child(tag string) *node {
	if n == nil {
		return nil
	}
	for _, c := range n.Children {
		if c.Name == tag {
			return c
		}
	}
	return nil
}

// parseTree reads untrusted XML into a node tree. It caps input size, nesting
// depth and element count, rejects any DTD (<!DOCTYPE>, <!ENTITY>) and never
// resolves external resources (encoding/xml does not fetch anything; unknown
// entities are an error in strict mode).
func parseTree(r io.Reader) (*node, error) {
	if r == nil {
		return nil, errors.New("jmx: nil reader")
	}
	data, err := io.ReadAll(io.LimitReader(r, inputLimit+1))
	if err != nil {
		return nil, fmt.Errorf("jmx: read: %w", err)
	}
	if int64(len(data)) > inputLimit {
		return nil, fmt.Errorf("jmx: input exceeds %d bytes", inputLimit)
	}
	dec := xml.NewDecoder(bytes.NewReader(data))
	dec.Strict = true
	dec.CharsetReader = charsetReader
	var root *node
	var stack []*node
	count := 0
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("jmx: malformed XML: %w", err)
		}
		switch t := tok.(type) {
		case xml.StartElement:
			count++
			if count > maxNodes {
				return nil, fmt.Errorf("jmx: more than %d elements", maxNodes)
			}
			if len(stack) >= maxDepth {
				return nil, fmt.Errorf("jmx: nesting deeper than %d levels", maxDepth)
			}
			n := &node{Name: t.Name.Local, Attrs: make(map[string]string, len(t.Attr))}
			for _, a := range t.Attr {
				n.Attrs[a.Name.Local] = a.Value
			}
			if len(stack) == 0 {
				if root != nil {
					return nil, errors.New("jmx: malformed XML: multiple root elements")
				}
				root = n
			} else {
				p := stack[len(stack)-1]
				p.Children = append(p.Children, n)
			}
			stack = append(stack, n)
		case xml.EndElement:
			if len(stack) == 0 {
				return nil, errors.New("jmx: malformed XML: unbalanced end element")
			}
			stack = stack[:len(stack)-1]
		case xml.CharData:
			if len(stack) > 0 {
				top := stack[len(stack)-1]
				top.text = append(top.text, t...)
			} else if len(bytes.TrimSpace(t)) > 0 {
				return nil, errors.New("jmx: malformed XML: text outside the root element")
			}
		case xml.Directive:
			return nil, errors.New("jmx: DOCTYPE/DTD declarations are not allowed in .jmx input")
		}
	}
	if root == nil {
		return nil, errors.New("jmx: empty document")
	}
	if len(stack) != 0 {
		return nil, errors.New("jmx: malformed XML: unclosed elements")
	}
	return root, nil
}

// charsetReader accepts the single-byte encodings JMeter files are realistically
// saved in besides UTF-8. Anything else is rejected rather than guessed.
func charsetReader(label string, in io.Reader) (io.Reader, error) {
	switch strings.ToLower(strings.TrimSpace(label)) {
	case "iso-8859-1", "iso8859-1", "latin1", "latin-1", "us-ascii", "ascii":
		return &latin1Reader{r: bufio.NewReader(in)}, nil
	}
	return nil, fmt.Errorf("unsupported XML encoding %q (save the plan as UTF-8)", label)
}

type latin1Reader struct {
	r       io.ByteReader
	pending []byte
}

func (l *latin1Reader) Read(p []byte) (int, error) {
	n := 0
	for n < len(p) {
		if len(l.pending) > 0 {
			c := copy(p[n:], l.pending)
			l.pending = l.pending[c:]
			n += c
			continue
		}
		b, err := l.r.ReadByte()
		if err != nil {
			if n > 0 {
				return n, nil
			}
			return 0, err
		}
		if b < utf8.RuneSelf {
			p[n] = b
			n++
			continue
		}
		var buf [utf8.UTFMax]byte
		w := utf8.EncodeRune(buf[:], rune(b))
		l.pending = append(l.pending[:0], buf[:w]...)
	}
	return n, nil
}
