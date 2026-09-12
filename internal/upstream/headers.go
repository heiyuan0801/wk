// Package headers 构造三类上游请求头（common / chat / billing / refresh）。
// 规则来自 docs/api-reference.md §0/§4/§6。
package upstream

import (
	"net/http"
	"strings"

	"workbuddy2api/internal/auth"
)

const (
	// Match the Electron client family observed in WorkBuddy 5.5.2 traffic.
	clientUA            = "WorkBuddy/5.5.2 CLI/2.137.1"
	originRefererCN     = "https://www.workbuddy.cn"
	originRefererGlobal = "https://www.workbuddy.ai"
)

func originRefererFor(a *auth.Auth) string {
	domain := a.RequestDomain()
	if domain != "" {
		if strings.HasSuffix(domain, ".ai") {
			return "https://" + domain
		}
		if strings.HasSuffix(domain, ".cn") {
			return "https://" + domain
		}
	}
	if a != nil && a.Region() == auth.RegionGlobal {
		return originRefererGlobal
	}
	return originRefererCN
}

// CommonHeaders 设置所有 API 共享的请求头。
func CommonHeaders(req *http.Request, a *auth.Auth) {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	origin := originRefererFor(a)
	req.Header.Set("Origin", origin)
	req.Header.Set("Referer", origin+"/")
	req.Header.Set("User-Agent", clientUA)
}

// ChatHeaders 在 common 之上加 chat 专属的账号头。
// 缺省字段用 X-No-* 约定（与 CodeBuddy 官方 CLI 一致）。
func ChatHeaders(req *http.Request, a *auth.Auth) {
	CommonHeaders(req, a)
	credentials := a.Snapshot()
	if credentials.AccessToken != "" {
		req.Header.Set("Authorization", "Bearer "+credentials.AccessToken)
	} else {
		req.Header.Set("X-No-Authorization", "1")
	}
	if credentials.UID != "" {
		req.Header.Set("X-User-Id", credentials.UID)
	} else {
		req.Header.Set("X-No-User-Id", "1")
	}
	if credentials.EnterpriseID != "" {
		req.Header.Set("X-Enterprise-Id", credentials.EnterpriseID)
	} else {
		req.Header.Set("X-No-Enterprise-Id", "1")
	}
	// 安全红线：绝不在 chat 请求里携带 X-Refresh-Token。
	req.Header.Set("X-Domain", a.RequestDomain())
	req.Header.Set("X-Product", "SaaS")
}

// BillingHeaders billing 接口请求头。
func BillingHeaders(req *http.Request, a *auth.Auth) {
	origin := originRefererFor(a)
	req.Header.Set("Origin", origin)
	req.Header.Set("Referer", origin+"/")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	req.Header.Set("X-Product", "SaaS")
	req.Header.Set("User-Agent", clientUA)
	credentials := a.Snapshot()
	req.Header.Set("Authorization", "Bearer "+credentials.AccessToken)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	if credentials.UID != "" {
		req.Header.Set("X-User-Id", credentials.UID)
	}
	if credentials.EnterpriseID != "" {
		req.Header.Set("X-Enterprise-Id", credentials.EnterpriseID)
		req.Header.Set("X-Tenant-Id", credentials.EnterpriseID)
	}
	req.Header.Set("X-Domain", a.RequestDomain())
}

// RefreshHeaders refresh 端点专属头（X-Refresh-Token 只允许出现在这里）。
func RefreshHeaders(req *http.Request, a *auth.Auth) {
	CommonHeaders(req, a)
	credentials := a.Snapshot()
	req.Header.Set("X-Refresh-Token", credentials.RefreshToken)
	if credentials.EnterpriseID != "" {
		req.Header.Set("X-Enterprise-Id", credentials.EnterpriseID)
	}
	req.Header.Set("X-Auth-Refresh-Source", "workbuddy")
}
