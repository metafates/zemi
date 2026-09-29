// instruction.go flags memory text phrased as instructions to the model
// (DESIGN §10.4): imperative verbs, second person and instruction-override
// phrasings. The heuristic is deliberately recall-biased: a false positive
// costs one review click, a missed "curl ... | sh" convention costs more.

package learn

import "regexp"

var (
	// imperative start: text opens with a command verb
	imperativeStart = regexp.MustCompile(
		`(?i)^\W*(run|use|always|never|ignore|disregard|forget|override|remember|ensure|make|set|add|remove|delete|install|execute|prefer|follow|stop|start|keep|do|dont|don't|write|read|check|call|curl|wget|sudo|export|source|apply|enable|disable|avoid|append|replace|drop)\b`,
	)

	// second person anywhere
	secondPerson = regexp.MustCompile(`(?i)\b(you|your|you're|youre|ur)\b`)

	// "always/never <anything>" phrasings anywhere
	alwaysNever = regexp.MustCompile(`(?i)\b(always|never)\b`)

	// explicit override phrasings
	overrides = regexp.MustCompile(`(?i)(ignore (all |any )?(previous|prior|above)|disregard (all |any |your |the )?(previous|prior|instructions)|forget (your|everything|all)|new instructions|system prompt|you are now|act as|override (your |the )?(instructions|prompt))`)
)

// InstructionShaped reports whether the text reads like an instruction to
// the model rather than a fact about the world (DESIGN §10.4).
func InstructionShaped(text string) bool {
	return imperativeStart.MatchString(text) ||
		secondPerson.MatchString(text) ||
		alwaysNever.MatchString(text) ||
		overrides.MatchString(text)
}
