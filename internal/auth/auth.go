// Package auth 解析 WorkBuddy auth 文件（嵌套形/扁平形双形态），
// 提供 region 判定与 refresh 后的原子写回。
package auth

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Auth 是归一化后的账号凭证（来源可以是插件 OAuth 嵌套形或 CPA 面板扁平形）。
type Auth struct {
	mu        sync.RWMutex
	refreshMu sync.Mutex
	saveMu    sync.Mutex

	AccessToken  string
	RefreshToken string
	ExpiresAt    int64 // Unix 秒
	Domain       string
	UID          string
	EnterpriseID string
	Nickname     string
	FilePath     string // 来源文件；refresh 后原子写回此处
}

// Credentials is an immutable point-in-time copy of an account credential.
type Credentials struct {
	AccessToken  string
	RefreshToken string
	ExpiresAt    int64
	Domain       string
	UID          string
	EnterpriseID string
	Nickname     string
	FilePath     string
}

// Lock serializes refresh operations for one account. Credential field access
// still goes through Snapshot and ApplyRefresh so HTTP calls never observe a
// partially updated token bundle.
func (a *Auth) Lock() { a.refreshMu.Lock() }

// Unlock 释放 a.Lock 获取的锁。
func (a *Auth) Unlock() { a.refreshMu.Unlock() }

// Snapshot returns a consistent copy safe for concurrent request building.
func (a *Auth) Snapshot() Credentials {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return Credentials{
		AccessToken: a.AccessToken, RefreshToken: a.RefreshToken, ExpiresAt: a.ExpiresAt,
		Domain: a.Domain, UID: a.UID, EnterpriseID: a.EnterpriseID,
		Nickname: a.Nickname, FilePath: a.FilePath,
	}
}

// ApplyRefresh atomically updates the fields returned by a token refresh.
// Empty optional values preserve their current value.
func (a *Auth) ApplyRefresh(accessToken, refreshToken, domain string, expiresAt int64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.AccessToken = accessToken
	if refreshToken != "" {
		a.RefreshToken = refreshToken
	}
	if domain != "" {
		a.Domain = domain
	}
	if expiresAt > 0 {
		a.ExpiresAt = expiresAt
	}
}

// globalSuffix 判定全球区（global）账号的域名后缀；子域（如 www./api.）也属于全球区。
const globalSuffix = ".workbuddy.ai"

// Region 返回 "cn" 或 "global"。domain 为空视为 CN（向后兼容）。
func (a *Auth) Region() string {
	d := strings.ToLower(strings.TrimSpace(a.Snapshot().Domain))
	if d == strings.TrimPrefix(globalSuffix, ".") || strings.HasSuffix(d, globalSuffix) {
		return "global"
	}
	return "cn"
}

// NeedsRefresh 报告 token 是否将在 within 内过期（或已过期/无 expiry）。
func (a *Auth) NeedsRefresh(within time.Duration) bool {
	expiresAt := a.Snapshot().ExpiresAt
	if expiresAt <= 0 {
		return true
	}
	return time.Now().Add(within).Unix() >= expiresAt
}

// Parse 兼容两种磁盘形态：
//
//	嵌套形 {"auth":{...},"account":{...}}  （插件 OAuth 输出）
//	扁平形 {"accessToken":...,"uid":...}   （CPA 面板手建）
func Parse(raw []byte) (*Auth, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("empty auth storage")
	}
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(raw, &probe); err != nil {
		return nil, fmt.Errorf("storage_parse_error: %w", err)
	}
	var a Auth
	if _, nested := probe["auth"]; nested {
		var n struct {
			Auth struct {
				AccessToken  string `json:"accessToken"`
				RefreshToken string `json:"refreshToken"`
				ExpiresAt    int64  `json:"expiresAt"`
				Domain       string `json:"domain"`
			} `json:"auth"`
			Account struct {
				UID          string `json:"uid"`
				EnterpriseID string `json:"enterpriseId"`
				Nickname     string `json:"nickname"`
			} `json:"account"`
		}
		if err := json.Unmarshal(raw, &n); err != nil {
			return nil, fmt.Errorf("storage_parse_error: %w", err)
		}
		a = Auth{
			AccessToken:  n.Auth.AccessToken,
			RefreshToken: n.Auth.RefreshToken,
			ExpiresAt:    n.Auth.ExpiresAt,
			Domain:       n.Auth.Domain,
			UID:          n.Account.UID,
			EnterpriseID: n.Account.EnterpriseID,
			Nickname:     n.Account.Nickname,
		}
	} else {
		var f struct {
			AccessToken  string `json:"accessToken"`
			RefreshToken string `json:"refreshToken"`
			ExpiresAt    int64  `json:"expiresAt"`
			Domain       string `json:"domain"`
			UID          string `json:"uid"`
			EnterpriseID string `json:"enterpriseId"`
			Nickname     string `json:"nickname"`
		}
		if err := json.Unmarshal(raw, &f); err != nil {
			return nil, fmt.Errorf("storage_parse_error: %w", err)
		}
		a = Auth{
			AccessToken:  f.AccessToken,
			RefreshToken: f.RefreshToken,
			ExpiresAt:    f.ExpiresAt,
			Domain:       f.Domain,
			UID:          f.UID,
			EnterpriseID: f.EnterpriseID,
			Nickname:     f.Nickname,
		}
	}
	if strings.TrimSpace(a.AccessToken) == "" {
		return nil, fmt.Errorf("parse_error: missing accessToken")
	}
	return &a, nil
}

// SaveAtomic 以嵌套形原子写回 FilePath（tmp + rename），保持 CPA 插件可读格式。
// 使用一致快照写回，防止与 RefreshToken 并发时落盘半更新 token。
// 防御：accessToken 为空时拒绝写回，避免误用空凭证覆盖有效文件。
func (a *Auth) SaveAtomic() error {
	a.saveMu.Lock()
	defer a.saveMu.Unlock()
	snapshot := a.Snapshot()
	if strings.TrimSpace(snapshot.AccessToken) == "" {
		return fmt.Errorf("save refused: empty accessToken (uid=%s)", snapshot.UID)
	}
	if snapshot.FilePath == "" {
		return fmt.Errorf("no FilePath set")
	}
	doc := map[string]any{
		"auth": map[string]any{
			"accessToken":  snapshot.AccessToken,
			"refreshToken": snapshot.RefreshToken,
			"expiresAt":    snapshot.ExpiresAt,
			"domain":       snapshot.Domain,
		},
		"account": map[string]any{
			"uid":          snapshot.UID,
			"enterpriseId": snapshot.EnterpriseID,
			"nickname":     snapshot.Nickname,
		},
	}
	raw, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	tmp := snapshot.FilePath + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, snapshot.FilePath)
}

// LoadDir 扫描 dir 下 workbuddy*.json，只收 wantRegion（"cn"/"global"）。
// 解析失败与 region 不符的文件静默跳过（启动日志由调用方统计）。
func LoadDir(dir, wantRegion string) ([]*Auth, error) {
	files, err := filepath.Glob(filepath.Join(dir, "workbuddy*.json"))
	if err != nil {
		return nil, err
	}
	var out []*Auth
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		a, err := Parse(raw)
		if err != nil || a.Region() != wantRegion {
			continue
		}
		a.FilePath = f
		out = append(out, a)
	}
	return out, nil
}
