package cmd

import (
	"fmt"
	"strings"
)

// engineArgsUsage is the --engine-args help text. The splitting rule lives
// here because it is the part an operator gets wrong: the flag used to be
// a CSV slice, so a space-separated value reached vLLM as one argument.
const engineArgsUsage = `extra arguments for the engine, after --model/--host/--port. ` +
	`Each value is split like a shell command line: spaces separate arguments and single or double quotes group them, ` +
	`so --engine-args "--max-model-len 32768 --tensor-parallel-size 8" passes four arguments. ` +
	`Unquoted commas also separate arguments, so the older --engine-args --max-model-len,8192 form still works; ` +
	`quote an argument that needs a literal comma or space (e.g. JSON). Repeatable`

// splitEngineArgs turns the raw --engine-args values into the engine's argv.
//
// Whitespace and unquoted commas both separate arguments. Commas are kept
// as separators because every example written against the old CSV flag
// uses them, and an argument containing one (JSON, mostly) could not be
// passed through the CSV parse anyway, so nothing that worked before
// changes meaning. Quoting is the escape hatch for both.
func splitEngineArgs(raw []string) ([]string, error) {
	var out []string
	for _, v := range raw {
		args, err := shellSplit(v)
		if err != nil {
			return nil, fmt.Errorf("--engine-args %q: %w", v, err)
		}
		out = append(out, args...)
	}
	return out, nil
}

// shellSplit is a small POSIX-ish word splitter: no expansion, no globbing,
// just quotes, backslash escapes, and whitespace or comma as separators.
func shellSplit(s string) ([]string, error) {
	var (
		out    []string
		cur    strings.Builder
		inWord bool // true once the word has content or an empty quoted pair
		quote  rune // 0, '\'' or '"'
		escape bool
	)
	flush := func() {
		if inWord {
			out = append(out, cur.String())
		}
		cur.Reset()
		inWord = false
	}
	for _, r := range s {
		switch {
		case escape:
			cur.WriteRune(r)
			inWord, escape = true, false
		case quote == '\'':
			if r == '\'' {
				quote = 0
			} else {
				cur.WriteRune(r)
			}
		case quote == '"':
			switch r {
			case '"':
				quote = 0
			case '\\':
				escape = true
			default:
				cur.WriteRune(r)
			}
		case r == '\'' || r == '"':
			quote, inWord = r, true
		case r == '\\':
			escape = true
		case r == ',' || r == ' ' || r == '\t' || r == '\n' || r == '\r':
			flush()
		default:
			cur.WriteRune(r)
			inWord = true
		}
	}
	if quote != 0 {
		return nil, fmt.Errorf("unterminated %c quote", quote)
	}
	if escape {
		return nil, fmt.Errorf("trailing backslash")
	}
	flush()
	return out, nil
}
