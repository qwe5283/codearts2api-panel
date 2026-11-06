package server

import (
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"

	"codearts2api-panel/internal/auth"
)

// Import validates the whole batch before persisting any account. SaveNew is
// exclusive; rollback removes only files successfully created by this request.
func (h *Handler) adminImport(w http.ResponseWriter, r *http.Request) {
	if h.cfg.AuthDir == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "message": "auth_dir 未配置"})
		return
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxAuthBytes+1))
	if err != nil || len(raw) > maxAuthBytes {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "message": "凭据 JSON 无效或超过 8MB"})
		return
	}
	accounts, err := auth.ParseImport(raw)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "message": err.Error()})
		return
	}
	h.accountsMu.Lock()
	defer h.accountsMu.Unlock()
	seen := map[string]bool{}
	for _, a := range accounts {
		name := a.FileName()
		if seen[name] || h.cfg.Pool.Get(a.UserID) != nil {
			writeJSON(w, http.StatusConflict, map[string]any{"ok": false, "message": "批次包含重复或已存在的账号"})
			return
		}
		seen[name] = true
		if _, err := os.Lstat(filepath.Join(h.cfg.AuthDir, name)); !errors.Is(err, os.ErrNotExist) {
			writeJSON(w, http.StatusConflict, map[string]any{"ok": false, "message": "账号文件已存在或无法访问"})
			return
		}
	}
	created := []string{}
	for _, a := range accounts {
		if err := auth.SaveNew(h.cfg.AuthDir, a); err != nil {
			for _, path := range created {
				_ = os.Remove(path)
			}
			// 带上底层错误：权限类失败自带 Docker 挂载解法指引（见 auth.permHint），
			// 只回「本批次已回滚」等于把最有用的那半句丢掉。
			writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "message": "保存失败，本批次已回滚：" + err.Error()})
			return
		}
		created = append(created, filepath.Join(h.cfg.AuthDir, a.FileName()))
	}
	summary := make([]map[string]string, 0, len(accounts))
	for _, a := range accounts {
		h.cfg.Pool.AddAccount(a)
		summary = append(summary, map[string]string{"uid": a.UserID, "nickname": a.UserName})
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "imported": len(accounts), "accounts": summary})
}

func (h *Handler) adminDelete(w http.ResponseWriter, r *http.Request) {
	body, err := readUIDBody(r)
	if err != nil || body.UID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "message": "uid required"})
		return
	}
	h.accountsMu.Lock()
	defer h.accountsMu.Unlock()
	if h.cfg.Pool.Get(body.UID) == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"ok": false, "message": "account not found"})
		return
	}
	if err := h.cfg.Pool.Remove(body.UID); err != nil {
		writeJSON(w, http.StatusConflict, map[string]any{"ok": false, "message": err.Error()})
		return
	}
	h.convMu.Lock()
	delete(h.chats, body.UID)
	for conversation, account := range h.convAcct {
		if account == body.UID {
			delete(h.convAcct, conversation)
			delete(h.convSeen, conversation)
		}
	}
	h.convMu.Unlock()
	h.saveChats()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
