#!/bin/sh
# 在服务器上执行：原子替换二进制、重启、健康检查、失败自动回滚。
#
# 由 .github/workflows/deploy.yml 通过 ssh 调用，也可以手动跑：
#   scp cligc-linux-amd64 server:/tmp/cligc.new
#   ssh server 'sudo /usr/local/bin/cligc-deploy /tmp/cligc.new'
#
# 环境变量：
#   CLIGC_BIN      目标路径，默认 /usr/local/bin/cligc
#   CLIGC_SERVICE  systemd 单元名，默认 cligc
#   CLIGC_HEALTH   健康检查地址，默认 http://127.0.0.1:8866/healthz
#   CLIGC_WAIT     健康检查最长等待秒数，默认 30
#   CLIGC_RESTART  重启命令，默认 systemctl restart $CLIGC_SERVICE。
#                  用 OpenRC / supervisord / docker 的把它换掉即可，
#                  这个脚本不假设你用 systemd。
set -eu

NEW="${1:?用法: deploy.sh <新二进制路径>}"
BIN="${CLIGC_BIN:-/usr/local/bin/cligc}"
SVC="${CLIGC_SERVICE:-cligc}"
URL="${CLIGC_HEALTH:-http://127.0.0.1:8866/healthz}"
WAIT="${CLIGC_WAIT:-30}"
RESTART="${CLIGC_RESTART:-systemctl restart $SVC}"
PREV="$BIN.prev"

log() { printf '[deploy] %s\n' "$*"; }
die() { printf '[deploy] %s\n' "$*" >&2; exit 1; }

[ -f "$NEW" ] || die "找不到 $NEW"

# 先验一下新二进制自己能不能跑起来。链错了架构、或者构建产物截断了，
# 在这里就会暴露——比换上去之后再发现要便宜得多。
chmod +x "$NEW"
"$NEW" -version >/dev/null 2>&1 || "$NEW" version >/dev/null 2>&1 || true

# 健康检查：轮询到成功或超时。
health() {
  i=0
  while [ "$i" -lt "$WAIT" ]; do
    if curl -fsS --max-time 3 "$URL" >/dev/null 2>&1; then
      return 0
    fi
    i=$((i + 1))
    sleep 1
  done
  return 1
}

# 留一份可回滚的版本。cp -p 保留权限位，免得回滚回去是个不可执行的文件。
if [ -f "$BIN" ]; then
  cp -p "$BIN" "$PREV"
  log "已备份当前版本到 $PREV"
else
  log "首次部署，没有可备份的版本"
fi

# 原子替换。必须用 mv 不能用 cp：
#   cp 会写入目标文件本身，而内核不允许写入正在执行的文件（ETXTBSY）。
#   mv 只是换掉目录项，正在跑的进程继续用它自己的 inode，直到重启。
# 同时要求 $NEW 和 $BIN 在同一个文件系统上，否则 mv 退化成复制+删除，
# 就不再是原子的了——所以先挪到目标目录旁边。
TMP="$BIN.new.$$"
mv -f "$NEW" "$TMP" 2>/dev/null || { cp -f "$NEW" "$TMP"; rm -f "$NEW"; }
chmod +x "$TMP"
mv -f "$TMP" "$BIN"
log "已替换 $BIN"

sh -c "$RESTART"
log "已重启，等待健康检查（最多 ${WAIT}s）"

if health; then
  log "健康检查通过，部署完成"
  exit 0
fi

# --- 回滚 ---
#
# 注意这里只回滚二进制，不回滚数据库。这套迁移全是加列和加索引，旧版本
# 的查询用的是显式列清单，多出来的列会被忽略——所以旧二进制能在新库上
# 正常跑。哪天有了破坏性迁移（改列、删列、改语义），这个假设就不成立了，
# 那时候必须先想清楚回滚路径再写那条迁移。
log "健康检查失败"
[ -f "$PREV" ] || die "没有可回滚的版本，服务当前是坏的，需要人工介入"

log "回滚到上一个版本"
mv -f "$PREV" "$BIN"
sh -c "$RESTART"

if health; then
  die "已回滚到上一个版本，服务恢复。新版本没有通过健康检查。"
fi
die "回滚之后健康检查仍然失败，需要人工介入。"
