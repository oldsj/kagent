package dotfiles

import "strings"

// Target is one CLI's user configuration directory and what it takes from a
// bundle.
type Target struct {
	// Dir is the CLI's user configuration directory: CLAUDE_CONFIG_DIR or CODEX_HOME.
	Dir string
	// place maps the bundle's files to the files laid out under Dir.
	place func([]File) []File
	// yieldsSkills means a skill already in Dir that the layout did not write
	// wins a name clash: the Agent's compiler-selected skills share Codex's root.
	yieldsSkills bool
}

// Claude lays out every file as it is in the bundle. Claude Code does not read
// AGENTS.md, so a bundle with AGENTS.md and no CLAUDE.md gets a CLAUDE.md that
// imports it.
func Claude(dir string) Target {
	return Target{Dir: dir, place: func(files []File) []File {
		var agents, claude bool
		for _, f := range files {
			agents = agents || f.Path == "AGENTS.md"
			claude = claude || f.Path == "CLAUDE.md"
		}
		out := append([]File(nil), files...)
		if agents && !claude {
			out = append(out, File{Path: "CLAUDE.md", Data: []byte("@AGENTS.md\n")})
		}
		return out
	}}
}

// Codex lays out AGENTS.md and skills. Codex agents are TOML and its custom
// prompts are deprecated, so subagents, commands and CLAUDE.md are not mapped.
// A compiler-selected skill wins a name clash with a dotfiles skill.
func Codex(dir string) Target {
	return Target{Dir: dir, yieldsSkills: true, place: func(files []File) []File {
		var out []File
		for _, f := range files {
			if f.Path == "AGENTS.md" || strings.HasPrefix(f.Path, "skills/") {
				out = append(out, f)
			}
		}
		return out
	}}
}

// entry is the top-level destination a file belongs to, the unit the layout
// writes, removes and checks: a skill's directory, or a single file.
func entry(p string) string {
	parts := strings.SplitN(p, "/", 3)
	if parts[0] == "skills" && len(parts) == 3 {
		return parts[0] + "/" + parts[1]
	}
	return p
}
