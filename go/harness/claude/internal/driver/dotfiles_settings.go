package driver

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"
)

// dotfilesSettingKeys are the settings an owner's dotfiles may add, per
// top-level object: the auto mode classifier's rules and permission rules.
// Everything else (hooks, env, defaultMode, disableBypassPermissionsMode, MCP,
// helpers) stays the harness's, so dotfiles cannot undo a locked mode.
var dotfilesSettingKeys = map[string][]string{
	"autoMode":    {"environment", "allow", "soft_deny", "hard_deny"},
	"permissions": {"allow", "deny", "ask"},
}

// FilterDotfilesSettings keeps the allowed rule lists of a dotfiles
// claude/settings.json. Each kept value is a list of non-empty strings. It
// returns the kept settings and the keys it dropped, sorted. A file that is
// not a JSON object, or an allowed key that is not a list of strings, is an
// error: the whole file is then ignored.
func FilterDotfilesSettings(raw []byte) (map[string]any, []string, error) {
	var settings map[string]json.RawMessage
	decoder := json.NewDecoder(bytes.NewReader(raw))
	if err := decoder.Decode(&settings); err != nil || settings == nil {
		return nil, nil, fmt.Errorf("dotfiles claude/settings.json is not a JSON object")
	}
	if decoder.More() {
		return nil, nil, fmt.Errorf("dotfiles claude/settings.json has trailing data")
	}
	kept := map[string]any{}
	var dropped []string
	for top, value := range settings {
		allowed, known := dotfilesSettingKeys[top]
		if !known {
			if top != "$schema" {
				dropped = append(dropped, top)
			}
			continue
		}
		var section map[string]json.RawMessage
		if err := json.Unmarshal(value, &section); err != nil || section == nil {
			return nil, nil, fmt.Errorf("dotfiles claude/settings.json %s is not an object", top)
		}
		out := map[string]any{}
		for key, list := range section {
			if !slices.Contains(allowed, key) {
				dropped = append(dropped, top+"."+key)
				continue
			}
			var rules []string
			if err := json.Unmarshal(list, &rules); err != nil {
				return nil, nil, fmt.Errorf("dotfiles claude/settings.json %s.%s is not a list of strings", top, key)
			}
			for _, rule := range rules {
				if strings.TrimSpace(rule) == "" {
					return nil, nil, fmt.Errorf("dotfiles claude/settings.json %s.%s has an empty entry", top, key)
				}
			}
			if len(rules) != 0 {
				out[key] = rules
			}
		}
		if len(out) != 0 {
			kept[top] = out
		}
	}
	sort.Strings(dropped)
	return kept, dropped, nil
}

// MergeDotfilesSettings adds filtered dotfiles settings under the harness's
// settings layer. Lists are joined, the dotfiles' entries first and duplicates
// dropped; on any other key the harness's value stands.
func MergeDotfilesSettings(harness string, dotfiles map[string]any) (string, error) {
	if len(dotfiles) == 0 {
		return harness, nil
	}
	var layer map[string]any
	if err := json.Unmarshal([]byte(harness), &layer); err != nil || layer == nil {
		return "", fmt.Errorf("decode Claude settings layer: %v", err)
	}
	merged, ok := mergeSettingValue(dotfiles, layer).(map[string]any)
	if !ok {
		return "", fmt.Errorf("merge Claude settings layer")
	}
	encoded, err := json.Marshal(merged)
	if err != nil {
		return "", fmt.Errorf("encode Claude settings layer: %w", err)
	}
	return string(encoded), nil
}

func mergeSettingValue(base, over any) any {
	switch o := over.(type) {
	case map[string]any:
		b, ok := base.(map[string]any)
		if !ok {
			return o
		}
		out := make(map[string]any, len(b)+len(o))
		for k, v := range b {
			out[k] = v
		}
		for k, v := range o {
			if existing, found := out[k]; found {
				out[k] = mergeSettingValue(existing, v)
			} else {
				out[k] = v
			}
		}
		return out
	case []any:
		var joined []any
		seen := map[string]bool{}
		add := func(v any) {
			key, _ := json.Marshal(v)
			if !seen[string(key)] {
				seen[string(key)] = true
				joined = append(joined, v)
			}
		}
		switch b := base.(type) {
		case []string:
			for _, v := range b {
				add(v)
			}
		case []any:
			for _, v := range b {
				add(v)
			}
		default:
			return o
		}
		for _, v := range o {
			add(v)
		}
		return joined
	default:
		return over
	}
}
