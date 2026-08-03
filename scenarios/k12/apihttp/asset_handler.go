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

	"github.com/hexagon-codes/hexclaw/records"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/assetstore"
	k12storage "github.com/hexagon-codes/hexclaw/scenarios/k12/storage"
	"github.com/hexagon-codes/hexclaw/scenarios/k12/usecase"
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
			var maxErr *http.MaxBytesError
			if errors.As(err, &maxErr) {
				writeAssetErr(w, err)
			} else {
				// Multipart syntax is wholly caller-controlled and safe to report
				// as the existing 400 contract; it is not a repository failure.
				writeErr(w, http.StatusBadRequest, err.Error())
			}
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
	ownerScope, err := h.authorizedAgentOwnerScope(r.Context(), agent)
	if err != nil {
		writeAssetScopeErr(w, err)
		return
	}
	if len(data) > assetstore.MaxAssetBytes {
		writeErr(w, http.StatusRequestEntityTooLarge, "图片超过大小上限（10MB）")
		return
	}
	repository := h.pageAssetGateway()
	if repository == nil {
		writeErr(w, http.StatusServiceUnavailable, "PageAsset repository unavailable")
		return
	}
	ready, err := repository.Persist(r.Context(), ownerScope, agent, data)
	if err != nil {
		writeAssetErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"asset_id": ready.Metadata.PageAssetID,
		"size":     ready.Metadata.SizeBytes,
	})
}

// getAsset GET /assets/{file}?agent= —— 回图。归属隔离：agent 必带且只在其目录下解析。
func (h *handler) getAsset(w http.ResponseWriter, r *http.Request) {
	agent := r.URL.Query().Get("agent")
	if agent == "" {
		writeErr(w, http.StatusBadRequest, "agent required")
		return
	}
	ownerScope, err := h.authorizedAgentOwnerScope(r.Context(), agent)
	if err != nil {
		writeAssetScopeErr(w, err)
		return
	}
	repository := h.pageAssetGateway()
	if repository == nil {
		writeErr(w, http.StatusServiceUnavailable, "PageAsset repository unavailable")
		return
	}
	ready, err := repository.OpenReady(
		r.Context(),
		ownerScope,
		agent,
		assetstore.IDPrefix+agent+"/"+r.PathValue("file"),
	)
	if err != nil {
		// Only owner-scoped absence is hidden as 404. Integrity drift and storage
		// failures are server faults; disguising them as absence prevents repair
		// and makes availability telemetry dishonest.
		if errors.Is(err, records.ErrScopeNotFound) ||
			errors.Is(err, k12storage.ErrPageAssetNotFound) {
			writeErr(w, http.StatusNotFound, "资产不存在")
			return
		}
		writeErr(w, http.StatusInternalServerError, "资产服务暂时不可用")
		return
	}
	w.Header().Set("Content-Type", ready.Metadata.MediaType)
	w.Header().Set("Content-Length", strconv.Itoa(len(ready.Data)))
	w.Header().Set("Cache-Control", "private, max-age=86400, immutable") // 内容寻址：同名即同内容
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(ready.Data)
}

func (h *handler) pageAssetGateway() usecase.PageAssetGateway {
	if h.rt.PageAssets != nil {
		return h.rt.PageAssets
	}
	if h.rt.Records == nil {
		return nil
	}
	return &usecase.PageAssetRepository{Records: h.rt.Records}
}

func writeAssetScopeErr(w http.ResponseWriter, err error) {
	if errors.Is(err, errAgentScopeNotFound) {
		writeErr(w, http.StatusNotFound, "资产不存在")
		return
	}
	writeErr(w, http.StatusUnauthorized, "authenticated asset principal required")
}

// writeAssetErr maps typed domain errors to stable public responses. Unknown
// errors are infrastructure failures and must never expose paths/SQL/details.
func writeAssetErr(w http.ResponseWriter, err error) {
	var maxErr *http.MaxBytesError
	switch {
	case errors.Is(err, records.ErrScopeNotFound),
		errors.Is(err, k12storage.ErrPageAssetNotFound):
		writeErr(w, http.StatusNotFound, "资产不存在")
	case errors.Is(err, k12storage.ErrPageAssetConflict):
		writeErr(w, http.StatusConflict, "资产状态冲突")
	case errors.As(err, &maxErr):
		writeErr(w, http.StatusRequestEntityTooLarge, "图片超过大小上限（10MB）")
	case errors.Is(err, assetstore.ErrAssetTooLarge):
		writeErr(w, http.StatusRequestEntityTooLarge, err.Error())
	case errors.Is(err, assetstore.ErrUnsupportedAssetMediaType):
		writeErr(w, http.StatusUnsupportedMediaType, err.Error())
	case errors.Is(err, assetstore.ErrInvalidAssetInput):
		writeErr(w, http.StatusBadRequest, err.Error())
	default:
		writeErr(w, http.StatusInternalServerError, "资产服务暂时不可用")
	}
}
