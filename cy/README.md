# Cy

Cy is a terminal coding agent for inspecting and editing a workspace, running
commands, and working through longer tasks in resumable sessions. It is built
on the shared `pkg/golem` and `pkg/llm` packages in this repository.

## Install

Install the latest Linux or macOS release:

```bash
curl -fsSL https://raw.githubusercontent.com/levmv/golems/main/cy/install.sh | sh
```

From a repository checkout:

```bash
go run ./cy
go install ./cy
```

## Usage

```bash
cy                                      # interactive session
cy "Review the current changes"         # one-shot
printf '%s\n' "Summarize this repo" | cy
cy -v "Run the focused tests"            # tool activity on stderr
cy --json "Inspect the workspace"        # structured result on stdout
```

New one-shot sessions are temporary unless `--save-session` is used:

```bash
cy --save-session "Investigate the flaky test"
cy resume                               # latest session in this workspace
cy resume 01234567
cy resume 01234567 "Continue the investigation"
```

Session IDs may be shortened to a unique prefix.

Use `--` when a prompt begins with `resume`:

```bash
cy -- resume the previous discussion
```

In the interactive UI, `!<command>` runs Bash directly and adds its output to
the session context:

```text
!git status --short
!go test ./cy/...
```

### Exit codes

| Code | Meaning |
| --- | --- |
| 0 | Success |
| 1 | Failure that does not fit the classes below |
| 2 | Invocation or configuration: a bad flag or setting, a missing credential, a required sandbox that is unavailable. Rerunning unchanged will fail the same way |
| 3 | The provider rejected or could not serve the request |
| 130 | Interrupted |

The class is decided by the kind of failure, not by matching its message, so
the wording of an error may change without changing what a script does.

### Models and credentials

Use `/login` and `/logout` in the interactive UI to manage credentials. Use
`/model` to choose and remember a model, or `--model` to select one for a single
invocation:

```bash
cy --model openrouter/moonshotai/kimi-k3 "Review these changes"
```

DeepSeek, OpenAI, and OpenRouter credentials may instead be supplied through
`DEEPSEEK_API_KEY`, `OPENAI_API_KEY`, and `OPENROUTER_API_KEY`. Local
Ollama models use `cy --model ollama/<model>`.

Optional web integrations use `TAVILY_API_KEY`, `EXA_API_KEY`, and
`FIRECRAWL_API_KEY`, or the corresponding `/login` commands.

## Interactive UI

Type `/` for command completion or `/help` for the full command list.

| Command | Purpose |
| --- | --- |
| `/model` | List or switch models |
| `/profile` | Switch the tool capability profile |
| `/sandbox` | Switch model command isolation |
| `/login`, `/logout` | Manage credentials |
| `/resume` | Resume another session |
| `/clear` | Start a new session |
| `/usage`, `/context` | Show token use and context budget |
| `/compact [focus]` | Compact the current context |

Use Shift+Enter, Alt+Enter, or Ctrl+J to insert a newline. Enter while Cy is
working queues the input for the next model boundary. Escape cancels active work;
Ctrl+C cancels active work or exits when idle.

Cy uses the normal terminal buffer, so scrolling, selection, and copying remain
native. Resizing the terminal or running `/clear` rebuilds the current frame
and clears its terminal scrollback.

## Configuration

Flags override environment variables, which override saved defaults.

| Flag | Environment | Purpose |
| --- | --- | --- |
| `--model` | `CY_MODEL` | Model URI in `provider/model` form |
| `--base-url` | `CY_BASE_URL` | Provider endpoint, for a self-hosted or proxied deployment |
| `--context-window` | `CY_CONTEXT_WINDOW` | Context window in tokens, for a model Cy has no entry for |
| `--system-prompt` | `CY_SYSTEM_PROMPT` | Replace the built-in system prompt |
| `--root` | `CY_ROOT` | Workspace available to tools |
| `--home` | `CY_HOME` | Credentials, sessions, and tool state |
| `--profile` | `CY_PROFILE` | `full`, `edit`, or `read-only` |
| `--sandbox` | `CY_SANDBOX` | `auto`, `off`, or `on` |
| `--theme` | `CY_THEME` | `auto`, `light`, or `dark` |

Run `cy --help` for all flags. `CY_COLOR=always|never` controls ANSI styling;
`NO_COLOR` disables it. In `auto` theme mode, Cy queries the terminal
background and falls back to the light palette when no answer is available.

`--system-prompt` replaces the built-in prompt for the current invocation.
Applicable `AGENTS.md` instructions are still loaded separately.

`--base-url` points the chosen provider at a different OpenAI-compatible
endpoint while keeping that provider's headers and authentication. Since the
endpoint is no longer the provider's, its credential stops being required; any
key that is configured is still sent, because some proxies check it. A model
served this way has no catalog entry, so `--context-window` is how Cy learns
its size — otherwise it assumes 128k, which decides when context is compacted.

## Tools and profiles

Cy exposes workspace search and file editing, managed Bash processes, public
page fetching, and optional web search.

| Profile | Available tools |
| --- | --- |
| `full` | Files, Bash/jobs, and available web tools |
| `edit` | Read/list/write files and available web tools; no Bash |
| `read-only` | Read/search/list files and available web tools; no writes or Bash |

Profiles govern model capabilities; explicit `!` commands remain available to
the user.

File tools stay beneath the workspace root and reject escapes through absolute
paths, `..`, or symlinks. Web fetching rejects credentials in URLs and
loopback, private, and link-local destinations. Fetched content is bounded and
treated as untrusted.

## Sessions and context

Interactive sessions are append-only JSONL journals under
`$CY_HOME/sessions`. They contain prompts, responses, tool calls, and tool
results verbatim; treat copied journals as secret-bearing data. Each run is
closed by a record saying how it ended — completed, failed, or interrupted —
so a session that was cut short is distinguishable from one that finished.

Cy loads the applicable `AGENTS.md` chain for the workspace. Context is
compacted automatically when needed; `/context` shows the current budget and
`/compact [focus]` forces compaction.

## Security

`CY_SANDBOX=auto` selects the available platform filesystem sandbox: Landlock
on Linux and Seatbelt on macOS. On Linux it disables the inner sandbox when Cy
confidently detects a known OS container (Docker, Podman, LXC/Incus, OpenVZ, or
systemd-nspawn). `on` requires a working platform sandbox; `off` disables it.
Use `/sandbox` to change and remember the policy during an interactive session.

When the effective policy is `off`, model-requested Bash inherits the ambient
environment, real `HOME`, and user permissions. Sandboxed processes instead
receive a separate tool home and a minimal environment without provider
credentials. `TMPDIR` points inside that tool home rather than at the shared
machine temp directory, which is neither granted nor reachable: scratch files
are not left where every other process on the host can read them, and files
left there by anyone else are not reachable from inside. Explicit `!` commands
always run outside the model sandbox with the ambient environment and may
expose their output to the model through the saved session context.

`$CY_HOME` must not be inside the workspace. The workspace is granted to tool
processes, so a home beneath it puts credentials and session journals inside
the fence. The startup probe detects this and names the granted directory
responsible; `--home` or `CY_HOME` is how to move out of it.

The intended boundary is filesystem access. Network remains open, and Cy does
not parse commands or mediate their semantic effects. The interactive startup
line reports the effective backend and network state. A non-interactive run has
no such line, so when `auto` ends up without a sandbox — because the probe did
not come back clean, or because a trusted container was detected — it says so on
stderr; stdout, including `--json`, is unaffected. A run that must not proceed
unsandboxed should use `on`, which fails instead.

## Development

The root `cy` package owns CLI wiring and session lifecycle:

- `internal/engine` — model/tool execution and context management;
- `internal/session` — journals and replay;
- `internal/state` — credentials and saved settings;
- `internal/tools` — workspace, process, web, and sandbox tools;
- `internal/ui` — one-shot output and the interactive terminal UI.
