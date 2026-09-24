package api

import (
	"fmt"
	"net/http"
	"strings"

	siteadapter "prism-2api/internal/adapter/prism"
	"prism-2api/internal/adminapi"
	"prism-2api/internal/pool"
	"prism-2api/internal/tasks"
)

// ============================================================
// 机器导入：POST /api/v1/accounts/import（API Key 鉴权）
//
// 供注册机（freeagent-producer）在账号注册成功后直接推送：
// email + password + totp_secret 在这里组装成凭据束写入 RefreshToken
// （加密落库），账号先停用（待登录态）；login=true 时自动排一个侧车
// 登录任务，登录成功即启用进池。推送方无需管理台会话，也不需要懂
// 凭据束格式——和管理台 CSV 导入（credentialCSVToImports）等价。
// ============================================================

type machineImportAccount struct {
	Name     string `json:"name,omitempty"`
	Email    string `json:"email"`
	Password string `json:"password"`
	TOTP     string `json:"totp_secret"`
	Proxy    string `json:"proxy,omitempty"`
}

func (s *Server) handleMachineImport(h *adminapi.Handler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Accounts  []machineImportAccount `json:"accounts"`
			Overwrite bool                   `json:"overwrite"`
			Login     bool                   `json:"login"`
		}
		if err := decodeJSON(r, &body); err != nil || len(body.Accounts) == 0 {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "accounts is required"})
			return
		}
		res := adminapi.ImportResult{Errors: []adminapi.ImportItemError{}}
		// 只有新导入的才自动登录；已存在（skipped）交由管理台处理。
		loginNames := make([]string, 0, len(body.Accounts))
		for _, m := range body.Accounts {
			email := strings.TrimSpace(m.Email)
			password := strings.TrimSpace(m.Password)
			name := strings.TrimSpace(m.Name)
			if name == "" {
				name = pool.SanitizeName(email)
			}
			in := adminapi.AccountImport{
				Name:  name,
				Email: email,
			}
			if email != "" && password != "" {
				// 凭据束（邮箱+密码+TOTP+代理）：token 到期 keepalive 靠它自动重登。
				in.RefreshToken = siteadapter.BundleForImport(email, password, strings.TrimSpace(m.TOTP), strings.TrimSpace(m.Proxy))
				if p := strings.TrimSpace(m.Proxy); p != "" {
					in.ProxyURL = &p
				}
			}
			disabled := true
			in.Enabled = &disabled
			imported, skipped, err := h.ImportOne(in, body.Overwrite)
			if err != nil {
				res.Errors = append(res.Errors, adminapi.ImportItemError{Name: name, Error: err.Error()})
				continue
			}
			if imported {
				res.Imported++
				loginNames = append(loginNames, name)
			} else if skipped {
				res.Skipped++
			}
		}
		resp := map[string]any{"imported": res.Imported, "skipped": res.Skipped, "errors": res.Errors}
		if body.Login && len(loginNames) > 0 && s.jobs != nil {
			t := s.jobs.Enqueue(tasks.CreateRequest{
				Type:       "accounts.login",
				Title:      fmt.Sprintf("登录 %d 个账号（导入后自动）", len(loginNames)),
				Total:      len(loginNames),
				Cancelable: true,
				Meta:       map[string]any{"action": "login", "names": loginNames},
				Run: func(ctx *tasks.Context) (any, error) {
					return s.runAccountLogin(ctx, loginNames, "")
				},
			})
			resp["login_task_id"] = t.ID
		}
		writeJSON(w, http.StatusOK, resp)
	}
}
