# MCP declaration translation

MCP servers are declared once, in the project configuration
(`[mcp.<name>]` with `command`, `args` and `scope`). The
`internal/provision` package translates the declarations that apply to
one agent into that agent's native configuration.

## Scopes

| Scope | Applies to |
| --- | --- |
| `shared` | every agent whose kind has a translation |
| `agent:<name>` | the agent with that configured name |
| `role:<name>` | an agent playing that role; an agent without a role matches no `role:` scope |

`provision.Applicable(servers, agent, role)` returns the names of the
applicable servers, sorted. Any other scope is refused with
`provision.ErrInvalidMCP`.

## Native formats

`provision.TranslateMCP(servers, agent, kind, role)` returns an
`MCPConfig` holding the kind, the translated server names and the
rendered content. Only the applicable declarations are checked and
rendered: one without a command or holding invalid UTF-8 is refused with
`ErrInvalidMCP`.

| Kind | Output |
| --- | --- |
| `claude-code` | the project file `.mcp.json` (`MCPConfig.File`) |
| `codex` | a fragment of Codex's configuration; `MCPConfig.File` is empty |

For `claude-code`, the content is indented JSON with a trailing newline;
`args` is always an array and characters such as `<`, `>` and `&` are
kept verbatim:

```json
{
  "mcpServers": {
    "github": {
      "command": "github-mcp",
      "args": [
        "stdio"
      ]
    }
  }
}
```

For `codex`, the content is one `[mcp_servers.<name>]` table per server,
separated by a blank line. Maestro renders it itself: values are TOML
basic strings with `"`, `\`, the usual control escapes and other control
characters as `\uXXXX`; a server name that is not a bare TOML key is
quoted.

```toml
[mcp_servers.github]
command = "github-mcp"
args = ["stdio"]
```

Servers are always rendered in name order, so the same declarations give
byte-identical output. No environment value is translated: the
configuration already refuses secrets, and declarations carry none.

Any other kind is refused with `provision.ErrUnknownKind`.
