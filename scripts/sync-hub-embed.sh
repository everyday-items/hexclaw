#!/usr/bin/env bash
# 同步内嵌市场快照（skill/hub/embed/）到 pinned 的 hexclaw-hub 分支内容。
#
# 离线优先 catalog 的「出厂种子」层就是这两个文件——发版时务必刷新，
# 让内嵌快照与本次发布的 DefaultHubBranch 契约一致。
#
# 用法：
#   scripts/sync-hub-embed.sh [hexclaw-hub 仓库路径]
# 默认从相邻目录 ../hexclaw-hub 拷贝（与本仓同级 checkout）。
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
HUB_DIR="${1:-$REPO_ROOT/../hexclaw-hub}"
DEST="$REPO_ROOT/skill/hub/embed"

if [[ ! -f "$HUB_DIR/index.json" || ! -f "$HUB_DIR/mcp-registry.json" ]]; then
  echo "找不到 hexclaw-hub 的 index.json / mcp-registry.json：$HUB_DIR" >&2
  echo "请传入 hexclaw-hub 仓库路径：scripts/sync-hub-embed.sh /path/to/hexclaw-hub" >&2
  exit 1
fi

cp "$HUB_DIR/index.json" "$DEST/index.json"
cp "$HUB_DIR/mcp-registry.json" "$DEST/mcp-registry.json"
echo "已同步内嵌市场快照 ← $HUB_DIR"
echo "记得提交 skill/hub/embed/*.json，并确认 DefaultHubBranch 与该分支一致。"
