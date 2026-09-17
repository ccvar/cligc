#!/bin/sh
# cligc 一键安装。
#
# 用法：
#   curl -fsSL https://raw.githubusercontent.com/ccvar/cligc/main/install.sh | sh
#
# 或者先看一遍再跑（管道跑网上下来的脚本前，看一眼是个好习惯）：
#   curl -fsSL https://raw.githubusercontent.com/ccvar/cligc/main/install.sh -o install.sh
#   less install.sh && sh install.sh
#
# 环境变量：
#   CLIGC_DIR    安装目录，默认 ./cligc
#   CLIGC_PORT   端口，默认 8866
#   CLIGC_REPO   源码仓库，默认 https://github.com/ccvar/cligc
#
# 用 POSIX sh 而不是 bash：macOS 自带的 bash 停在 3.2，很多 bash 写法在那上面
# 会静默行为不同。sh 的子集虽然笨，但到处都一样。
set -eu

DIR="${CLIGC_DIR:-cligc}"
PORT="${CLIGC_PORT:-8866}"
REPO="${CLIGC_REPO:-https://github.com/ccvar/cligc}"

say()  { printf '\033[1m%s\033[0m\n' "$*"; }
info() { printf '  %s\n' "$*"; }
die()  { printf '\033[31m%s\033[0m\n' "$*" >&2; exit 1; }

# --- 环境检查 -------------------------------------------------------------
#
# 先把话说全再退出。一次只报一个缺失，用户就得来回跑三趟。
missing=''
command -v git >/dev/null 2>&1 || missing="$missing git"
command -v go  >/dev/null 2>&1 || missing="$missing go"
[ -z "$missing" ] || die "缺少：$missing
安装后重试。macOS: brew install$missing
Debian/Ubuntu: sudo apt install -y$missing"

# Go 版本。用 go.mod 里声明的下限比写死一个数字好——升级 go.mod 时
# 这里不会忘了跟着改。
need=$(sed -n 's/^go \([0-9.]*\).*/\1/p' go.mod 2>/dev/null || true)
have=$(go env GOVERSION 2>/dev/null | sed 's/^go//')
if [ -n "$need" ] && [ -n "$have" ]; then
  # 版本比较交给 sort -V，自己写字符串比较会在 1.10 vs 1.9 上出错
  low=$(printf '%s\n%s\n' "$need" "$have" | sort -V | head -1)
  [ "$low" = "$need" ] || die "Go 版本过低：当前 $have，需要 $need 以上"
fi

# --- 取源码 ---------------------------------------------------------------
if [ -d "$DIR/.git" ]; then
  say "更新已有的 $DIR"
  git -C "$DIR" pull --ff-only
elif [ -f go.mod ] && [ -d cmd/cligc ]; then
  # 在源码树里直接跑脚本：就地构建，不再 clone 一份
  say "在当前源码树里构建"
  DIR=.
else
  say "拉取源码到 $DIR"
  git clone --depth 1 "$REPO" "$DIR"
fi

# --- 构建 -----------------------------------------------------------------
say "构建"
( cd "$DIR" && CGO_ENABLED=0 go build -trimpath -o cligc ./cmd/cligc )
info "$(cd "$DIR" && pwd)/cligc"

# 没有 cgo：modernc.org/sqlite 是纯 Go 实现，产物是静态二进制，
# 可以直接扔到任何同架构的机器上跑，不用装运行时依赖。

# --- 建管理员 -------------------------------------------------------------
#
# 所有交互输入都显式走 /dev/tty，不走 stdin。
#
# 因为最常见的安装方式是 `curl … | sh`，那时脚本自己的 stdin 就是那条管道：
# read 会立刻读到 EOF，看起来像"用户什么都没输"。这不是假想的问题——
# 第一版就是这么写的，实测直接报"邮箱不能为空"然后装不下去。
#
# 密码也不由脚本收集再用 -password 传给 cligc：命令行参数会出现在 ps 的
# 输出里，同一台机器上任何一个用户都看得到。cligc user add 自己会去
# /dev/tty 问，输入不回显。
DB="$DIR/data/cligc.db"
if [ -f "$DB" ]; then
  say "已有数据库，跳过建账号"
  info "$DB"
else
  # /dev/tty 存在不代表读得到：容器和某些 CI 里它可打开、但一读就是 EOF。
  # 所以不靠 [ -r /dev/tty ] 判断，直接试着读一次，读不到就转为给指引——
  # 装到一半因为"没人回答提问"而失败，是最糟的一种失败。
  say "创建管理员账号"
  EMAIL=''
  if [ -w /dev/tty ]; then
    printf '  邮箱（直接回车跳过建号）: ' > /dev/tty 2>/dev/null || true
    read -r EMAIL < /dev/tty 2>/dev/null || EMAIL=''
  fi
  if [ -n "$EMAIL" ]; then
    printf '  显示名: ' > /dev/tty
    read -r NAME < /dev/tty 2>/dev/null || NAME=''
    [ -n "$NAME" ] || NAME="$EMAIL"
    ( cd "$DIR" && ./cligc user add -db data/cligc.db -email "$EMAIL" -name "$NAME" -admin )
  else
    say "跳过建账号"
    info "需要时运行： cd $DIR && ./cligc user add -email you@example.com -name 你的名字 -admin"
  fi
fi

# --- 收尾 -----------------------------------------------------------------
# 已经在源码树里时不必让人再 cd 一次
cdline=""
[ "$DIR" = "." ] || cdline="    cd $DIR
"

cat <<EOF

$(say '装好了')

  启动：
$cdline    ./cligc serve -addr :$PORT -base-url http://localhost:$PORT -title "我的站"

  后台：  http://localhost:$PORT/admin
  接 AI： 后台 → API Token → 创建，然后「下载技能包」

  部署到服务器：deploy/ 目录下有 systemd、Caddy、Litestream 的现成配置。

EOF
