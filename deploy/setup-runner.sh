#!/bin/sh
# 在服务器上装一个 GitHub 自托管 Runner，让部署不需要任何 SSH 密钥。
#
# 用法（在服务器上以 root 执行）：
#   REPO=你的用户名/cligc TOKEN=从GitHub页面复制 sh setup-runner.sh
#
# TOKEN 从哪来：
#   仓库 → Settings → Actions → Runners → New self-hosted runner
#   页面上那条 ./config.sh --token AXXXX 里的 AXXXX 就是。
#   它一小时内有效，只用于注册这一次，之后不再需要。
#
# 装完之后：
#   - 服务器主动去 GitHub 拉任务，不需要对外开任何端口
#   - 仓库里一个 secret 都不用配
set -eu

REPO="${REPO:?需要 REPO，如 ccvar/cligc}"
TOKEN="${TOKEN:?需要 TOKEN，见脚本开头说明}"
RUNNER_USER="${RUNNER_USER:-cligc-runner}"
HOME_DIR="${RUNNER_HOME:-/opt/actions-runner}"
SVC="${CLIGC_SERVICE:-cligc}"
BIN="${CLIGC_BIN:-/usr/local/bin/cligc}"

say() { printf '\033[1m%s\033[0m\n' "$*"; }
die() { printf '\033[31m%s\033[0m\n' "$*" >&2; exit 1; }

[ "$(id -u)" = 0 ] || die "需要 root（Runner 要建用户、装 systemd 服务）"
command -v curl >/dev/null 2>&1 || die "缺 curl"
command -v tar  >/dev/null 2>&1 || die "缺 tar"

case "$(uname -m)" in
  x86_64|amd64) ARCH=x64 ;;
  aarch64|arm64) ARCH=arm64 ;;
  *) die "不支持的架构：$(uname -m)" ;;
esac

# --- 建一个专用用户 -------------------------------------------------------
#
# 不要用 root 跑 Runner：它执行的是仓库里的工作流代码，等于把仓库的写权限
# 直接等同于服务器的 root。专用用户 + 下面那条极窄的 sudoers 才是正确的
# 权限边界。
if ! id "$RUNNER_USER" >/dev/null 2>&1; then
  useradd --system --create-home --home-dir "$HOME_DIR" --shell /bin/sh "$RUNNER_USER"
  say "已建用户 $RUNNER_USER"
fi
install -d -o "$RUNNER_USER" -g "$RUNNER_USER" "$HOME_DIR"

# --- 下载并注册 -----------------------------------------------------------
say "下载 Runner"
VER=$(curl -fsSL https://api.github.com/repos/actions/runner/releases/latest \
      | sed -n 's/.*"tag_name": *"v\([^"]*\)".*/\1/p' | head -1)
[ -n "$VER" ] || die "取不到 Runner 版本号"
URL="https://github.com/actions/runner/releases/download/v${VER}/actions-runner-linux-${ARCH}-${VER}.tar.gz"
su -s /bin/sh "$RUNNER_USER" -c "cd '$HOME_DIR' && curl -fsSL -o runner.tar.gz '$URL' && tar xzf runner.tar.gz && rm runner.tar.gz"

say "注册到 $REPO"
su -s /bin/sh "$RUNNER_USER" -c "cd '$HOME_DIR' && ./config.sh --unattended --replace \
  --url 'https://github.com/$REPO' --token '$TOKEN' --labels self-hosted --name '$(hostname)'"

# --- 权限 -----------------------------------------------------------------
#
# Runner 只需要能做一件事：执行部署脚本。给它一条只允许这一条命令的
# sudoers，而不是 NOPASSWD: ALL —— 后者等于把仓库写权限升级成服务器 root。
install -d /usr/local/lib/cligc
SRC="$(dirname "$0")/deploy.sh"
[ -f "$SRC" ] || die "找不到 $SRC —— 请在仓库的 deploy/ 目录下执行本脚本"
install -m 755 "$SRC" /usr/local/lib/cligc/deploy.sh
cat > /etc/sudoers.d/cligc-runner <<EOF
$RUNNER_USER ALL=(root) NOPASSWD: /usr/local/lib/cligc/deploy.sh
EOF
chmod 440 /etc/sudoers.d/cligc-runner
visudo -cf /etc/sudoers.d/cligc-runner >/dev/null || die "sudoers 写错了，已中止"
say "已授予 $RUNNER_USER 执行部署脚本的权限（仅此一条）"

# --- 装成服务 -------------------------------------------------------------
( cd "$HOME_DIR" && ./svc.sh install "$RUNNER_USER" && ./svc.sh start )

cat <<EOF

$(say '装好了')

  Runner 已在后台运行，开机自启。
  仓库 → Settings → Actions → Runners 里应该能看到这台机器是 Idle。

  之后每次合并到 main，GitHub 会把任务派给这台机器：
  本地构建 → 原子替换 → 重启 → 健康检查 → 失败自动回滚。

  一个 secret 都不需要配。

EOF
