// Package migrate moves a host from legacy self-hosted runners to
// runner-warden: scanning, rewriting `runs-on` labels, applying the rewrite
// (print, PR or push), and retiring legacy local runners.
package migrate

import (
	"bytes"
	"encoding/json"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"gopkg.in/yaml.v3"

	"github.com/kylepantall/runner-warden/internal/core"
	"github.com/kylepantall/runner-warden/internal/naming"
)

// Rules decide which runs-on values are ours and where they map.
type Rules struct {
	Prefix string `json:"prefix"`
	// LegacyLabels are labels (besides self-hosted) that mark a runs-on as
	// ours, for example "home-ci", "general", "isolink", "kscript".
	LegacyLabels []string `json:"legacyLabels,omitempty"`
	// WindowsLabels are legacy labels that imply Windows (any label containing
	// "windows" already does).
	WindowsLabels []string `json:"windowsLabels,omitempty"`
	// MacLabels are legacy labels that imply macOS (any label containing
	// "macos" already does). macOS is not served yet: such values are skipped.
	MacLabels []string `json:"macLabels,omitempty"`
	// BareHost is true when a runner on this host carries only the default
	// labels (self-hosted, Linux, X64, ...); only then is a runs-on made of
	// defaults alone ours. LegacyLabels must come from this host's runners
	// (plus explicit flags), never from other hosts at the same org.
	BareHost bool `json:"bareHost,omitempty"`
}

var defaultLabelSet = []string{"self-hosted", "linux", "windows", "macos", "x64"}

// IsDefaultLabel reports the implicit labels every runner of that OS carries.
// Architecture and capability labels (ARM64, gpu, ...) are not defaults.
func IsDefaultLabel(l string) bool { return containsFold(defaultLabelSet, l) }

// Reason says why a runs-on occurrence was not rewritten (or what was
// reported).
type Reason string

// Skip reasons.
const (
	ReasonMacOS            Reason = "macos_not_served"
	ReasonVariable         Reason = "variable_reference" // report only: a vars.* reference to review/update
	ReasonUnsupportedExpr  Reason = "unsupported_expression"
	ReasonUnsupportedForm  Reason = "unsupported_form"
	ReasonMatrixUnresolved Reason = "matrix_unresolved"
	ReasonGroupOnly        Reason = "group_without_labels"
	ReasonParse            Reason = "parse_error"
	ReasonForeignLabels    Reason = "foreign_labels" // runs-on needs labels no runner on this host has
	ReasonMatrixKeyUsed    Reason = "matrix_key_used_elsewhere"
	ReasonVarNotFound             = Reason("variable_not_found")  // referenced variable could not be read
	ReasonVarNotLabels            = Reason("variable_not_labels") // variable value holds no legacy labels
)

// Form is the syntactic form that was rewritten.
type Form string

// Rewrite forms.
const (
	FormPlain          Form = "plain"
	FormSequence       Form = "sequence"
	FormExpression     Form = "expression_literal"
	FormExpressionJSON Form = "expression_json"
	FormMatrix         Form = "matrix_value"
	FormMatrixJSON     Form = "matrix_json"
)

// Change is one replaced span.
type Change struct {
	Job    string  `json:"job"`
	Line   int     `json:"line"`
	Column int     `json:"column"`
	Form   Form    `json:"form"`
	OS     core.OS `json:"os"`
	Old    string  `json:"old"`
	New    string  `json:"new"`
}

// Skip is an occurrence left alone, or (ReasonVariable) a reference to report.
type Skip struct {
	Job    string `json:"job"`
	Line   int    `json:"line"`
	Column int    `json:"column"`
	Reason Reason `json:"reason"`
	Text   string `json:"text"`
	Var    string `json:"var,omitempty"` // ReasonVariable: the variable name
}

// Untouched is a runs-on that is not ours (hosted labels and the like).
type Untouched struct {
	Job  string `json:"job"`
	Line int    `json:"line"`
	Text string `json:"text"`
}

// Result is the full outcome of analysing one workflow file.
type Result struct {
	Out       []byte
	Changes   []Change
	Skipped   []Skip
	Untouched []Untouched
}

// Rewrite rewrites runs-on values in a workflow, preserving all other bytes.
func Rewrite(src []byte, rules Rules) ([]byte, []Change, []Skip) {
	r := Analyze(src, rules)
	return r.Out, r.Changes, r.Skipped
}

// Analyze is Rewrite plus the list of untouched runs-on values.
func Analyze(src []byte, rules Rules) Result {
	a := &analyzer{rules: rules, src: newSource(src), done: map[int]bool{}}
	var doc yaml.Node
	if err := yaml.Unmarshal(src, &doc); err != nil {
		return Result{Out: src, Skipped: []Skip{{Reason: ReasonParse, Text: err.Error()}}}
	}
	root := &doc
	if root.Kind == yaml.DocumentNode && len(root.Content) > 0 {
		root = root.Content[0]
	}
	if jobs := mapGet(root, "jobs"); jobs != nil && jobs.Kind == yaml.MappingNode {
		for i := 0; i+1 < len(jobs.Content); i += 2 {
			if j := jobs.Content[i+1]; j.Kind == yaml.MappingNode {
				a.job(jobs.Content[i].Value, j)
			}
		}
	}
	out := a.apply()
	return Result{Out: out, Changes: a.changes, Skipped: a.skips, Untouched: a.untouched}
}

// ---- source positions ----

type source struct {
	b          []byte
	lineStarts []int
}

func newSource(b []byte) *source {
	s := &source{b: b, lineStarts: []int{0}}
	for i, c := range b {
		if c == '\n' {
			s.lineStarts = append(s.lineStarts, i+1)
		}
	}
	return s
}

// offset converts a 1-based line/column (column counted in characters) to a
// byte offset.
func (s *source) offset(line, col int) int {
	if line < 1 || line > len(s.lineStarts) {
		return -1
	}
	i := s.lineStarts[line-1]
	for c := 1; c < col && i < len(s.b); c++ {
		_, n := utf8.DecodeRune(s.b[i:])
		i += n
	}
	return i
}

// scalarSpan returns the byte span of a scalar node including its quotes.
func (s *source) scalarSpan(n *yaml.Node) (int, int, bool) {
	if n.Kind != yaml.ScalarNode || n.Anchor != "" || n.Style&(yaml.LiteralStyle|yaml.FoldedStyle) != 0 {
		return 0, 0, false
	}
	st := s.offset(n.Line, n.Column)
	if st < 0 || st >= len(s.b) {
		return 0, 0, false
	}
	switch {
	case n.Style&yaml.SingleQuotedStyle != 0:
		if s.b[st] != '\'' {
			return 0, 0, false
		}
		for i := st + 1; i < len(s.b); i++ {
			if s.b[i] == '\'' {
				if i+1 < len(s.b) && s.b[i+1] == '\'' {
					i++
					continue
				}
				return st, i + 1, true
			}
		}
	case n.Style&yaml.DoubleQuotedStyle != 0:
		if s.b[st] != '"' {
			return 0, 0, false
		}
		for i := st + 1; i < len(s.b); i++ {
			switch s.b[i] {
			case '\\':
				i++
			case '"':
				return st, i + 1, true
			}
		}
	default:
		if n.Value != "" && !strings.Contains(n.Value, "\n") && bytes.HasPrefix(s.b[st:], []byte(n.Value)) {
			return st, st + len(n.Value), true
		}
	}
	return 0, 0, false
}

// ---- analyzer ----

type edit struct {
	start, end int
	text       string
}

type analyzer struct {
	rules     Rules
	src       *source
	edits     []edit
	changes   []Change
	skips     []Skip
	untouched []Untouched
	done      map[int]bool // span starts already edited

	jobName string
}

func (a *analyzer) apply() []byte {
	if len(a.edits) == 0 {
		return a.src.b
	}
	sort.Slice(a.edits, func(i, j int) bool { return a.edits[i].start > a.edits[j].start })
	out := append([]byte(nil), a.src.b...)
	for _, e := range a.edits {
		out = append(out[:e.start], append([]byte(e.text), out[e.end:]...)...)
	}
	sort.SliceStable(a.changes, func(i, j int) bool { return a.changes[i].Line < a.changes[j].Line })
	return out
}

func mapGet(m *yaml.Node, key string) *yaml.Node {
	if m == nil || m.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	return nil
}

func (a *analyzer) skip(n *yaml.Node, r Reason, text, v string) {
	a.skips = append(a.skips, Skip{Job: a.jobName, Line: n.Line, Column: n.Column, Reason: r, Text: text, Var: v})
}

func (a *analyzer) job(name string, j *yaml.Node) {
	ro := mapGet(j, "runs-on")
	if ro == nil {
		return
	}
	a.jobName = name
	a.runsOn(j, ro)
}

// ---- classification ----

func (a *analyzer) legacy(l string) bool {
	if strings.EqualFold(l, "self-hosted") {
		return true
	}
	if p := a.rules.Prefix; p != "" && (l == p || strings.HasPrefix(l, p+"-")) {
		return false
	}
	for _, x := range a.rules.LegacyLabels {
		if strings.EqualFold(l, x) {
			return true
		}
	}
	return false
}

func containsFold(list []string, l string) bool {
	for _, x := range list {
		if strings.EqualFold(x, l) {
			return true
		}
	}
	return false
}

// classify reports whether labels are ours, the OS they map to, and whether
// they also need a label no runner on this host has (foreign): moving such a
// job to our pool would send it to the wrong machine.
func (a *analyzer) classify(labels []string) (ours bool, os core.OS, foreign bool) {
	var win, mac, extras bool
	nonDefault := 0
	for _, l := range labels {
		if a.legacy(l) {
			ours = true
		}
		ll := strings.ToLower(l)
		if strings.Contains(ll, "windows") || containsFold(a.rules.WindowsLabels, l) {
			win = true
		}
		if strings.Contains(ll, "macos") || containsFold(a.rules.MacLabels, l) {
			mac = true
		}
		if IsDefaultLabel(l) || a.ourLabel(l) {
			continue
		}
		nonDefault++
		if !containsFold(a.rules.LegacyLabels, l) && !containsFold(a.rules.WindowsLabels, l) && !containsFold(a.rules.MacLabels, l) {
			extras = true
		}
	}
	switch {
	case win:
		os = core.Windows
	case mac:
		os = core.MacOS
	default:
		os = core.Linux
	}
	foreign = ours && (extras || (nonDefault == 0 && !a.rules.BareHost))
	return ours, os, foreign
}

func (a *analyzer) ourLabel(l string) bool {
	p := a.rules.Prefix
	return p != "" && (l == p || strings.HasPrefix(l, p+"-"))
}

func (a *analyzer) newLabel(os core.OS) string { return naming.Label(a.rules.Prefix, os) }

// ---- runs-on forms ----

func (a *analyzer) record(n *yaml.Node, st, end int, form Form, os core.OS, newText string) {
	if a.done[st] {
		return
	}
	a.done[st] = true
	a.edits = append(a.edits, edit{st, end, newText})
	a.changes = append(a.changes, Change{
		Job: a.jobName, Line: n.Line, Column: n.Column, Form: form, OS: os,
		Old: string(a.src.b[st:end]), New: newText,
	})
}

func (a *analyzer) runsOn(j, n *yaml.Node) {
	switch n.Kind {
	case yaml.AliasNode:
		a.skip(n, ReasonUnsupportedForm, "alias", "")
	case yaml.ScalarNode:
		if strings.Contains(n.Value, "${{") {
			a.expression(j, n)
			return
		}
		a.scalarLabel(n, []string{n.Value}, FormPlain)
	case yaml.SequenceNode:
		a.sequence(n)
	case yaml.MappingNode:
		labels := mapGet(n, "labels")
		if labels == nil {
			a.skip(n, ReasonGroupOnly, "", "")
			return
		}
		a.runsOn(j, labels)
	default:
		a.skip(n, ReasonUnsupportedForm, "", "")
	}
}

// scalarLabel rewrites a literal scalar holding the given labels (one value).
func (a *analyzer) scalarLabel(n *yaml.Node, labels []string, form Form) {
	ours, os, foreign := a.classify(labels)
	if !ours {
		a.untouched = append(a.untouched, Untouched{Job: a.jobName, Line: n.Line, Text: n.Value})
		return
	}
	if foreign {
		a.skip(n, ReasonForeignLabels, n.Value, "")
		return
	}
	if os == core.MacOS {
		a.skip(n, ReasonMacOS, n.Value, "")
		return
	}
	st, end, ok := a.src.scalarSpan(n)
	if !ok {
		a.skip(n, ReasonUnsupportedForm, n.Value, "")
		return
	}
	a.record(n, st, end, form, os, requote(n, a.newLabel(os)))
}

func requote(n *yaml.Node, v string) string {
	switch {
	case n.Style&yaml.SingleQuotedStyle != 0:
		return "'" + v + "'"
	case n.Style&yaml.DoubleQuotedStyle != 0:
		return `"` + v + `"`
	}
	return v
}

func (a *analyzer) sequence(n *yaml.Node) {
	var labels []string
	hasExpr := false
	for _, it := range n.Content {
		if it.Kind != yaml.ScalarNode {
			a.skip(n, ReasonUnsupportedForm, "non-scalar item", "")
			return
		}
		if strings.Contains(it.Value, "${{") {
			hasExpr = true
		}
		labels = append(labels, it.Value)
	}
	if len(labels) == 0 {
		return
	}
	ours, os, foreign := a.classify(labels)
	first := n.Content[0]
	if !ours {
		a.untouched = append(a.untouched, Untouched{Job: a.jobName, Line: first.Line, Text: strings.Join(labels, ", ")})
		return
	}
	if foreign {
		a.skip(first, ReasonForeignLabels, strings.Join(labels, ", "), "")
		return
	}
	if hasExpr {
		a.skip(first, ReasonUnsupportedExpr, strings.Join(labels, ", "), "")
		return
	}
	if os == core.MacOS {
		a.skip(first, ReasonMacOS, strings.Join(labels, ", "), "")
		return
	}
	st, _, ok1 := a.src.scalarSpan(first)
	_, end, ok2 := a.src.scalarSpan(n.Content[len(n.Content)-1])
	if !ok1 || !ok2 {
		a.skip(first, ReasonUnsupportedForm, strings.Join(labels, ", "), "")
		return
	}
	a.record(first, st, end, FormSequence, os, a.newLabel(os))
}

// ---- expressions ----

var (
	varRE    = regexp.MustCompile(`\bvars\.([A-Za-z_][A-Za-z0-9_]*)`)
	matrixRE = regexp.MustCompile(`\bmatrix\.([A-Za-z_][A-Za-z0-9_-]*)`)
)

type literal struct {
	start, end int // absolute byte span including quotes
	value      string
}

// expressionLiterals finds single-quoted literals inside ${{ }} regions of the
// raw scalar text, plus the expression text itself.
func expressionLiterals(raw []byte, base int) (lits []literal, expr string) {
	var sb strings.Builder
	i := 0
	for i < len(raw) {
		o := bytes.Index(raw[i:], []byte("${{"))
		if o < 0 {
			break
		}
		i += o + 3
		regionStart := i
		for i < len(raw) {
			if raw[i] == '\'' {
				j := i + 1
				var v strings.Builder
				for j < len(raw) {
					if raw[j] == '\'' {
						if j+1 < len(raw) && raw[j+1] == '\'' {
							v.WriteByte('\'')
							j += 2
							continue
						}
						break
					}
					v.WriteByte(raw[j])
					j++
				}
				if j >= len(raw) {
					return lits, sb.String()
				}
				lits = append(lits, literal{base + i, base + j + 1, v.String()})
				i = j + 1
				continue
			}
			if raw[i] == '}' && i+1 < len(raw) && raw[i+1] == '}' {
				break
			}
			i++
		}
		sb.Write(raw[regionStart:min(i, len(raw))])
		sb.WriteByte(' ')
		i += 2
	}
	return lits, sb.String()
}

// literalAllowed decides whether a literal sits in a value position (not a
// comparison operand or an argument of a function other than fromJSON).
func literalAllowed(b []byte, l literal) bool {
	i := l.start - 1
	for i >= 0 && (b[i] == ' ' || b[i] == '\t') {
		i--
	}
	if i >= 0 {
		switch b[i] {
		case '=', '!', '<', '>', ',':
			return false
		case '(':
			j := i - 1
			for j >= 0 && (b[j] == ' ' || b[j] == '\t') {
				j--
			}
			k := j
			for k >= 0 && (b[k] == '_' || b[k] >= 'A' && b[k] <= 'Z' || b[k] >= 'a' && b[k] <= 'z') {
				k--
			}
			name := string(b[k+1 : j+1])
			if name != "" && !strings.EqualFold(name, "fromJSON") {
				return false
			}
		}
	}
	k := l.end
	for k < len(b) && (b[k] == ' ' || b[k] == '\t') {
		k++
	}
	if k+1 < len(b) && (b[k] == '=' || b[k] == '!') && b[k+1] == '=' {
		return false
	}
	return true
}

// jsonLabels parses a JSON array of strings.
func jsonLabels(s string) ([]string, bool) {
	t := strings.TrimSpace(s)
	if !strings.HasPrefix(t, "[") {
		return nil, false
	}
	var out []string
	if err := json.Unmarshal([]byte(t), &out); err != nil {
		return nil, false
	}
	return out, true
}

func (a *analyzer) expression(j, n *yaml.Node) {
	st, end, ok := a.src.scalarSpan(n)
	if !ok || (n.Style&yaml.SingleQuotedStyle != 0 && strings.Contains(string(a.src.b[st:end]), "''")) {
		a.skip(n, ReasonUnsupportedExpr, n.Value, "")
		return
	}
	lits, expr := expressionLiterals(a.src.b[st:end], st)
	before := len(a.changes)
	skipsBefore := len(a.skips)
	for _, l := range lits {
		a.expressionLiteral(n, l)
	}
	seen := map[string]bool{}
	for _, m := range varRE.FindAllStringSubmatch(expr, -1) {
		if !seen[m[1]] {
			seen[m[1]] = true
			a.skip(n, ReasonVariable, n.Value, m[1])
		}
	}
	for _, m := range matrixRE.FindAllStringSubmatch(expr, -1) {
		if !seen["matrix."+m[1]] {
			seen["matrix."+m[1]] = true
			a.matrixKey(j, n, m[1])
		}
	}
	if len(a.changes) == before {
		onlyVars := true
		for _, s := range a.skips[skipsBefore:] {
			if s.Reason != ReasonVariable {
				onlyVars = false
			}
		}
		if onlyVars {
			a.untouched = append(a.untouched, Untouched{Job: a.jobName, Line: n.Line, Text: n.Value})
		}
	}
}

func (a *analyzer) expressionLiteral(n *yaml.Node, l literal) {
	form := FormExpression
	labels := []string{l.value}
	asJSON := false
	if ls, ok := jsonLabels(l.value); ok {
		labels, form, asJSON = ls, FormExpressionJSON, true
	}
	ours, os, foreign := a.classify(labels)
	if !ours {
		return
	}
	at := &yaml.Node{Line: n.Line, Column: n.Column}
	if foreign {
		a.skip(at, ReasonForeignLabels, l.value, "")
		return
	}
	if os == core.MacOS {
		a.skip(at, ReasonMacOS, l.value, "")
		return
	}
	if !literalAllowed(a.src.b, l) {
		a.skip(at, ReasonUnsupportedExpr, l.value, "")
		return
	}
	nl := a.newLabel(os)
	text := "'" + nl + "'"
	if asJSON {
		text = `'"` + nl + `"'`
	}
	line, col := a.lineCol(l.start)
	if a.done[l.start] {
		return
	}
	a.done[l.start] = true
	a.edits = append(a.edits, edit{l.start, l.end, text})
	a.changes = append(a.changes, Change{Job: a.jobName, Line: line, Column: col, Form: form, OS: os,
		Old: string(a.src.b[l.start:l.end]), New: text})
}

func (a *analyzer) lineCol(off int) (int, int) {
	line := sort.Search(len(a.src.lineStarts), func(i int) bool { return a.src.lineStarts[i] > off })
	return line, utf8.RuneCount(a.src.b[a.src.lineStarts[line-1]:off]) + 1
}

// ---- matrix ----

func (a *analyzer) matrixKey(j, at *yaml.Node, key string) {
	matrix := mapGet(mapGet(j, "strategy"), "matrix")
	if matrix == nil || matrix.Kind != yaml.MappingNode {
		a.skip(at, ReasonMatrixUnresolved, "matrix."+key, "")
		return
	}
	found := false
	before := len(a.changes)
	visit := func(v *yaml.Node) {
		found = true
		switch v.Kind {
		case yaml.ScalarNode:
			a.matrixValue(v)
		case yaml.SequenceNode:
			for _, it := range v.Content {
				if it.Kind == yaml.ScalarNode {
					a.matrixValue(it)
				}
			}
		}
	}
	if v := mapGet(matrix, key); v != nil && key != "include" && key != "exclude" {
		visit(v)
	}
	for _, list := range []string{"include", "exclude"} {
		if inc := mapGet(matrix, list); inc != nil && inc.Kind == yaml.SequenceNode {
			for _, e := range inc.Content {
				if v := mapGet(e, key); v != nil {
					visit(v)
				}
			}
		}
	}
	if !found {
		a.skip(at, ReasonMatrixUnresolved, "matrix."+key, "")
		return
	}
	if len(a.changes) > before {
		a.reportMatrixRefs(j, at, matrix, key)
	}
}

// reportMatrixRefs reports every other use of matrix.<key> in the job (an
// `if:` or a step that compares the old value would now be wrong).
func (a *analyzer) reportMatrixRefs(j, runsOn, matrix *yaml.Node, key string) {
	var walk func(n *yaml.Node)
	walk = func(n *yaml.Node) {
		if n == nil || n == runsOn || n == matrix {
			return
		}
		if n.Kind == yaml.ScalarNode {
			for _, m := range matrixRE.FindAllStringSubmatch(n.Value, -1) {
				if m[1] == key {
					a.skip(n, ReasonMatrixKeyUsed, n.Value, "")
					break
				}
			}
			return
		}
		for _, c := range n.Content {
			walk(c)
		}
	}
	walk(j)
}

func (a *analyzer) matrixValue(n *yaml.Node) {
	if strings.Contains(n.Value, "${{") {
		a.skip(n, ReasonUnsupportedExpr, n.Value, "")
		return
	}
	labels, form := []string{n.Value}, FormMatrix
	asJSON := false
	if ls, ok := jsonLabels(n.Value); ok {
		labels, form, asJSON = ls, FormMatrixJSON, true
	}
	ours, os, foreign := a.classify(labels)
	if !ours {
		return
	}
	if foreign {
		a.skip(n, ReasonForeignLabels, n.Value, "")
		return
	}
	if os == core.MacOS {
		a.skip(n, ReasonMacOS, n.Value, "")
		return
	}
	st, end, ok := a.src.scalarSpan(n)
	if !ok {
		a.skip(n, ReasonUnsupportedForm, n.Value, "")
		return
	}
	nl := a.newLabel(os)
	text := requote(n, nl)
	if asJSON {
		switch {
		case n.Style&yaml.DoubleQuotedStyle != 0:
			text = `"\"` + nl + `\""`
		case n.Style&yaml.SingleQuotedStyle != 0:
			text = `'"` + nl + `"'`
		default:
			a.skip(n, ReasonUnsupportedForm, n.Value, "")
			return
		}
	}
	a.record(n, st, end, form, os, text)
}

// RewriteVarValue rewrites the value of an Actions variable that holds
// runner labels (plain label, or a JSON array/string used with fromJSON). It
// returns ok=false when the value is not ours or maps to macOS.
func RewriteVarValue(value string, rules Rules) (string, bool) {
	a := &analyzer{rules: rules}
	t := strings.TrimSpace(value)
	if ls, ok := jsonLabels(t); ok {
		ours, os, foreign := a.classify(ls)
		if !ours || foreign || os == core.MacOS {
			return "", false
		}
		return `"` + a.newLabel(os) + `"`, true
	}
	ours, os, foreign := a.classify([]string{t})
	if !ours || foreign || os == core.MacOS {
		return "", false
	}
	return a.newLabel(os), true
}
