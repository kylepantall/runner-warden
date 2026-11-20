package migrate

import (
	"fmt"
	"strings"
)

// unifiedDiff renders a unified diff (3 lines of context) of a to b. The
// inputs are small workflow files, so a plain LCS table is enough.
func unifiedDiff(name, side, a, b string) string {
	_ = side
	if a == b {
		return ""
	}
	al, bl := splitLines(a), splitLines(b)
	n, m := len(al), len(bl)
	lcs := make([][]int, n+1)
	for i := range lcs {
		lcs[i] = make([]int, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if al[i] == bl[j] {
				lcs[i][j] = lcs[i+1][j+1] + 1
			} else {
				lcs[i][j] = max(lcs[i+1][j], lcs[i][j+1])
			}
		}
	}
	type op struct {
		kind byte // ' ', '-', '+'
		text string
		a, b int // 0-based positions in a and b
	}
	var ops []op
	i, j := 0, 0
	for i < n || j < m {
		switch {
		case i < n && j < m && al[i] == bl[j]:
			ops = append(ops, op{' ', al[i], i, j})
			i++
			j++
		case j < m && (i == n || lcs[i][j+1] >= lcs[i+1][j]):
			ops = append(ops, op{'+', bl[j], i, j})
			j++
		default:
			ops = append(ops, op{'-', al[i], i, j})
			i++
		}
	}
	const ctx = 3
	var sb strings.Builder
	fmt.Fprintf(&sb, "--- a/%s\n+++ b/%s\n", name, name)
	for k := 0; k < len(ops); {
		if ops[k].kind == ' ' {
			k++
			continue
		}
		start := max(k-ctx, 0)
		end := k
		for end < len(ops) {
			if ops[end].kind != ' ' {
				end++
				continue
			}
			// extend while another change is within 2*ctx context lines
			next := end
			for next < len(ops) && ops[next].kind == ' ' {
				next++
			}
			if next < len(ops) && next-end <= 2*ctx {
				end = next
				continue
			}
			break
		}
		stop := min(end+ctx, len(ops))
		var as, bs, ac, bc int
		as, bs = ops[start].a+1, ops[start].b+1
		for _, o := range ops[start:stop] {
			if o.kind != '+' {
				ac++
			}
			if o.kind != '-' {
				bc++
			}
		}
		fmt.Fprintf(&sb, "@@ -%d,%d +%d,%d @@\n", as, ac, bs, bc)
		for _, o := range ops[start:stop] {
			sb.WriteString(string(o.kind) + o.text + "\n")
		}
		k = stop
	}
	return sb.String()
}

func splitLines(s string) []string {
	s = strings.TrimSuffix(s, "\n")
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}
