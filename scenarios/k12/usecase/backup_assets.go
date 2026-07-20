package usecase

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"

	"github.com/hexagon-codes/hexclaw/records"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/assetstore"
)

var ErrHexbakAssetManifest = errors.New("hexbak asset manifest invalid")

// HexbakAsset 是 v3 归档中的内容文件条目。AssetID/OwnerAgent/SHA256/MIME
// 都是受 checksum 保护的显式 manifest；Data 由 encoding/json 以 base64 编码。
type HexbakAsset struct {
	AssetID    string `json:"asset_id"`
	OwnerAgent string `json:"owner_agent"`
	SHA256     string `json:"sha256"`
	MIME       string `json:"mime"`
	Data       []byte `json:"data"`
}

// PackHexbakAssets 读取 canonical records 中所有精确 asset:// 引用，逐字节验证
// 内容寻址身份并按 asset_id 排序、去重打包。引用存在但文件缺失/损坏时整份备份失败。
func PackHexbakAssets(agent string, recs []*records.AgentRecord) ([]HexbakAsset, error) {
	refs, err := ReferencedHexbakAssetIDs(recs)
	if err != nil {
		return nil, err
	}
	return packHexbakAssetIDs(agent, refs)
}

func packHexbakAssetIDs(agent string, refs []string) ([]HexbakAsset, error) {
	out := make([]HexbakAsset, 0, len(refs))
	for _, id := range refs {
		owner, file, err := assetstore.Parse(id)
		if err != nil || owner != agent {
			return nil, fmt.Errorf("%w: asset %q owner 不属于 archive agent %q", ErrHexbakAssetManifest, id, agent)
		}
		data, storedMIME, err := assetstore.Read(owner, file)
		if err != nil {
			return nil, fmt.Errorf("%w: 读取 %q: %v", ErrHexbakAssetManifest, id, err)
		}
		computedID, mime, digest, err := assetstore.Describe(owner, data)
		if err != nil || computedID != id || mime != storedMIME {
			return nil, fmt.Errorf("%w: asset %q 内容寻址身份不一致", ErrHexbakAssetManifest, id)
		}
		out = append(out, HexbakAsset{
			AssetID: id, OwnerAgent: owner, SHA256: digest, MIME: mime,
			Data: append([]byte(nil), data...),
		})
	}
	return out, nil
}

// MergeHexbakAssets combines independently discovered canonical references into
// one deterministic exact-set manifest. The same content ID may be referenced by
// records and V19 Problems, but its bytes are packed once.
func MergeHexbakAssets(groups ...[]HexbakAsset) ([]HexbakAsset, error) {
	byID := make(map[string]HexbakAsset)
	for _, group := range groups {
		for _, item := range group {
			if prior, ok := byID[item.AssetID]; ok {
				if prior.OwnerAgent != item.OwnerAgent || prior.SHA256 != item.SHA256 ||
					prior.MIME != item.MIME || !bytes.Equal(prior.Data, item.Data) {
					return nil, fmt.Errorf("%w: duplicate asset %q 内容不一致", ErrHexbakAssetManifest, item.AssetID)
				}
				continue
			}
			item.Data = append([]byte(nil), item.Data...)
			byID[item.AssetID] = item
		}
	}
	out := make([]HexbakAsset, 0, len(byID))
	for _, item := range byID {
		out = append(out, item)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].AssetID < out[j].AssetID })
	return out, nil
}

// ReferencedHexbakAssetIDs 枚举所有 record Fields JSON 内作为完整字符串值出现的
// asset:// 引用。递归遍历使 CreativeWork versions/feedback evidence 与
// PracticeSet return_assets 以及后续新增的嵌套字段共用同一归属门。
func ReferencedHexbakAssetIDs(recs []*records.AgentRecord) ([]string, error) {
	seen := map[string]struct{}{}
	for _, rec := range recs {
		if rec == nil {
			continue
		}
		value, err := decodeJSONValue(rec.Fields)
		if err != nil {
			return nil, fmt.Errorf("%w: record %q fields JSON: %v", ErrHexbakAssetManifest, rec.RecordID, err)
		}
		if err := walkJSONStrings(value, func(raw string) (string, error) {
			if !assetstore.IsAssetID(raw) {
				return raw, nil
			}
			if _, _, err := assetstore.Parse(raw); err != nil {
				return "", fmt.Errorf("%w: record %q 包含非法 asset id: %v", ErrHexbakAssetManifest, rec.RecordID, err)
			}
			seen[raw] = struct{}{}
			return raw, nil
		}); err != nil {
			return nil, err
		}
	}
	refs := make([]string, 0, len(seen))
	for id := range seen {
		refs = append(refs, id)
	}
	sort.Strings(refs)
	return refs, nil
}

// ValidateHexbakAssets 验证 manifest 与 records 引用 exact-set、owner、MIME、SHA
// 及内容寻址 ID。v1/v2 不允许携带未签名的 Assets 字段；其历史 asset 引用只可在
// 原 Tutor 恢复中沿用，restore-as 会另行 fail-closed。
func ValidateHexbakAssets(bak *Hexbak) error {
	if bak == nil {
		return fmt.Errorf("%w: nil archive", ErrHexbakAssetManifest)
	}
	if bak.Version < 3 {
		if len(bak.Assets) != 0 {
			return fmt.Errorf("%w: v%d assets 未受该版本 checksum 保护", ErrHexbakAssetManifest, bak.Version)
		}
		return nil
	}
	refs, err := referencedHexbakArchiveAssetIDs(bak)
	if err != nil {
		return err
	}
	entries := make(map[string]HexbakAsset, len(bak.Assets))
	for i, item := range bak.Assets {
		if _, duplicate := entries[item.AssetID]; duplicate {
			return fmt.Errorf("%w: duplicate asset %q", ErrHexbakAssetManifest, item.AssetID)
		}
		owner, _, err := assetstore.Parse(item.AssetID)
		if err != nil || owner != item.OwnerAgent || owner != bak.AgentName {
			return fmt.Errorf("%w: asset[%d] owner/header 不一致", ErrHexbakAssetManifest, i)
		}
		computedID, mime, digest, err := assetstore.Describe(owner, item.Data)
		if err != nil || computedID != item.AssetID || mime != item.MIME || digest != item.SHA256 {
			return fmt.Errorf("%w: asset[%d] 内容/MIME/SHA/ID 不一致", ErrHexbakAssetManifest, i)
		}
		entries[item.AssetID] = item
	}
	if len(entries) != len(refs) {
		return fmt.Errorf("%w: referenced=%d packed=%d", ErrHexbakAssetManifest, len(refs), len(entries))
	}
	for _, id := range refs {
		if _, ok := entries[id]; !ok {
			return fmt.Errorf("%w: referenced asset %q 未打包", ErrHexbakAssetManifest, id)
		}
	}
	return nil
}

func referencedHexbakArchiveAssetIDs(bak *Hexbak) ([]string, error) {
	recordRefs, err := ReferencedHexbakAssetIDs(bak.Records)
	if err != nil {
		return nil, err
	}
	seen := make(map[string]struct{}, len(recordRefs))
	for _, id := range recordRefs {
		seen[id] = struct{}{}
	}
	if bak.Version >= 5 {
		problemRefs, err := ReferencedHexbakProblemAssetIDs(bak.AgentName, bak.ProblemAttempts)
		if err != nil {
			return nil, err
		}
		for _, id := range problemRefs {
			seen[id] = struct{}{}
		}
	}
	out := make([]string, 0, len(seen))
	for id := range seen {
		out = append(out, id)
	}
	sort.Strings(out)
	return out, nil
}

// migrateHexbakAssets rewrites every nested asset ID and creates an independent target
// manifest. Source bytes are never mutated; missing v2 content fails before persistence.
func migrateHexbakAssets(source *Hexbak, target string, recs []*records.AgentRecord) ([]HexbakAsset, error) {
	refs, err := ReferencedHexbakAssetIDs(source.Records)
	if err != nil {
		return nil, err
	}
	if len(refs) > 0 && source.Version < 3 {
		return nil, fmt.Errorf("%w: v%d archive 引用了资产但不含受校验内容，无法跨 Tutor 迁移", ErrHexbakAssetManifest, source.Version)
	}
	if err := ValidateHexbakAssets(source); err != nil {
		return nil, err
	}
	mapping := make(map[string]string, len(source.Assets))
	out := make([]HexbakAsset, 0, len(source.Assets))
	for _, item := range source.Assets {
		targetID, mime, digest, err := assetstore.Describe(target, item.Data)
		if err != nil {
			return nil, fmt.Errorf("%w: target asset: %v", ErrHexbakAssetManifest, err)
		}
		mapping[item.AssetID] = targetID
		out = append(out, HexbakAsset{
			AssetID: targetID, OwnerAgent: target, SHA256: digest, MIME: mime,
			Data: append([]byte(nil), item.Data...),
		})
	}
	for _, rec := range recs {
		if rec == nil {
			continue
		}
		value, err := decodeJSONValue(rec.Fields)
		if err != nil {
			return nil, fmt.Errorf("%w: record %q fields JSON: %v", ErrHexbakAssetManifest, rec.RecordID, err)
		}
		if err := walkJSONStrings(value, func(raw string) (string, error) {
			if !assetstore.IsAssetID(raw) {
				return raw, nil
			}
			rewritten, ok := mapping[raw]
			if !ok {
				return "", fmt.Errorf("%w: record %q asset %q 无受校验内容", ErrHexbakAssetManifest, rec.RecordID, raw)
			}
			return rewritten, nil
		}); err != nil {
			return nil, err
		}
		raw, err := json.Marshal(value)
		if err != nil {
			return nil, fmt.Errorf("%w: rewrite record %q: %v", ErrHexbakAssetManifest, rec.RecordID, err)
		}
		rec.Fields = string(raw)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].AssetID < out[j].AssetID })
	return out, nil
}

func decodeJSONValue(raw string) (any, error) {
	dec := json.NewDecoder(bytes.NewBufferString(raw))
	dec.UseNumber()
	var value any
	if err := dec.Decode(&value); err != nil {
		return nil, err
	}
	if err := dec.Decode(new(any)); !errors.Is(err, io.EOF) {
		if err == nil {
			return nil, fmt.Errorf("multiple JSON values")
		}
		return nil, err
	}
	return value, nil
}

func walkJSONStrings(value any, visit func(string) (string, error)) error {
	switch v := value.(type) {
	case string:
		_, err := visit(v)
		return err
	case []any:
		for i := range v {
			if text, ok := v[i].(string); ok {
				next, err := visit(text)
				if err != nil {
					return err
				}
				v[i] = next
				continue
			}
			if err := walkJSONStrings(v[i], visit); err != nil {
				return err
			}
		}
	case map[string]any:
		for key, item := range v {
			if text, ok := item.(string); ok {
				next, err := visit(text)
				if err != nil {
					return err
				}
				v[key] = next
				continue
			}
			if err := walkJSONStrings(item, visit); err != nil {
				return err
			}
		}
	}
	return nil
}
