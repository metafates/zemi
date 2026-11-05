<div align="center">
	<img width="300" alt="zemi" src="./assets/zemi.png" />
	<h1>zemi @··</h1>
	<p>There are many agent harnesses, but this one takes <i>the best from them</i></p>
</div>

<div align="center">
	<img width="800" alt="zemi chat" src="https://github.com/user-attachments/assets/45797dac-3470-4983-863c-fc23c223d473" />
</div>

Zemi is a (coding) agent harness that...

- never forgets _anything_ with endless chats like [UniiChat](https://gist.github.com/VictorTaelin/91837951a5ce5b38f341ec1ba1df6449).
- fast and native like [crush](https://github.com/charmbracelet/crush)
- extensible and cache-efficient like [pi](https://github.com/earendil-works/pi)
- batteries included like [codex](https://github.com/openai/codex) & [claude-code](https://github.com/anthropics/claude-code)
- has a nice TUI like [opencode](https://github.com/anomalyco/opencode)

...and can be whatever you want it to be.

> [!NOTE]
> I use zemi daily exclusively and have better experience with it than with other harnesses.
> However, things may be different for you. If you like this project but something is not working well, open an issue / PR.

Zemi is still in preview and iterating rapidly. There will be compatibility-breaking changes.

## Install

```sh
go install github.com/metafates/zemi@latest
```

Then just

```bash
# or ANTHROPIC_API_KEY, OPENAI_API_KEY, or any other provider zemi supports.
# zemi will guide you if not set
export DEEPSEEK_API_KEY=...

zemi
```

Or let another agent set it up for you:

```markdown
Install zemi with `go install github.com/metafates/zemi@latest`.
Then, follow `zemi docs setup` output - it guides you how to setup zemi.
```

## Features

- Keeps prompts small, cache hit rates high
- Knows when to shut up
- Endless chats that remember every message
- Does more with less steps
- Subagents, workflows, background jobs, lsp, mcp, and more.
- Fully scriptable with Lua and knows how to modify itself
- Compatible with Claude Code plugins

## API keys

You can use zemi with any provider.

| Environment Variable        | Provider                                           |
| --------------------------- | -------------------------------------------------- |
| `ANTHROPIC_API_KEY`         | Anthropic                                          |
| `OPENAI_API_KEY`            | OpenAI                                             |
| `VERCEL_API_KEY`            | Vercel AI Gateway                                  |
| `GEMINI_API_KEY`            | Google Gemini                                      |
| `ZAI_API_KEY`               | Z.ai                                               |
| `MINIMAX_API_KEY`           | MiniMax                                            |
| `SYNTHETIC_API_KEY`         | Synthetic                                          |
| `HF_TOKEN`                  | Hugging Face Inference                             |
| `CEREBRAS_API_KEY`          | Cerebras                                           |
| `OPENROUTER_API_KEY`        | OpenRouter                                         |
| `IONET_API_KEY`             | io.net                                             |
| `ALIBABA_SINGAPORE_API_KEY` | Alibaba (Singapore)                                |
| `ALIBABA_US_API_KEY`        | Alibaba (United States)                            |
| `GROQ_API_KEY`              | Groq                                               |
| `AVIAN_API_KEY`             | Avian                                              |
| `OPENCODE_API_KEY`          | OpenCode Zen & Go                                  |
| `MOONSHOT_API_KEY`          | Moonshot                                           |
| `XAI_API_KEY`               | xAI                                                |
| `DEEPSEEK_API_KEY`          | DeepSeek                                           |
| `KIMI_CODING_API_KEY`       | Kimi Coding                                        |
| `AIHUBMIX_API_KEY`          | AIHubMix                                           |
| `ATLASCLOUD_API_KEY`        | Atlas Cloud                                        |
| `BASETEN_API_KEY`           | Baseten                                            |
| `CHUTES_API_KEY`            | Chutes                                             |
| `CORALBRICKS_API_KEY`       | CoralBricks                                        |
| `CORTECS_API_KEY`           | Cortecs                                            |
| `FIREWORKS_API_KEY`         | Fireworks                                          |
| `NEBIUS_API_KEY`            | Nebius Token Factory                               |
| `NEURALWATT_API_KEY`        | Neuralwatt                                         |
| `QINIUCLOUD_API_KEY`        | QiniuCloud                                         |
| `SCW_SECRET_KEY`            | Scaleway                                           |
| `VENICE_API_KEY`            | Venice AI                                          |
| `ZHIPU_API_KEY`             | Zhipu                                              |

## Local models & custom providers

zemi talks to any OpenAI-compatible server: Ollama, llama.cpp, LM Studio, vLLM, a gateway like LiteLLM. Register it in `~/.config/zemi/init.lua`:

```lua
zemi.provider.add({
	id = "local",
	kind = "openai_compat", -- or "anthropic", "openai" (OpenAI itself), "google", ...
	base_url = "http://localhost:11434/v1",
	-- api_key = zemi.env.get("MY_KEY"),   -- for a server that wants one
	models = { { id = "qwen3", context_window = 32000 } },   -- listed by hand; the id is the name your server reports
})
```

Then pick the model in `/model`. Registering under a builtin's id, say `openai`, replaces that builtin whole.
