package api

import (
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"prism-2api/internal/admin"
	"prism-2api/internal/auth"
)

// startKeepalive 启动账号池 Token 后台保活（kiro 60s / 过期前 15 分钟）。
// 空池安全；失败只记数。重复调用是 no-op。
// 死号链路（保持池子干净）：未就绪号靠保活走代理重登（指数退避）；
// 连败达阈值（约 3 小时救不回）自动删号，凭据摘要在删前落盘 dead-accounts.log。
func (s *Server) startKeepalive() {
	if s == nil || s.ka != nil {
		return
	}
	// 紧急闸：上游 auth 面（auth.openai.com）风控加强时，保活重登会全体撞 CF
	// 无限重试，把登录侧车（浏览器冷启×N）打满，连带 mint/材料产线全灭（线上
	// 2026-09-19 深夜实测 CPU 275% 对话雪崩）。WEB2API_KEEPALIVE=off 可一键停掉
	// 重登循环止血；账号现有 session 不受影响，风控松动后删 env 恢复。
	if v := strings.ToLower(strings.TrimSpace(os.Getenv("WEB2API_KEEPALIVE"))); v == "0" || v == "false" || v == "off" {
		log.Printf("keepalive: disabled by WEB2API_KEEPALIVE (relogin storm brake)")
		return
	}
	s.ka = auth.NewKeepalive(auth.KeepaliveConfig{
		OnDead: func(name string, lastErr error) {
			// 先落档案再删：删后凭据即从 pg 消失，摘要（邮箱/最近错误）留档可追溯。
			s.archiveDeadAccount(name, lastErr)
			if err := s.pool.Remove(name); err != nil {
				log.Printf("keepalive: remove dead account %q failed: %v", name, err)
				return
			}
			log.Printf("keepalive: dead account %q removed from pool", name)
			if s.logs != nil {
				s.logs.Publish(admin.Event{
					Type:      admin.EventCooldown,
					Time:      time.Now(),
					Account:   name,
					FailClass: "dead_removed",
				})
			}
		},
	})
	s.ka.Start(func() []auth.Ref {
		if s.pool == nil {
			return nil
		}
		accs := s.pool.Accounts()
		out := make([]auth.Ref, 0, len(accs))
		for _, a := range accs {
			if a == nil || a.Tokens == nil {
				continue
			}
			// 停用账号不保活：导入但还没登录的账号是停用态（等用户在管理端/CLI 选中去登录），
			// 没有 AccessToken 时 KeepaliveDue 恒为真，不排除就会把刚导入的账号全部自动登一遍。
			if !a.Enabled() {
				continue
			}
			out = append(out, auth.Ref{Name: a.Name, Tokens: a.Tokens})
		}
		return out
	})
}

// archiveDeadAccount 删号前把凭据摘要追加到数据卷（/data/dead-accounts.log），
// 一行 JSON：时间/账号名/邮箱/最后错误。仅摘要不落密码，防泄漏。
func (s *Server) archiveDeadAccount(name string, lastErr error) {
	f, err := os.OpenFile("/data/dead-accounts.log", os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	email := ""
	if acc := s.pool.Get(name); acc != nil {
		email = acc.IdentityEmail()
	}
	msg := ""
	if lastErr != nil {
		msg = lastErr.Error()
		if len(msg) > 200 {
			msg = msg[:200]
		}
	}
	fmt.Fprintf(f, "{\"time\":%q,\"account\":%q,\"email\":%q,\"last_error\":%q}\n",
		time.Now().UTC().Format(time.RFC3339), name, email, msg)
}
