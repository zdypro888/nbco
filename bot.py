import json
import logging
import db
import auth
import ai
import key
import notify
from telegram import BotCommand, Update
from telegram.ext import Application, CommandHandler, MessageHandler, filters, ContextTypes

logging.basicConfig(
    format="%(asctime)s - %(name)s - %(levelname)s - %(message)s",
    level=logging.INFO,
)
logger = logging.getLogger(__name__)

with open("config.json") as f:
    config = json.load(f)

db.init(config["mongodb_uri"], config["mongodb_db"])
auth.init(config["superadmins"])


async def start(update: Update, context: ContextTypes.DEFAULT_TYPE):
    await update.message.reply_text("你好！我是 TGCompany 管理 Bot，有什么可以帮你的？")


async def help_cmd(update: Update, context: ContextTypes.DEFAULT_TYPE):
    text = (
        "<b>TGCompany 管理 Bot</b>\n\n"
        "直接发消息即可与 AI 助手对话，支持：\n"
        "• 查看/修改个人信息\n"
        "• 管理自我介绍和画像\n"
        "• 查看/更新任务、拆分下发\n"
        "• 记录工作进度\n"
        "• 管理权限（如有）\n"
        "• 项目规划（如有）\n\n"
        "<b>命令：</b>\n"
        "/new - 开启新对话\n"
        "/join &lt;key&gt; - 输入 Key 加入系统\n"
        "/help - 查看帮助"
    )
    await update.message.reply_text(text, parse_mode="HTML")


async def new(update: Update, context: ContextTypes.DEFAULT_TYPE):
    await ai.new_chat(update.effective_user.id)
    await update.message.reply_text("已开启新对话。")


async def join(update: Update, context: ContextTypes.DEFAULT_TYPE):
    user = update.effective_user

    if await auth.is_authorized(user.id):
        await update.message.reply_text("你已经是系统用户了，无需再次绑定。")
        return

    if not context.args:
        await update.message.reply_text("请输入绑定 Key：/join <key>")
        return

    code = context.args[0]
    if await key.consume(code, user.id):
        await update.message.reply_text(f"绑定成功！欢迎 {user.full_name}。")
    else:
        await update.message.reply_text("Key 无效或已被使用。")


async def handle_message(update: Update, context: ContextTypes.DEFAULT_TYPE):
    # 只在私聊中工作
    if update.effective_chat.type != "private":
        return

    user = update.effective_user
    text = update.message.text
    logger.info(f"收到消息 from {user.id} ({user.full_name}): {text}")

    cached_user = await auth.get_user(user.id)
    if not cached_user:
        await update.message.reply_text(
            f"您好 {user.full_name}，您的ID为 {user.id}，暂未获得使用权限。"
        )
        return

    # 超管首次使用检测
    if cached_user.is_superadmin and not cached_user.has_profile:
        text = "系统检测到你是超级管理员但尚未录入个人信息，请先录入。\n用户原始消息：" + text

    await update.message.chat.send_action("typing")

    for attempt in range(2):
        try:
            reply = await ai.chat(user.id, text, chat_id=update.effective_chat.id)
            try:
                await update.message.reply_text(reply, parse_mode="HTML")
            except Exception:
                await update.message.reply_text(reply)
            return
        except Exception as e:
            if attempt == 0:
                logger.warning(f"Claude 调用失败，重试: {e}")
                continue
            logger.error(f"Claude 调用失败: {e}")
            await update.message.reply_text("系统暂时出错，请稍后再试。")


def main():
    app = Application.builder().token(config["bot_token"]).build()
    app.add_handler(CommandHandler("start", start))
    app.add_handler(CommandHandler("help", help_cmd))
    app.add_handler(CommandHandler("new", new))
    app.add_handler(CommandHandler("join", join))
    app.add_handler(MessageHandler(filters.TEXT & ~filters.COMMAND, handle_message))

    async def post_init(application):
        await db.ensure_indexes()
        await auth._load_superadmins()
        notify.init(application.bot)
        await ai.init(application.bot)
        await application.bot.set_my_commands([
            BotCommand("start", "开始使用"),
            BotCommand("help", "查看帮助"),
            BotCommand("new", "开启新对话"),
            BotCommand("join", "输入 Key 加入系统"),
        ])

    app.post_init = post_init
    logger.info("Bot 启动中...")
    app.run_polling()


if __name__ == "__main__":
    main()
