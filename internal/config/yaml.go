package config

import (
	"fmt"
	"strings"
)

// This file implements the small YAML subset the config file uses: nested
// mappings, block and inline lists, quoted strings, integers and booleans.

type yKind int

const (
	yUnknown yKind = iota
	yMap
	yList
	yScalar
)

type yNode struct {
	kind yKind
	m    map[string]*yNode
	l    []*yNode
	s    string
}

func (n *yNode) get(keys ...string) *yNode {
	cur := n
	for _, k := range keys {
		if cur == nil || cur.kind != yMap {
			return nil
		}
		cur = cur.m[k]
	}
	return cur
}

type yFrame struct {
	node   *yNode
	indent int
}

func parseYAML(data []byte) (*yNode, error) {
	root := &yNode{kind: yMap, m: map[string]*yNode{}}
	stack := []*yFrame{{node: root, indent: -1}}

	for ln, raw := range strings.Split(string(data), "\n") {
		line := stripComment(raw)
		if strings.TrimSpace(line) == "" {
			continue
		}
		if strings.Contains(strings.TrimLeft(line, " "), "\t") {
			return nil, fmt.Errorf("line %d: tabs are not allowed for indentation", ln+1)
		}
		indent := countIndent(line)
		content := strings.TrimSpace(line)

		for len(stack) > 1 && stack[len(stack)-1].indent >= indent {
			stack = stack[:len(stack)-1]
		}
		top := stack[len(stack)-1]

		if strings.HasPrefix(content, "-") && (content == "-" || strings.HasPrefix(content, "- ")) {
			itemText := strings.TrimSpace(strings.TrimPrefix(content, "-"))
			if top.node.kind == yUnknown {
				top.node.kind = yList
			}
			if top.node.kind != yList {
				return nil, fmt.Errorf("line %d: list item under a non-list", ln+1)
			}
			if itemText == "" {
				return nil, fmt.Errorf("line %d: empty list item", ln+1)
			}
			item, err := parseItem(itemText)
			if err != nil {
				return nil, fmt.Errorf("line %d: %w", ln+1, err)
			}
			top.node.l = append(top.node.l, item)
			if item.kind == yMap {
				stack = append(stack, &yFrame{node: item, indent: indent})
			}
			continue
		}

		key, val, ok := splitKeyValue(content)
		if !ok {
			return nil, fmt.Errorf("line %d: expected key: value", ln+1)
		}
		if top.node.kind == yUnknown {
			top.node.kind = yMap
			top.node.m = map[string]*yNode{}
		}
		if top.node.kind != yMap {
			return nil, fmt.Errorf("line %d: mapping key under a non-map", ln+1)
		}
		if val == "" {
			child := &yNode{kind: yUnknown}
			top.node.m[key] = child
			stack = append(stack, &yFrame{node: child, indent: indent})
			continue
		}
		scalar, err := parseScalar(val)
		if err != nil {
			return nil, fmt.Errorf("line %d: %w", ln+1, err)
		}
		top.node.m[key] = scalar
	}
	return root, nil
}

func parseItem(text string) (*yNode, error) {
	if key, val, ok := splitKeyValue(text); ok {
		if val == "" {
			return &yNode{kind: yMap, m: map[string]*yNode{key: &yNode{kind: yUnknown}}}, nil
		}
		scalar, err := parseScalar(val)
		if err != nil {
			return nil, err
		}
		return &yNode{kind: yMap, m: map[string]*yNode{key: scalar}}, nil
	}
	return parseScalar(text)
}

func parseScalar(s string) (*yNode, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return &yNode{kind: yScalar, s: ""}, nil
	}
	if strings.HasPrefix(s, "{") && strings.HasSuffix(s, "}") {
		return parseInlineMap(s[1 : len(s)-1])
	}
	if strings.HasPrefix(s, "[") && strings.HasSuffix(s, "]") {
		return parseInlineList(s[1 : len(s)-1])
	}
	if len(s) >= 2 && ((s[0] == '"' && s[len(s)-1] == '"') || (s[0] == '\'' && s[len(s)-1] == '\'')) {
		return &yNode{kind: yScalar, s: s[1 : len(s)-1]}, nil
	}
	return &yNode{kind: yScalar, s: s}, nil
}

func parseInlineMap(s string) (*yNode, error) {
	node := &yNode{kind: yMap, m: map[string]*yNode{}}
	for _, part := range splitTopLevel(s, ',') {
		key, val, ok := splitKeyValue(strings.TrimSpace(part))
		if !ok {
			return nil, fmt.Errorf("invalid inline map entry %q", part)
		}
		child, err := parseScalar(val)
		if err != nil {
			return nil, err
		}
		node.m[key] = child
	}
	return node, nil
}

func parseInlineList(s string) (*yNode, error) {
	node := &yNode{kind: yList}
	for _, part := range splitTopLevel(s, ',') {
		item, err := parseScalar(strings.TrimSpace(part))
		if err != nil {
			return nil, err
		}
		node.l = append(node.l, item)
	}
	return node, nil
}

func splitKeyValue(s string) (key, val string, ok bool) {
	depth := 0
	var quote byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		if quote != 0 {
			if c == quote {
				quote = 0
			}
			continue
		}
		switch c {
		case '"', '\'':
			quote = c
		case '{', '[':
			depth++
		case '}', ']':
			depth--
		case ':':
			if depth == 0 {
				return strings.TrimSpace(s[:i]), strings.TrimSpace(s[i+1:]), true
			}
		}
	}
	return "", "", false
}

func splitTopLevel(s string, sep byte) []string {
	var parts []string
	depth := 0
	var quote byte
	start := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		if quote != 0 {
			if c == quote {
				quote = 0
			}
			continue
		}
		switch c {
		case '"', '\'':
			quote = c
		case '{', '[':
			depth++
		case '}', ']':
			depth--
		case sep:
			if depth == 0 {
				parts = append(parts, s[start:i])
				start = i + 1
			}
		}
	}
	parts = append(parts, s[start:])
	return parts
}

func stripComment(line string) string {
	var quote byte
	for i := 0; i < len(line); i++ {
		c := line[i]
		if quote != 0 {
			if c == quote {
				quote = 0
			}
			continue
		}
		if c == '"' || c == '\'' {
			quote = c
			continue
		}
		if c == '#' && (i == 0 || line[i-1] == ' ' || line[i-1] == '\t') {
			return line[:i]
		}
	}
	return line
}

func countIndent(line string) int {
	return len(line) - len(strings.TrimLeft(line, " "))
}
