package codegraph

// tsconfig.json is JSONC: it permits // and /* */ comments and trailing
// commas. encoding/json rejects both, so the content is sanitized first.
// Comments are replaced with spaces (preserving offsets) and trailing commas
// are removed; string literals are respected throughout.

// stripJSONCComments replaces // line comments and /* block comments with
// spaces, leaving string contents untouched.
func stripJSONCComments(data []byte) []byte {
	out := make([]byte, len(data))
	copy(out, data)

	const (
		stateCode = iota
		stateString
		stateLineComment
		stateBlockComment
	)
	state := stateCode
	escaped := false

	for i := 0; i < len(out); i++ {
		c := out[i]
		switch state {
		case stateCode:
			switch {
			case c == '"':
				state = stateString
			case c == '/' && i+1 < len(out) && out[i+1] == '/':
				state = stateLineComment
				out[i] = ' '
			case c == '/' && i+1 < len(out) && out[i+1] == '*':
				state = stateBlockComment
				out[i] = ' '
			}
		case stateString:
			switch {
			case escaped:
				escaped = false
			case c == '\\':
				escaped = true
			case c == '"':
				state = stateCode
			}
		case stateLineComment:
			if c == '\n' {
				state = stateCode
			} else {
				out[i] = ' '
			}
		case stateBlockComment:
			if c == '*' && i+1 < len(out) && out[i+1] == '/' {
				out[i] = ' '
				out[i+1] = ' '
				i++
				state = stateCode
			} else if c != '\n' {
				out[i] = ' '
			}
		}
	}
	return out
}

// stripTrailingCommas removes commas that are followed (across whitespace)
// by a closing brace or bracket, outside of string literals.
func stripTrailingCommas(data []byte) []byte {
	out := make([]byte, 0, len(data))
	inString := false
	escaped := false

	for i := 0; i < len(data); i++ {
		c := data[i]
		if inString {
			switch {
			case escaped:
				escaped = false
			case c == '\\':
				escaped = true
			case c == '"':
				inString = false
			}
			out = append(out, c)
			continue
		}
		switch c {
		case '"':
			inString = true
			out = append(out, c)
		case ',':
			j := i + 1
			for j < len(data) && isJSONSpace(data[j]) {
				j++
			}
			if j < len(data) && (data[j] == '}' || data[j] == ']') {
				continue // trailing comma: drop it
			}
			out = append(out, c)
		default:
			out = append(out, c)
		}
	}
	return out
}

func isJSONSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r'
}
