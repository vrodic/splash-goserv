package jinja

import "strings"

// PatchLaterSystem returns the template with the one construct that
// rejects (mode "rejects": the raise_exception in a message loop's system
// branch) or drops (mode "drops": the loop's condition that skips system
// messages) a system message after the first replaced by a render of it in
// the template's own system block, prefix + content|trim + suffix, as
// chat_templates._patch does in the source. ok is false where there is not
// exactly one such construct.
func PatchLaterSystem(src, mode, prefix, suffix string) (*Template, bool) {
	t, err := Parse(src)
	if err != nil {
		return nil, false
	}
	type site struct {
		body  *[]node
		index int
		v     string
	}
	var sites []site
	seen := map[node]bool{}
	var walkLoops func(body *[]node)
	walkLoops = func(body *[]node) {
		for i := range *body {
			n := (*body)[i]
			if f, ok := n.(*forNode); ok {
				if name, isName := f.iter.(*nameExpr); isName && name.name == "messages" && len(f.targets) == 1 && f.filter == nil {
					v := f.targets[0]
					if mode == "rejects" {
						collectRejections(&f.body, v, func(b *[]node, j int) {
							if !seen[(*b)[j]] {
								seen[(*b)[j]] = true
								sites = append(sites, site{b, j, v})
							}
						})
					} else {
						for j, child := range f.body {
							if in, ok := child.(*ifNode); ok && len(in.conds) == 1 && in.orelse == nil &&
								anyTerm(in.conds[0], "and", func(e expr) bool { return comparesRole(e, v, "!=") }) {
								sites = append(sites, site{&f.body, j, v})
							}
						}
					}
				}
			}
			for _, child := range childBodies(n) {
				walkLoops(child)
			}
		}
	}
	walkLoops(&t.body)
	if len(sites) != 1 {
		return nil, false
	}
	s := sites[0]
	later := &outputNode{expr: &binExpr{op: "+", left: &binExpr{op: "+", left: &literal{prefix},
		right: &filterExpr{value: &attrExpr{obj: &nameExpr{name: s.v}, attr: "content"}, name: "trim"}},
		right: &literal{suffix}}}
	body := *s.body
	if mode == "rejects" {
		body[s.index] = later
		// "-}}" strips the text after it.
		if s.index+1 < len(body) {
			if tn, ok := body[s.index+1].(*textNode); ok {
				tn.text = strings.TrimLeft(tn.text, " \t\n\r\f\v")
			}
		}
	} else {
		orig := body[s.index].(*ifNode)
		cond := &binExpr{op: "and",
			left:  &unaryExpr{op: "not", x: &attrExpr{obj: &nameExpr{name: "loop"}, attr: "first"}},
			right: &compareExpr{first: &attrExpr{obj: &nameExpr{name: s.v}, attr: "role"}, ops: []string{"=="}, rest: []expr{&literal{"system"}}}}
		body[s.index] = &ifNode{conds: []expr{cond, orig.conds[0]}, bodies: [][]node{{later}, orig.bodies[0]}}
	}
	return t, true
}

func childBodies(n node) []*[]node {
	switch n := n.(type) {
	case *ifNode:
		out := make([]*[]node, 0, len(n.bodies)+1)
		for i := range n.bodies {
			out = append(out, &n.bodies[i])
		}
		if n.orelse != nil {
			out = append(out, &n.orelse)
		}
		return out
	case *forNode:
		return []*[]node{&n.body, &n.orelse}
	case *macroNode:
		return []*[]node{&n.body}
	case *setNode:
		if n.body != nil {
			return []*[]node{&n.body}
		}
	case *scopeNode:
		return []*[]node{&n.body}
	case *callBlockNode:
		return []*[]node{&n.body}
	case *filterBlockNode:
		return []*[]node{&n.body}
	}
	return nil
}

// collectRejections finds, in the bodies under a loop, each branch of an if
// (an elif is a branch of its own) whose test is an or-chain naming
// v.role == 'system', and in it each output of a raise_exception call.
func collectRejections(body *[]node, v string, found func(*[]node, int)) {
	for _, n := range *body {
		if in, ok := n.(*ifNode); ok {
			for i, c := range in.conds {
				if anyTerm(c, "or", func(e expr) bool { return comparesRole(e, v, "==") }) {
					collectRaises(&in.bodies[i], found)
				}
			}
		}
		for _, child := range childBodies(n) {
			collectRejections(child, v, found)
		}
	}
}

func collectRaises(body *[]node, found func(*[]node, int)) {
	for i, n := range *body {
		if out, ok := n.(*outputNode); ok {
			if call, ok := out.expr.(*callExpr); ok {
				if name, ok := call.fn.(*nameExpr); ok && name.name == "raise_exception" {
					found(body, i)
				}
			}
		}
		for _, child := range childBodies(n) {
			collectRaises(child, found)
		}
	}
}

func anyTerm(e expr, op string, pred func(expr) bool) bool {
	if b, ok := e.(*binExpr); ok && b.op == op {
		return anyTerm(b.left, op, pred) || anyTerm(b.right, op, pred)
	}
	return pred(e)
}

func comparesRole(e expr, v, op string) bool {
	c, ok := e.(*compareExpr)
	if !ok || len(c.ops) != 1 || c.ops[0] != op {
		return false
	}
	if lit, ok := c.rest[0].(*literal); !ok || lit.v != "system" {
		return false
	}
	var target expr
	switch s := c.first.(type) {
	case *attrExpr:
		if s.attr != "role" {
			return false
		}
		target = s.obj
	case *itemExpr:
		lit, ok := s.key.(*literal)
		if !ok || lit.v != "role" {
			return false
		}
		target = s.obj
	default:
		return false
	}
	name, ok := target.(*nameExpr)
	return ok && name.name == v
}
