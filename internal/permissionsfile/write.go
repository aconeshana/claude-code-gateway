package permissionsfile

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// ErrAlreadyExists is returned by Add when the exact (source, behavior,
// content) triple already lives in the target settings.json. Callers
// should treat this as success (the desired state is reached) rather than
// surfacing it as an error.
var ErrAlreadyExists = errors.New("rule already exists")

// ErrNotFound is returned by Remove when the rule does not exist in the
// target file. Like ErrAlreadyExists, callers may want to treat this as
// success-by-other-means (the rule isn't there, which is what they
// wanted).
var ErrNotFound = errors.New("rule not found")

// Add inserts a single rule into the target source's settings.json.
// Creates the file (and its parent .claude directory) if missing.
// Preserves all existing top-level keys (model, hooks, env, ...). Other
// rules in the same behavior list are kept; this is an append.
//
// Returns ErrAlreadyExists when the rule string is already present in
// the same behavior list at the same source — callers should treat that
// as a no-op success, not a fatal error.
func Add(rule Rule, projectDir string) error {
	if !IsValidSource(rule.Source) {
		return fmt.Errorf("invalid source: %q", rule.Source)
	}
	if !IsValidBehavior(rule.Behavior) {
		return fmt.Errorf("invalid behavior: %q", rule.Behavior)
	}
	if rule.Content == "" {
		return fmt.Errorf("rule content is empty")
	}
	path := SettingsPath(rule.Source, projectDir)
	if path == "" {
		return fmt.Errorf("cannot resolve settings path for source %q (projectDir=%q)",
			rule.Source, projectDir)
	}

	doc, err := readSettings(path)
	if err != nil {
		return err
	}
	perms := decodePermissions(doc)
	list := perms[string(rule.Behavior)]
	for _, existing := range list {
		if existing == rule.Content {
			return ErrAlreadyExists
		}
	}
	perms[string(rule.Behavior)] = append(list, rule.Content)
	encodePermissions(doc, perms)

	return writeSettings(path, doc)
}

// Remove deletes the first occurrence of (rule.Source, rule.Behavior,
// rule.Content) from the target file. If the file or rule is missing,
// returns ErrNotFound — caller can treat that as already-achieved state.
//
// Empty behavior arrays are left in place after removal (e.g. {"allow":
// []}) — preserving the schema shape matches what the claude-code TUI
// would write. Callers that want stricter cleanup can read the file
// again afterwards.
func Remove(rule Rule, projectDir string) error {
	if !IsValidSource(rule.Source) {
		return fmt.Errorf("invalid source: %q", rule.Source)
	}
	if !IsValidBehavior(rule.Behavior) {
		return fmt.Errorf("invalid behavior: %q", rule.Behavior)
	}
	if rule.Content == "" {
		return fmt.Errorf("rule content is empty")
	}
	path := SettingsPath(rule.Source, projectDir)
	if path == "" {
		return fmt.Errorf("cannot resolve settings path for source %q (projectDir=%q)",
			rule.Source, projectDir)
	}

	doc, err := readSettings(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return ErrNotFound
		}
		return err
	}
	perms := decodePermissions(doc)
	list := perms[string(rule.Behavior)]
	idx := -1
	for i, existing := range list {
		if existing == rule.Content {
			idx = i
			break
		}
	}
	if idx == -1 {
		return ErrNotFound
	}
	perms[string(rule.Behavior)] = append(list[:idx], list[idx+1:]...)
	encodePermissions(doc, perms)

	return writeSettings(path, doc)
}

// readSettings reads and JSON-decodes the settings.json at path into a
// flat top-level map. Missing files return an empty map (not an error)
// for Add's create-on-write path — Remove handles fs.ErrNotExist itself
// because "remove from missing file" has different semantics than "add
// to missing file".
func readSettings(path string) (map[string]json.RawMessage, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return map[string]json.RawMessage{}, nil
		}
		return nil, err
	}
	if len(data) == 0 {
		return map[string]json.RawMessage{}, nil
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("parse JSON at %s: %w", path, err)
	}
	if doc == nil {
		doc = map[string]json.RawMessage{}
	}
	return doc, nil
}

// decodePermissions extracts the `permissions` block as a typed map.
// Returns an empty map when permissions key is absent or empty —
// callers always get a mutable structure to append to.
//
// We keep the three behavior arrays separately rather than flattening
// because settings.json schema requires them as top-level keys in the
// permissions block; downstream encodePermissions will set them back in
// the same shape.
func decodePermissions(doc map[string]json.RawMessage) map[string][]string {
	perms := map[string][]string{
		string(BehaviorAllow): {},
		string(BehaviorDeny):  {},
		string(BehaviorAsk):   {},
	}
	rawPerms, ok := doc["permissions"]
	if !ok || len(rawPerms) == 0 {
		return perms
	}
	var existing map[string][]string
	if err := json.Unmarshal(rawPerms, &existing); err != nil {
		// Malformed permissions block — be defensive: start fresh rather
		// than failing the whole write. The user is editing this via
		// Gateway, so a clean rewrite is the recovery path.
		return perms
	}
	for k, v := range existing {
		perms[k] = v
	}
	return perms
}

// encodePermissions sets doc["permissions"] from the typed map. Empty
// behavior arrays are preserved (not dropped) to keep the schema shape
// stable and predictable for users inspecting the JSON file directly.
func encodePermissions(doc map[string]json.RawMessage, perms map[string][]string) {
	out := map[string][]string{}
	for _, b := range AllBehaviors {
		list := perms[string(b)]
		if list == nil {
			list = []string{}
		}
		out[string(b)] = list
	}
	raw, _ := json.Marshal(out) // map[string][]string never fails to marshal
	doc["permissions"] = raw
}

// writeSettings JSON-encodes doc and atomically writes it to path.
// Creates the parent .claude directory if missing (0o755) and writes
// with 0o600 to match claude-code TUI's write permissions for
// settings.json (the file may carry secrets in env/hooks).
func writeSettings(path string, doc map[string]json.RawMessage) error {
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return fmt.Errorf("encode settings: %w", err)
	}
	data = append(data, '\n')
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, ".settings-*.json.tmp")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tmpName := tmp.Name()
	cleanup := func() {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
	}
	if _, err := tmp.Write(data); err != nil {
		cleanup()
		return fmt.Errorf("write temp file: %w", err)
	}
	if err := tmp.Chmod(0o600); err != nil {
		cleanup()
		return fmt.Errorf("chmod temp file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		cleanup()
		return fmt.Errorf("sync temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("close temp file: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("rename %s → %s: %w", tmpName, path, err)
	}
	return nil
}
