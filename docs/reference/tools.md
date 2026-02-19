# Tool Reference

Generated from runtime tool registration and tool descriptions.

## Built-In Tools

| Tool | Description |
| --- | --- |
| `append_file` | Append content to the end of a file |
| `cron` | Schedule reminders, tasks, or system commands. IMPORTANT: When user asks to be reminded or scheduled, you MUST call this tool. Use 'at_seconds' for one-time reminders (e.g., 'remind me in 10 minutes' → at_seconds=600). Use 'every_seconds' ONLY for recurring tasks (e.g., 'every 2 hours' → every_seconds=7200). Use 'cron_expr' for complex recurring schedules. Use 'command' to execute shell commands directly. |
| `edit_file` | Edit a file by replacing old_text with new_text. The old_text must exist exactly in the file. |
| `exec` | Execute a shell command and return its output. Use with caution. |
| `list_dir` | List files and directories in a path |
| `message` | Send a message to user on a chat channel. Use this when you want to communicate something. |
| `process` | Manage long-running shell processes with lifecycle control. Actions: start, list, poll, write, kill, clear. |
| `read_file` | Read the contents of a file |
| `session` | Inspect and operate on sessions. Actions: list, status, history, send, spawn. |
| `spawn` | Spawn a subagent to handle a task in the background. Use this for complex or time-consuming tasks that can run independently. The subagent will complete the task and report back when done. |
| `subagent` | Execute a subagent task synchronously and return the result. Use this for delegating specific tasks to an independent agent instance. Returns execution summary to user and full details to LLM. |
| `web_fetch` | Fetch a URL and extract readable content (HTML to text). Use this to get weather info, news, articles, or any web content. |
| `web_search` | Search the web for current information. Returns titles, URLs, and snippets from search results. |
| `write_file` | Write content to a file |

## Notes

- `agents.defaults.restrict_to_workspace=true` enables stricter shell/filesystem guards.
- In restricted mode, `exec` blocks shell control operators (`&&`, `|`, redirects), path traversal (`../`), and absolute paths outside the current working directory.
- For repo clone workflows in restricted mode, set `working_dir` to the workspace root and use relative destination paths.
