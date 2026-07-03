"""通知模块 — 通过 Telegram Bot 推送消息"""

import logging

logger = logging.getLogger(__name__)

_bot = None


def init(bot):
    global _bot
    _bot = bot


async def send(tg_id: int, text: str) -> bool:
    """给指定用户发送通知，返回是否成功"""
    if not _bot:
        return False
    try:
        await _bot.send_message(chat_id=tg_id, text=text, parse_mode="HTML")
        return True
    except Exception as e:
        logger.warning(f"通知发送失败 ({tg_id}): {e}")
        return False


async def task_assigned(assignee_tg_id: int, title: str, goal: str, assigner_tg_id: int):
    await send(assignee_tg_id, f"📋 你收到了新任务：<b>{title}</b>\n目标：{goal}\n分配者：{assigner_tg_id}")


async def task_status_changed(assigner_tg_id: int, title: str, assignee_tg_id: int, status: str):
    await send(assigner_tg_id, f"📊 任务状态更新：<b>{title}</b>\n{assignee_tg_id} → {status}")


async def task_split(assigner_tg_id: int, title: str, splitter_tg_id: int, count: int):
    await send(assigner_tg_id, f"🔀 任务已拆分：<b>{title}</b>\n由 {splitter_tg_id} 拆分为 {count} 个子任务")


async def task_desc_changed(assignee_tg_id: int, title: str):
    await send(assignee_tg_id, f"✏️ 你的任务描述已更新：<b>{title}</b>\n请查看最新内容。")
