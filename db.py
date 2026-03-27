import logging
import motor.motor_asyncio

logger = logging.getLogger(__name__)

_client = None
_db = None


def init(uri: str, db_name: str):
    global _client, _db
    _client = motor.motor_asyncio.AsyncIOMotorClient(uri)
    _db = _client[db_name]
    logger.info(f"MongoDB 已连接: {db_name}")


async def ensure_indexes():
    """创建必要的索引"""
    await _db.users.create_index("tg_id", unique=True)
    await _db.tasks.create_index([("assignee_tg_id", 1), ("status", 1)])
    await _db.tasks.create_index("assigner_tg_id")
    await _db.tasks.create_index("project_id")
    await _db.projects.create_index("creator_tg_id")
    await _db.roles.create_index("name", unique=True)
    logger.info("MongoDB 索引已创建")


def get_db():
    if _db is None:
        raise RuntimeError("数据库未初始化，请先调用 db.init()")
    return _db
