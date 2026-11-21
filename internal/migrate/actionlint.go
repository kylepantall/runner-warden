package migrate

import (
	"regexp"
	"strings"

	"github.com/kylepantall/runner-warden/internal/core"
	"github.com/kylepantall/runner-warden/internal/naming"
)

func actionlintLabels(prefix string) []string {
	return []string{naming.Label(prefix, core.Linux), naming.Label(prefix, core.Windows), prefix}
}

var labelsKey = regexp.MustCompile(`(?m)^([ \t]*)labels:[ \t]*\n`)

// AddActionlintLabels appends missing labels to the `self-hosted-runner:
// labels:` block list of an actionlint config, keeping everything else. It
// reports false when nothing changed or the file has no such block list.
func AddActionlintLabels(src []byte, labels []string) ([]byte, bool) {
	s := string(src)
	if !strings.Contains(s, "self-hosted-runner:") {
		return src, false
	}
	loc := labelsKey.FindStringSubmatchIndex(s)
	if loc == nil {
		return src, false
	}
	keyIndent := s[loc[2]:loc[3]]
	// Items follow the key, each "<indent>- value", the indent deeper than the key's.
	rest := s[loc[1]:]
	var items []string
	end := loc[1]
	itemIndent := ""
	for _, line := range strings.SplitAfter(rest, "\n") {
		t := strings.TrimLeft(line, " \t")
		ind := line[:len(line)-len(t)]
		if !strings.HasPrefix(t, "- ") || len(ind) <= len(keyIndent) {
			if strings.TrimSpace(line) == "" || strings.HasPrefix(t, "#") {
				end += len(line)
				continue
			}
			break
		}
		itemIndent = ind
		items = append(items, strings.Trim(strings.TrimSpace(strings.TrimPrefix(t, "- ")), `"'`))
		end += len(line)
	}
	if itemIndent == "" {
		return src, false
	}
	var add strings.Builder
	for _, l := range labels {
		found := false
		for _, it := range items {
			if it == l {
				found = true
			}
		}
		if !found {
			add.WriteString(itemIndent + "- " + l + "\n")
		}
	}
	if add.Len() == 0 {
		return src, false
	}
	// Insert after the last item (before trailing blank/comment lines that end the block).
	block := s[loc[1]:end]
	trimmed := strings.TrimRight(block, " \t\n")
	lastItemEnd := loc[1] + len(trimmed)
	if lastItemEnd < len(s) && s[lastItemEnd] == '\n' {
		lastItemEnd++
	} else {
		return []byte(s[:lastItemEnd] + "\n" + add.String() + s[lastItemEnd:]), true
	}
	return []byte(s[:lastItemEnd] + add.String() + s[lastItemEnd:]), true
}
