"""Optional MCP stdio transport for the harness-neutral read-only tools."""
from __future__ import annotations

import asyncio
import json
from typing import Any

from .tools import ReadOnlyAssistantTools, ToolCallError


def create_server(tools: ReadOnlyAssistantTools) -> Any:
    """Create an official MCP Python SDK v2 low-level Server."""
    try:
        from mcp.server import Server
        from mcp.types import CallToolResult, ListToolsResult, TextContent, Tool, ToolAnnotations
    except ImportError as exc:
        raise RuntimeError("Install the optional MCP extra with `pip install -e '.[mcp]'`.") from exc

    advertised_tools = [
        Tool(
            name=spec["name"],
            title=spec["title"],
            description=spec["description"],
            input_schema=json.loads(json.dumps(spec["input_schema"])),
            annotations=ToolAnnotations(read_only_hint=True, open_world_hint=False),
        )
        for spec in tools.list_tools()
    ]

    async def on_list_tools(_ctx: Any, _params: Any) -> Any:
        return ListToolsResult(tools=advertised_tools)

    async def on_call_tool(_ctx: Any, params: Any) -> Any:
        try:
            result = tools.call(params.name, params.arguments or {})
            serialized = json.dumps(result, ensure_ascii=False, separators=(",", ":"))
            return CallToolResult(
                content=[TextContent(type="text", text=serialized)],
                structured_content=result,
            )
        except ToolCallError as exc:
            return CallToolResult(content=[TextContent(type="text", text=str(exc))], is_error=True)

    return Server("bb-graph-assistant", on_list_tools=on_list_tools, on_call_tool=on_call_tool)


async def _serve_stdio() -> None:
    try:
        from mcp.server.stdio import stdio_server
    except ImportError as exc:
        raise RuntimeError("Install the optional MCP extra with `pip install -e '.[mcp]'`.") from exc
    tools = ReadOnlyAssistantTools.from_data_root()
    server = create_server(tools)
    async with stdio_server() as (read_stream, write_stream):
        await server.run(read_stream, write_stream, server.create_initialization_options())


def main() -> int:
    asyncio.run(_serve_stdio())
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
