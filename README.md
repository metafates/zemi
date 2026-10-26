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
Install github.com/metafates/zemi (go install github.com/metafates/zemi@latest), ask which model provider I use and put its API key in my shell config, then print the one command I should run.
```

## Features

- Keeps prompts small, cache hit rates high
- Knows when to shut up
- Does more with less steps
- Subagents, workflows, background jobs, lsp, mcp, and more.
- Fully scriptable with Lua and knows how to modify itself
- Learns from previous sessions
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
| `VERTEXAI_PROJECT`          | Google Cloud VertexAI (Gemini)                     |
| `VERTEXAI_LOCATION`         | Google Cloud VertexAI (Gemini)                     |
| `AWS_ACCESS_KEY_ID`         | Amazon Bedrock (Claude)                            |
| `AWS_SECRET_ACCESS_KEY`     | Amazon Bedrock (Claude)                            |
| `AWS_REGION`                | Amazon Bedrock (Claude)                            |
| `AWS_PROFILE`               | Amazon Bedrock (Custom Profile)                    |
| `AWS_BEARER_TOKEN_BEDROCK`  | Amazon Bedrock                                     |
| `AZURE_OPENAI_API_ENDPOINT` | Azure OpenAI models                                |
| `AZURE_OPENAI_API_KEY`      | Azure OpenAI models (optional when using Entra ID) |
| `AZURE_OPENAI_API_VERSION`  | Azure OpenAI models                                |
| `MOONSHOT_API_KEY`          | Moonshot                                           |
| `XAI_API_KEY`               | xAI                                                |

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
