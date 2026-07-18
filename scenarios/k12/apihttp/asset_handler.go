package apihttp

// 作品照片资产端点（§3.10 作品 / §5.5 source_asset_id 的真实上传载体，最小资产服务，
// 设计申报见 scenarios/k12/assetstore 包注释）：
//
//	POST /assets?agent=   multipart（file 字段）或 JSON {agent, data_base64}
//	GET  /assets/{file}?agent=   回图（归属隔离：只在该 agent 目录下解析）
//
// 契约：魔数非 image/* → 415；>10MB → 413；跨 agent / 穿越名 → 404。

import (
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/hexagon-codes/hexclaw/scenarios/k12/assetstore"
)

// assetUploadReq base64 JSON 上传体（IM/脚本客户端友好；桌面端走 multipart）。
type assetUploadReq struct {
	Agent      string `json:"agent"`
	DataBase64 string `json:"data_base64"`
}

// uploadAsset POST /assets —— 作品照片上传（multipart 或 base64 JSON，自动识别）。
func (h *handler) uploadAsset(w http.ResponseWriter, r *http.Request) {
	// 统一在传输层钉大小上限（留 1MB 信封余量给 multipart 边界/base64 膨胀由下方另算）。
	r.Body = http.MaxBytesReader(w, r.Body, assetstore.MaxAssetBytes*3/2+1<<20)

	agent := r.URL.Query().Get("agent")
	var data []byte
	ct := r.Header.Get("Content-Type")
	switch {
	case strings.HasPrefix(ct, "multipart/form-data"):
		if err := r.ParseMultipartForm(assetstore.MaxAssetBytes + 1<<20); err != nil {
			writeAssetErr(w, err)
			return
		}
		if agent == "" {
			agent = r.FormValue("agent")
		}
		file, _, err := r.FormFile("file")
		if err != nil {
			writeErr(w, http.StatusBadRequest, "缺少 file 字段")
			return
		}
		defer file.Close()
		raw, err := io.ReadAll(io.LimitReader(file, assetstore.MaxAssetBytes+1))
		if err != nil {
			writeAssetErr(w, err)
			return
		}
		data = raw
	default:
		var req assetUploadReq
		if !decode(w, r, &req) {
			return
		}
		if agent == "" {
			agent = req.Agent
		}
		raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(req.DataBase64))
		if err != nil {
			writeErr(w, http.StatusBadRequest, "data_base64 解码失败")
			return
		}
		data = raw
	}
	if agent == "" {
		writeErr(w, http.StatusBadRequest, "agent required")
		return
	}
	if len(data) > assetstore.MaxAssetBytes {
		writeErr(w, http.StatusRequestEntityTooLarge, "图片超过大小上限（10MB）")
		return
	}
	id, err := assetstore.Save(agent, data)
	if err != nil {
		writeAssetErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"asset_id": id, "size": len(data)})
}

// getAsset GET /assets/{file}?agent= —— 回图。归属隔离：agent 必带且只在其目录下解析。
func (h *handler) getAsset(w http.ResponseWriter, r *http.Request) {
	agent := r.URL.Query().Get("agent")
	if agent == "" {
		writeErr(w, http.StatusBadRequest, "agent required")
		return
	}
	data, mime, err := assetstore.Read(agent, r.PathValue("file"))
	if err != nil {
		// 不区分「不存在 / 越权 / 非法名」——一律 404，防资产枚举探测。
		writeErr(w, http.StatusNotFound, "资产不存在")
		return
	}
	w.Header().Set("Content-Type", mime)
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	w.Header().Set("Cache-Control", "private, max-age=86400, immutable") // 内容寻址：同名即同内容
	w.WriteHeader(http.StatusOK)
	w.Write(data)
}

// writeAssetErr 资产错误 → HTTP 状态：魔数不符 415、超限 413、其余按输入错 400。
func writeAssetErr(w http.ResponseWriter, err error) {
	msg := err.Error()
	var maxErr *http.MaxBytesError
	switch {
	case errors.As(err, &maxErr), strings.Contains(msg, "大小上限"):
		writeErr(w, http.StatusRequestEntityTooLarge, "图片超过大小上限（10MB）")
	case strings.Contains(msg, "只接受图片"), strings.Contains(msg, "不是图片"):
		writeErr(w, http.StatusUnsupportedMediaType, msg)
	default:
		writeErr(w, http.StatusBadRequest, msg)
	}
}
