package permissionsfile

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"sort"
)

// Load returns every allow/deny/ask rule across all three sources for the
// given project. Missing files are skipped silently — Load only errors
// when a file exists but cannot be parsed (malformed JSON). Returns the
// merged list sorted by Source × Behavior × Content for stable rendering.
//
// projectDir may be empty when the caller has no current project; in that
// case only SourceUser is consulted.
//
// Important: this is a read of what's *written to disk*. claude-code's
// runtime resolution applies source precedence (local > project > user)
// when there are conflicts — see https://docs.anthropic.com/... for the
// full decision logic. Callers presenting this list to users should
// display the source tag so users understand why a rule may not be in
// effect (e.g. shadowed by a local deny).
func Load(projectDir string) ([]Rule, error) {
	var out []Rule
	for _, src := range AllSources {
		path := SettingsPath(src, projectDir)
		if path == "" {
			continue
		}
		rules, err := loadFromFile(path, src)
		if err != nil {
			return nil, fmt.Errorf("load %s settings (%s): %w", src, path, err)
		}
		out = append(out, rules...)
	}
	sortRules(out)
	return out, nil
}

// loadFromFile reads a single settings.json and returns rules tagged with
// the given source. Returns (nil, nil) when the file does not exist or
// has no `permissions` block — both states mean "no rules from here", not
// an error. Empty arrays inside `permissions` are likewise just an empty
// rule slice, not an error.
func loadFromFile(path string, src Source) ([]Rule, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	if len(data) == 0 {
		return nil, nil
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("parse JSON: %w", err)
	}
	rawPerms, ok := doc["permissions"]
	if !ok || len(rawPerms) == 0 {
		return nil, nil
	}
	var perms map[string][]string
	if err := json.Unmarshal(rawPerms, &perms); err != nil {
		return nil, fmt.Errorf("parse permissions block: %w", err)
	}
	var out []Rule
	for _, b := range AllBehaviors {
		for _, content := range perms[string(b)] {
			if content == "" {
				continue
			}
			out = append(out, Rule{Source: src, Behavior: b, Content: content})
		}
	}
	return out, nil
}

// sortRules orders rules by Source (user → project → local), then
// Behavior (allow → deny → ask), then Content (lexicographic). Used by
// Load to produce a stable list independent of map iteration order.
func sortRules(rules []Rule) {
	sourceIdx := map[Source]int{SourceUser: 0, SourceProject: 1, SourceLocal: 2}
	behaviorIdx := map[Behavior]int{BehaviorAllow: 0, BehaviorDeny: 1, BehaviorAsk: 2}
	sort.SliceStable(rules, func(i, j int) bool {
		if a, b := sourceIdx[rules[i].Source], sourceIdx[rules[j].Source]; a != b {
			return a < b
		}
		if a, b := behaviorIdx[rules[i].Behavior], behaviorIdx[rules[j].Behavior]; a != b {
			return a < b
		}
		return rules[i].Content < rules[j].Content
	})
}
