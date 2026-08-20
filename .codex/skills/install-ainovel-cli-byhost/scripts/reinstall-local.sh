#!/bin/sh
set -eu

TARGET="/usr/local/bin/ainovel-cli"
TARGET_DIR="/usr/local/bin"
EXPECTED_MODULE="github.com/voocel/ainovel-cli"
MODE="${1:-install}"

case "$MODE" in
	install|--check) ;;
	*)
		printf '用法：%s [--check]\n' "$0" >&2
		exit 2
		;;
esac

fail() {
	printf '错误：%s\n' "$*" >&2
	exit 1
}

for required_command in go git awk sed shasum uname; do
	command -v "$required_command" >/dev/null 2>&1 || fail "缺少必需命令：$required_command"
done

[ "$(uname -s)" = "Darwin" ] || fail "此 host 安装流程仅支持 macOS"
command -v osascript >/dev/null 2>&1 || fail "缺少 macOS 管理员授权工具 osascript"
command -v codex >/dev/null 2>&1 || fail "未找到 Codex CLI；请先安装并执行 codex login"

SCRIPT_DIR=$(CDPATH= cd "$(dirname "$0")" && pwd)
PROJECT_ROOT=$(CDPATH= cd "$SCRIPT_DIR/../../../.." && pwd)
cd "$PROJECT_ROOT"

MODULE=$(go list -m -f '{{.Path}}' 2>/dev/null) || fail "无法读取 Go module；请从 ainovel-cli 仓库运行"
[ "$MODULE" = "$EXPECTED_MODULE" ] || fail "当前仓库 module 为 $MODULE，不是 $EXPECTED_MODULE"
[ -d cmd/ainovel-cli ] || fail "缺少 cmd/ainovel-cli"
[ -d internal/codexcli ] || fail "缺少 local Codex CLI backend"

CURRENT_COMMAND=$(command -v ainovel-cli 2>/dev/null || true)
if [ -n "$CURRENT_COMMAND" ] && [ "$CURRENT_COMMAND" != "$TARGET" ]; then
	fail "PATH 中的 ainovel-cli 位于 $CURRENT_COMMAND；为避免误删，拒绝继续"
fi

printf '仓库：%s\n' "$PROJECT_ROOT"
printf '目标：%s\n' "$TARGET"
printf 'Codex：'
codex --version

if [ "$MODE" = "--check" ]; then
	if [ -e "$TARGET" ] || [ -L "$TARGET" ]; then
		printf '当前安装：'
		"$TARGET" --version 2>/dev/null | sed -n '1p' || printf '%s\n' '版本未知'
	else
		printf '%s\n' '当前安装：未安装'
	fi
	printf '%s\n' '只读检查通过；未卸载、构建或安装任何文件。'
	exit 0
fi

if [ -e "$TARGET" ] || [ -L "$TARGET" ]; then
	printf '正在卸载旧版：'
	"$TARGET" --version 2>/dev/null | sed -n '1p' || printf '%s\n' '版本未知'
	if [ -w "$TARGET_DIR" ]; then
		rm -- "$TARGET"
	else
		printf '%s\n' 'macOS 将请求管理员授权以删除旧版。'
		osascript -e 'do shell script "/bin/rm /usr/local/bin/ainovel-cli" with administrator privileges'
	fi
fi

[ ! -e "$TARGET" ] && [ ! -L "$TARGET" ] || fail "旧版仍存在于 $TARGET"
hash -r 2>/dev/null || true
REMAINING_COMMAND=$(command -v ainovel-cli 2>/dev/null || true)
[ -z "$REMAINING_COMMAND" ] || fail "卸载后仍解析到 $REMAINING_COMMAND"
printf '%s\n' '旧版已卸载。'

printf '%s\n' '运行完整测试...'
go test ./...

printf '%s\n' '运行 Codex CLI 真实预检（不发起模型请求、不消耗额度）...'
AINOVEL_CODEX_PREFLIGHT=1 go test ./internal/codexcli \
	-run '^TestRuntimeRealPreflightOptIn$' -count=1 -v

COMMIT=$(git rev-parse --short=12 HEAD)
VERSION="0.0.0-local.$COMMIT"
if [ -n "$(git status --porcelain)" ]; then
	VERSION="$VERSION.dirty"
fi
BUILD_DATE=$(date -u '+%Y-%m-%dT%H:%M:%SZ')
BUILD_DIR=$(mktemp -d /tmp/ainovel-cli-build.XXXXXX)
BUILD_BINARY="$BUILD_DIR/ainovel-cli"

cleanup() {
	rm -f "$BUILD_BINARY"
	rmdir "$BUILD_DIR" 2>/dev/null || true
}
trap cleanup EXIT HUP INT TERM

printf '从当前工作区构建 v%s...\n' "$VERSION"
go build -trimpath \
	-ldflags "-s -w -X main.version=$VERSION -X main.commit=$COMMIT -X main.date=$BUILD_DATE" \
	-o "$BUILD_BINARY" ./cmd/ainovel-cli

[ -s "$BUILD_BINARY" ] || fail "构建产物为空"
"$BUILD_BINARY" --version
BUILD_SHA=$(shasum -a 256 "$BUILD_BINARY" | awk '{print $1}')

if [ -w "$TARGET_DIR" ]; then
	/usr/bin/install -m 0755 "$BUILD_BINARY" "$TARGET"
else
	printf '%s\n' 'macOS 将请求管理员授权以安装本地构建。'
	osascript -e "do shell script \"/usr/bin/install -m 0755 $BUILD_BINARY $TARGET\" with administrator privileges"
fi

[ -x "$TARGET" ] || fail "安装后目标不可执行：$TARGET"
hash -r 2>/dev/null || true
INSTALLED_COMMAND=$(command -v ainovel-cli 2>/dev/null || true)
[ "$INSTALLED_COMMAND" = "$TARGET" ] || fail "安装后 PATH 解析为 ${INSTALLED_COMMAND:-空}，预期 $TARGET"

INSTALLED_SHA=$(shasum -a 256 "$TARGET" | awk '{print $1}')
[ "$INSTALLED_SHA" = "$BUILD_SHA" ] || fail "安装产物 SHA-256 与本地构建不一致"

printf '%s\n' '安装成功：'
"$TARGET" --version
printf 'path: %s\nsha256: %s\n' "$TARGET" "$INSTALLED_SHA"
printf '%s\n' '用户配置 ~/.ainovel、作品数据和 Codex 登录配置均未修改。'
