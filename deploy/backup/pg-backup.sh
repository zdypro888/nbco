#!/bin/sh
# nbco 数据库备份：pg_dump 自定义格式 + 按天轮换。
# 单二进制哲学下全部公司状态（用户/任务/知识/会话/审计）只在这一个库里——
# 没有它的备份等于公司没有备份。
#
# 用法： ./pg-backup.sh 'postgres://user@127.0.0.1:5432/nbco' /path/to/backups [保留天数]
# 恢复： pg_restore -d nbco --clean --if-exists <备份文件>
set -eu
DSN="${1:?用法: pg-backup.sh <DSN> <备份目录> [保留天数]}"
DIR="${2:?缺少备份目录}"
KEEP_DAYS="${3:-14}"

mkdir -p "$DIR"
STAMP="$(date +%Y%m%d-%H%M%S)"
OUT="$DIR/nbco-$STAMP.dump"

pg_dump --format=custom --compress=6 --file="$OUT.tmp" "$DSN"
mv "$OUT.tmp" "$OUT"
echo "已备份: $OUT ($(du -h "$OUT" | cut -f1))"

# 轮换：删除超过保留期的备份。
find "$DIR" -name 'nbco-*.dump' -mtime "+$KEEP_DAYS" -delete
