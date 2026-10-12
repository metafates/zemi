# zemi @··

> There are many agent harnesses, but this one takes _the best from them_

Zemi is a (coding) agent harness that...

Site: https://metafates.github.io/zemi

- learns like [hermes](https://github.com/nousresearch/hermes-agent)
- fast and native like [crush](https://github.com/charmbracelet/crush)
- extensible and cache-efficient like [pi](https://github.com/earendil-works/pi)
- batteries included like [codex](https://github.com/openai/codex) & [claude-code](https://github.com/anthropics/claude-code)
- has a nice TUI like [opencode](https://github.com/anomalyco/opencode)

...and can be whatever you want it to be.

## Install

```sh
go install github.com/metafates/zemi@latest

# or ANTHROPIC_API_KEY, OPENAI_API_KEY, or any other provider zemi supports.
# zemi will guide you if not set
export DEEPSEEK_API_KEY=...

zemi
```

## Features

- Keeps prompts small, cache hit rates high
- Knows when to shut up
- Does more with less steps
- Tools to make cheap models suck less, smart ones do more: subagents, workflows, forks, lsp, parallel tool calls... _yada yada_
- Fully scriptable with Lua and knows how to modify itself
- Learns from previous sessions
- Sandbox and deterministic permission resolution - you'll see "allow/deny" prompts way less
