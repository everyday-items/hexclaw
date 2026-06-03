package render

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"sort"
)

// AssetsHash 计算 renderer_assets_hash —— P0 真正可变资产的合成哈希。
//
// 任一资产改动（升级 default template / 改 CSS / 换字体 / 加 lua filter）
// 都让缓存自动失效，避免错误复用旧产物。
//
// P0 资产清单：
//   - pandoc default template（无则空）
//   - html default CSS（无则空）
//   - default reference.docx（本期内置一份用于字体/页边距基础保真）
//   - bundled fonts（noto-cjk-sc 子集等）
//   - lua filters（本期无则空，留扩展位）
//
// 用法：sidecar 启动时计算一次缓存到 Service.assetsHash 字段，运行期不变；
// release 升级二进制 / 资产时 sidecar 重启 → hash 自动重算 → 旧缓存自动失效。
func AssetsHash(paths []string) (string, error) {
	// 排序使 hash 与传入顺序无关
	sorted := append([]string(nil), paths...)
	sort.Strings(sorted)

	h := sha256.New()
	for _, p := range sorted {
		if p == "" {
			continue
		}
		// 把 path 本身也写进 hash（路径变了视为资产变了）
		fmt.Fprintf(h, "PATH:%s\n", p)

		data, err := os.ReadFile(p)
		if err != nil {
			if os.IsNotExist(err) {
				// 不存在的资产视为零字节（等价于"无此资产"）
				fmt.Fprintf(h, "MISSING\n")
				continue
			}
			return "", fmt.Errorf("read asset %q: %w", p, err)
		}
		fmt.Fprintf(h, "SIZE:%d\n", len(data))
		h.Write(data)
	}

	return hex.EncodeToString(h.Sum(nil)), nil
}
