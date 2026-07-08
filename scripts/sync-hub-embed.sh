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

# ── K12 场景包出厂 seed skill（产品决策：batteries-included 零下载）──
# 从 index.json 按 tag=k12 自动选（无硬编码清单），拷 skill 正文到 scenarios/k12/skills/，
# 由 scenarios/k12/skills_bundle.go 的 go:embed 打进二进制、首启幂等 seed 到 ~/.hexclaw/skills/。
K12_SEED_DEST="$REPO_ROOT/scenarios/k12/skills"
mkdir -p "$K12_SEED_DEST"
python3 - "$HUB_DIR" "$K12_SEED_DEST" <<'PY'
import json, os, shutil, sys
hub, dest = sys.argv[1], sys.argv[2]
idx = json.load(open(os.path.join(hub, "index.json"), encoding="utf-8"))
# 清旧 seed（防已下架 skill 残留），再按 k12 tag 重铺
for f in os.listdir(dest):
    if f.endswith(".md"):
        os.remove(os.path.join(dest, f))
n = 0
for s in idx.get("skills", []):
    if "k12" in (s.get("tags") or []):
        src = os.path.join(hub, s["file"])
        if os.path.isfile(src):
            shutil.copy(src, os.path.join(dest, os.path.basename(s["file"])))
            n += 1
print(f"已 seed {n} 个 K12 skill → scenarios/k12/skills/")
PY
echo "记得提交 skill/hub/embed/*.json + scenarios/k12/skills/*.md，并确认 DefaultHubBranch 与该分支一致。"
