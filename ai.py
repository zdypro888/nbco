import logging
from claude_agent_sdk import query, ClaudeAgentOptions, ResultMessage, AssistantMessage, TextBlock
from tools import create_user_tools, create_telegram_tools, create_admin_tools
import auth
import role
from db import get_db

logger = logging.getLogger(__name__)

_bot = None

_BASE_PROMPT = """你是 TGCompany 内部管理助手。用中文简洁回复。
不要透露技术实现细节。直接使用可用的 tool，不要自行判断权限。
回复仅支持 Telegram HTML：<b> <i> <u> <s> <code> <pre> <a href=""> <blockquote>。
禁止使用 <h1>-<h6>、<ul>、<li>、<br>、<p> 等标签。用换行和符号代替列表。
"""

_PROMPT_INFO_FLOW = """编辑画像必须遵循：先查已有→归纳去重→展示确认→确认后保存。"""

_PROMPT_TASK_FLOW = """汇报进度时：查任务详情→更新清单→记录进度→全部完成建议 done。拆分任务先展示方案再执行。"""

_PROMPT_SUPERADMIN = """你是超级管理员，所有权限自动通过，直接操作即可。"""

_PROMPT_PROJECT = """创建项目时：先了解团队画像→规划任务→确认后执行。"""

_PROMPT_TELEGRAM = """正常回复直接输出文字。telegram tool 仅用于主动推送等特殊场景。当前 chat_id={chat_id}。"""


async def init(bot):
    global _bot
    _bot = bot
    await role.load()


def _build_prompt(user, has_admin: bool, chat_id: int) -> str:
    parts = [_BASE_PROMPT, _PROMPT_INFO_FLOW, _PROMPT_TASK_FLOW]

    if user and user.is_superadmin:
        parts.append(_PROMPT_SUPERADMIN)
        parts.append(_PROMPT_PROJECT)
    elif has_admin:
        parts.append(_PROMPT_PROJECT)

    parts.append(_PROMPT_TELEGRAM.format(chat_id=chat_id))

    summary = role.get_summary()
    if summary:
        parts.append(f"可用角色（用 activate_role 激活）：\n{summary}")

    return "\n".join(parts)


async def _get_session(tg_id: int) -> str | None:
    doc = await get_db().users.find_one({"tg_id": tg_id}, {"session_id": 1})
    return doc.get("session_id") if doc else None


async def _set_session(tg_id: int, session_id: str):
    await get_db().users.update_one({"tg_id": tg_id}, {"$set": {"session_id": session_id}})


async def chat(user_id: int, text: str, chat_id: int = 0) -> str:
    session_id = await _get_session(user_id)
    user = await auth.get_user(user_id)
    is_superadmin = user.is_superadmin if user else False

    user_tools, _ = create_user_tools(user_id, is_superadmin)
    tg_tools = create_telegram_tools(_bot, user_id, is_superadmin, chat_id)
    mcp_servers = {"user": user_tools, "telegram": tg_tools}
    allowed = ["mcp__user__*", "mcp__telegram__*"]

    has_admin = user.has_admin if user else False
    if has_admin:
        admin_tools, _ = create_admin_tools(user_id, is_superadmin)
        mcp_servers["admin"] = admin_tools
        allowed.append("mcp__admin__*")

    options = ClaudeAgentOptions(
        system_prompt=_build_prompt(user, has_admin, chat_id),
        allowed_tools=allowed,
        disallowed_tools=["CronCreate", "CronDelete", "CronList"],
        mcp_servers=mcp_servers,
        max_turns=10,
        permission_mode="bypassPermissions",
    )
    if session_id:
        options.resume = session_id

    result_text = ""
    async for message in query(prompt=text, options=options):
        if isinstance(message, ResultMessage):
            await _set_session(user_id, message.session_id)
            if message.result:
                result_text = message.result
        elif isinstance(message, AssistantMessage):
            for block in message.content:
                if isinstance(block, TextBlock):
                    result_text = block.text

    if not result_text:
        return "抱歉，AI 暂时繁忙，请稍后再试。"
    return result_text


async def new_chat(user_id: int):
    await get_db().users.update_one({"tg_id": user_id}, {"$unset": {"session_id": 1}})
