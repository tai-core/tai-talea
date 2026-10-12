package controller

import "strings"

// argFlag returns the flag a rendered SGLang argument sets: the part before
// the first "=" or whitespace separator. Both "--tp-size=8" and "--tp-size 8"
// are accepted downstream, and both have to be recognised as the same flag
// here, otherwise an override would silently append a second copy instead of
// replacing the first.
func argFlag(argument string) string {
	value := strings.TrimSpace(argument)
	if value == "" {
		return ""
	}
	if index := strings.IndexAny(value, "= \t"); index >= 0 {
		return value[:index]
	}
	return value
}

// mergeSGLangArgs overlays the role-specific arguments on the shared list.
//
// The shared list carries what every worker needs; a role list carries the few
// entries that differ, and wins per flag. Replacing rather than appending
// matters: SGLang parses its command line with argparse, where a repeated flag
// is legal but only the last value is used, so an append-based override would
// leave the vector carrying two contradictory values that differ only by
// argument order. Here each flag appears at most once, and it keeps the
// position it first had - so a rendered command is easy to diff between roles.
func mergeSGLangArgs(base, override []string) []string {
	merged := make([]string, 0, len(base)+len(override))
	position := make(map[string]int, len(base)+len(override))

	apply := func(argument string) {
		flag := argFlag(argument)
		if flag == "" {
			return
		}
		if at, seen := position[flag]; seen {
			merged[at] = argument
			return
		}
		position[flag] = len(merged)
		merged = append(merged, argument)
	}

	for _, argument := range base {
		apply(argument)
	}
	for _, argument := range override {
		apply(argument)
	}
	return merged
}
