# zemi @··

> There are many agent harnesses, but this one takes _the best from them_

Zemi is a (coding) agent harness that...

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

If you prefer your other agent setup zemi for you, here's a prompt:

```markdown
Set up zemi for me from github.com/metafates/zemi:
install the binary (go install github.com/metafates/zemi@latest, or build from source),
ask me which model provider I use, and put its API key in my shell config.
Print the one command I should run next.
```

## Features

- Keeps prompts small, cache hit rates high
- Knows when to shut up
- Does more with less steps
- Tools to make cheap models suck less, smart ones do more: subagents, workflows, forks, lsp, parallel tool calls... _yada yada_
- Fully scriptable with Lua and knows how to modify itself
- Learns from previous sessions
- Everything runs, and a plugin whose manifest declares `perm` may deny an action or force its ask overlay (`permission.request`); an ask no one can answer fails closed - it keeps you aware of what the agent does, it is not isolation

## Benchmarks

[DeepSWE](https://github.com/datacurve-ai/deep-swe) benchmark with `GLM-5.3-Flash`

| Agent | Task | Score | Tokens in/out | Time |
| ----- | ---- | ----- | ------------- | ---- |
| zemi | meriyah | 46/49 | 12.9M / 154k | 29m |
| zemi | query-persist | 8/8, reward 1.0 | 17.4M / 117k | 23m |
| opencode | meriyah | 48/49 | 25.2M / 137k | 46m |
| opencode | query-persist | 5/8 | 20.6M / 90k | 36m |

> Benchmark isn't fully ready yet, work in progress.
