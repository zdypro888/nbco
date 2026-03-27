"""HTTP API Server — 对外暴露系统能力

启动方式：python mcp_server.py
认证：Header Authorization: Bearer <api_token> 或 ?token=<api_token>

GET /tools — 列出可用 tools
POST /call — 调用 tool {"tool": "user__get_my_profile", "arguments": {}}

复用 tools.py 的 @tool 定义，不重复写业务逻辑。
"""

import json
import logging
from aiohttp import web

import db
import auth
import api_token
import role
import fields
from tools import create_user_tools, create_admin_tools

logging.basicConfig(
    format="%(asctime)s - %(name)s - %(levelname)s - %(message)s",
    level=logging.INFO,
)
logger = logging.getLogger(__name__)

with open("config.json") as f:
    config = json.load(f)

db.init(config["mongodb_uri"], config["mongodb_db"])
auth.init(config["superadmins"])


def _get_token(request: web.Request) -> str | None:
    auth_header = request.headers.get("Authorization", "")
    if auth_header.startswith("Bearer "):
        return auth_header[7:]
    return request.query.get("token")


async def _get_user_tools(tg_id: int, is_superadmin: bool, has_admin: bool) -> dict:
    """返回 {tool_name: SdkMcpTool} 映射"""
    handlers = {}
    _, user_tool_list = create_user_tools(tg_id, is_superadmin)
    for t in user_tool_list:
        handlers[f"user__{t.name}"] = t

    if has_admin:
        _, admin_tool_list = create_admin_tools(tg_id, is_superadmin)
        for t in admin_tool_list:
            handlers[f"admin__{t.name}"] = t

    return handlers


async def handle_list(request: web.Request):
    token = _get_token(request)
    if not token:
        return web.json_response({"error": "missing token"}, status=401)

    tg_id = await api_token.verify(token)
    if tg_id is None:
        return web.json_response({"error": "invalid token"}, status=401)

    user = await auth.get_user(tg_id)
    if user is None:
        return web.json_response({"error": "user not found"}, status=401)

    tools = await _get_user_tools(tg_id, user.is_superadmin, user.has_admin)
    result = [
        {"name": name, "description": t.description or "", "input_schema": t.input_schema}
        for name, t in tools.items()
    ]
    return web.json_response({"tools": result})


async def handle_call(request: web.Request):
    token = _get_token(request)
    if not token:
        return web.json_response({"error": "missing token"}, status=401)

    tg_id = await api_token.verify(token)
    if tg_id is None:
        return web.json_response({"error": "invalid token"}, status=401)

    user = await auth.get_user(tg_id)
    if user is None:
        return web.json_response({"error": "user not found"}, status=401)

    try:
        body = await request.json()
    except Exception:
        return web.json_response({"error": "invalid json"}, status=400)

    tool_name = body.get("tool")
    arguments = body.get("arguments", {})
    if not tool_name:
        return web.json_response({"error": "missing 'tool' field"}, status=400)

    tools = await _get_user_tools(tg_id, user.is_superadmin, user.has_admin)
    t = tools.get(tool_name)
    if not t:
        return web.json_response({"error": f"tool not found: {tool_name}"}, status=404)

    result = await t.handler(arguments)
    return web.json_response(result)


app = web.Application()
app.router.add_get("/tools", handle_list)
app.router.add_post("/call", handle_call)


async def on_startup(app_):
    await db.ensure_indexes()
    await auth._load_superadmins()
    await role.load()
    await fields.load()
    logger.info("HTTP API Server 启动完成")


app.on_startup.append(on_startup)


if __name__ == "__main__":
    port = config.get("mcp_port", 8900)
    logger.info(f"HTTP API Server 启动中... port={port}")
    web.run_app(app, port=port)
