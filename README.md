# zemi @··

> There are many agent harnesses, but this one takes _the best from them_

<img align="center" width="800" alt="zemi" src="https://github.com/user-attachments/assets/e408a167-6126-45b8-aced-64b6ae726328" />

Zemi is a (coding) agent harness \[insert buzzword\] that...

- learns like [hermes](https://github.com/nousresearch/hermes-agent)
- fast and native like [crush](https://github.com/charmbracelet/crush)
- extensible and cache-efficient like [pi](https://github.com/earendil-works/pi)
- batteries included like [codex](https://github.com/openai/codex) & [claude-code](https://github.com/anthropics/claude-code)
- has a nice TUI like [opencode](https://github.com/anomalyco/opencode)

...and can be whatever you want it to be.

> [!NOTE]
> I use zemi daily exclusively and have better experience with it than with other harnesses.
> However, things may be different for you. If you like this project but something is not working well, open an issue / PR.
> Expect breaking changes until v1.

## Install

```sh
go install github.com/metafates/zemi@latest

# or ANTHROPIC_API_KEY, OPENAI_API_KEY, or any other provider zemi supports.
# zemi will guide you if not set
export DEEPSEEK_API_KEY=...

zemi
```

Or let another agent set it up for you:

```markdown
Install github.com/metafates/zemi (go install github.com/metafates/zemi@latest), ask which model provider I use and put its API key in my shell config, then print the one command I should run.
```

## Features

- Keeps prompts small, cache hit rates high
- Knows when to shut up
- Does more with less steps
- Tools to make cheap models suck less, smart ones do more: subagents, workflows, forks, parallel tool calls... _yada yada_
- Fully scriptable with Lua and knows how to modify itself
- Learns from previous sessions
- Compatible with Claude Code plugins
