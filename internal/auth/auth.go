// Package auth 管理 auths/ 目录下的 CodeArts 凭证文件 codearts-{user_id}.json。
package auth

import (
	"crypto/elliptic"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Auth 单个账号凭证（登录 oauth2/tokens 返回）。
type Auth struct {
	UserID   string `json:"user_id"`
	UserName string `json:"user_name"`
	DomainID string `json:"domain_id"`
	// Note 运维备注（如注册手机号、用途标记）。纯本地字段，不参与任何上游请求；
	// 上游身份端点不返回手机号，这类信息只能在导入/注册时自己记下来。
	Note            string `json:"note,omitempty"`
	CloudDragonTok  string `json:"cloud_dragon_token"` // = STS security_token，chat 用 x-auth-token
	AccessKeyID     string `json:"access_key_id"`
	SecretAccessKey string `json:"secret_access_key"`
	Expiration      string `json:"expiration"` // RFC3339
	RefreshToken    string `json:"refresh_token"`
	CodeVerifier    string `json:"code_verifier"` // PKCE verifier，refresh 需要
	UpdatedAt       int64  `json:"updated_at"`

	// ClientID 签发该 refresh_token 的 OAuth client。refresh_token 与 client_id
	// 绑定，换 client_id 刷新会被 STS 拒（invalid client id）。留空表示旧凭证，
	// 走 DefaultLoginConfig 的默认值。
	ClientID string `json:"client_id,omitempty"`

	// DPoPPrivateKey JWK（RFC 9449，ES256）。refresh_token 与签发时的 DPoP 公钥
	// 绑定：刷新必须用同一密钥对，否则 STS 报 InvalidDPoPHeader。留空表示旧凭证
	// （刷新时退化为临时密钥，仅在服务端不校验绑定时可用）。
	DPoPPrivateKey map[string]string `json:"dpop_private_key,omitempty"`

	path    string
	mu      sync.Mutex
	deleted bool
}

// ClientIDOr 返回该凭证绑定的 OAuth client_id；未记录时返回 fallback。
func (a *Auth) ClientIDOr(fallback string) string {
	if a == nil {
		return fallback
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.ClientID != "" {
		return a.ClientID
	}
	return fallback
}

// SetClientID 记录签发该 refresh_token 的 client_id。
func (a *Auth) SetClientID(v string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.ClientID = v
}

// DPoPPrivateJWK 返回该凭证绑定的 DPoP 私钥副本（可能为 nil，表示旧凭证）。
func (a *Auth) DPoPPrivateJWK() map[string]string {
	if a == nil {
		return nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.DPoPPrivateKey) == 0 {
		return nil
	}
	out := make(map[string]string, len(a.DPoPPrivateKey))
	for k, v := range a.DPoPPrivateKey {
		out[k] = v
	}
	return out
}

// SetDPoPPrivateKey 记录该凭证绑定的 DPoP 私钥。
func (a *Auth) SetDPoPPrivateKey(jwk map[string]string) {
	if len(jwk) == 0 {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.DPoPPrivateKey = make(map[string]string, len(jwk))
	for k, v := range jwk {
		a.DPoPPrivateKey[k] = v
	}
}

// New 构造 Auth（登录落盘用）。
func New(userID, userName, domainID, token, ak, sk, expiration, refreshToken, codeVerifier string, dpopPrivateJWK ...map[string]string) *Auth {
	a := &Auth{
		UserID:          userID,
		UserName:        userName,
		DomainID:        domainID,
		CloudDragonTok:  token,
		AccessKeyID:     ak,
		SecretAccessKey: sk,
		Expiration:      expiration,
		RefreshToken:    refreshToken,
		CodeVerifier:    codeVerifier,
		UpdatedAt:       time.Now().Unix(),
	}
	if len(dpopPrivateJWK) > 0 && len(dpopPrivateJWK[0]) > 0 {
		a.DPoPPrivateKey = make(map[string]string, len(dpopPrivateJWK[0]))
		for k, v := range dpopPrivateJWK[0] {
			a.DPoPPrivateKey[k] = v
		}
	}
	return a
}

// FileName 返回 auth 文件名。
func (a *Auth) FileName() string {
	id := a.UserID
	if id == "" || strings.ContainsAny(id, `/\\:.`) || strings.Trim(id, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789_-") != "" || len(id) > 120 {
		basis := id
		if basis == "" {
			basis = a.AccessKeyID
		}
		sum := sha256.Sum256([]byte(basis))
		id = fmt.Sprintf("%x", sum[:16])
	}
	return fmt.Sprintf("codearts-%s.json", id)
}

// Token 返回 cloud_dragon_token（并发安全快照）。
func (a *Auth) Token() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.CloudDragonTok
}

// Refresh 返回 refresh_token。
func (a *Auth) Refresh() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.RefreshToken
}

// Verifier 返回 PKCE code_verifier。
func (a *Auth) Verifier() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.CodeVerifier
}

// Credentials 返回签名请求所需的 STS 凭据快照。
func (a *Auth) Credentials() (token, accessKeyID, secretAccessKey string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.CloudDragonTok, a.AccessKeyID, a.SecretAccessKey
}

// UpdateCredentials 原子替换刷新后的 STS 凭据并写回磁盘。
func (a *Auth) UpdateCredentials(token, accessKeyID, secretAccessKey, expiration, refreshToken string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.updateCredentialsLocked(token, accessKeyID, secretAccessKey, expiration, refreshToken)
}

// UpdateCredentialsIfCurrent cannot overwrite a newer login while STS was in flight.
func (a *Auth) UpdateCredentialsIfCurrent(expected, token, accessKeyID, secretAccessKey, expiration, refreshToken string) (bool, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.RefreshToken != expected {
		return false, nil
	}
	return true, a.updateCredentialsLocked(token, accessKeyID, secretAccessKey, expiration, refreshToken)
}

func (a *Auth) updateCredentialsLocked(token, accessKeyID, secretAccessKey, expiration, refreshToken string) error {
	if a.deleted {
		return fmt.Errorf("account removed")
	}
	a.CloudDragonTok = token
	a.AccessKeyID = accessKeyID
	a.SecretAccessKey = secretAccessKey
	a.Expiration = expiration
	if refreshToken != "" {
		a.RefreshToken = refreshToken
	}
	a.UpdatedAt = time.Now().Unix()
	return saveLocked(a.path, a)
}

// UpdateIdentity 回填登录身份字段（WebUI 回调可能拿不到 user_id/user_name）。
func (a *Auth) UpdateIdentity(userID, userName, domainID string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.deleted {
		return fmt.Errorf("account removed")
	}
	if userID != "" {
		a.UserID = userID
	}
	if userName != "" {
		a.UserName = userName
	}
	if domainID != "" {
		a.DomainID = domainID
	}
	a.UpdatedAt = time.Now().Unix()
	return saveLocked(a.path, a)
}

// SetPathAndSave 设置落盘路径并写出（WebUI 登录在拿到身份后才能确定文件名）。
func (a *Auth) SetPathAndSave(path string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := validPath(path); err != nil {
		return err
	}
	if a.deleted {
		return fmt.Errorf("account removed")
	}
	a.path = path
	return saveLocked(a.path, a)
}

// ExpiresAt 返回 token 过期时间。
func (a *Auth) ExpiresAt() time.Time {
	a.mu.Lock()
	defer a.mu.Unlock()
	if t, err := time.Parse(time.RFC3339, a.Expiration); err == nil {
		return t
	}
	return time.Time{}
}

// Remaining 返回剩余有效期。
func (a *Auth) Remaining() time.Duration {
	t := a.ExpiresAt()
	if t.IsZero() {
		return 24 * time.Hour
	}
	return time.Until(t)
}

// Expired 报告 token 是否已过期。
func (a *Auth) Expired() bool { return a.Remaining() <= 0 }

// ExpiringSoon 报告 token 是否将在 within 内过期。
func (a *Auth) ExpiringSoon(within time.Duration) bool {
	r := a.Remaining()
	return r > 0 && r <= within
}

// Save 原子写回 auth 文件（0600）。
func (a *Auth) Save() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.deleted {
		return fmt.Errorf("account removed")
	}
	return saveLocked(a.path, a)
}

// SetPath 设置文件路径（LoadDir 内部调用）。
func (a *Auth) SetPath(p string) { a.mu.Lock(); defer a.mu.Unlock(); a.path = p }

// LoadDir 加载 auths 目录下全部 codearts-*.json。
func LoadDir(dir string) ([]*Auth, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []*Auth
	for _, e := range entries {
		if e.IsDir() || !strings.HasPrefix(e.Name(), "codearts-") || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		if e.Type()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("credential symlinks are not supported")
		}
		p := filepath.Join(dir, e.Name())
		raw, err := os.ReadFile(p)
		if err != nil {
			return nil, err
		}
		var a Auth
		if err := json.Unmarshal(raw, &a); err != nil {
			return nil, fmt.Errorf("parse %s: %w", e.Name(), err)
		}
		if err := validate(&a, false); err != nil {
			return nil, fmt.Errorf("parse %s: %w", e.Name(), err)
		}
		a.path = p
		out = append(out, &a)
	}
	return out, nil
}

// SaveNew 把新账号写入 auths 目录。
func SaveNew(dir string, a *Auth) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return permHint(err)
	}
	path := filepath.Join(dir, a.FileName())
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return permHint(err)
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(path)
		return err
	}
	if err := a.SetPathAndSave(path); err != nil {
		_ = os.Remove(path)
		return err
	}
	return nil
}

func saveLocked(path string, v any) (err error) {
	if err := validPath(path); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	// 写盘失败统一过 permHint：任一环节 EACCES 都补上 Docker 挂载权限指引。
	defer func() { err = permHint(err) }()
	file, err := os.CreateTemp(filepath.Dir(path), ".codearts-*.tmp")
	if err != nil {
		return err
	}
	tmp := file.Name()
	defer os.Remove(tmp)
	if err := file.Chmod(0o600); err != nil {
		file.Close()
		return err
	}
	if _, err := file.Write(raw); err != nil {
		file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// permHint 给权限类写失败补上 Docker bind-mount 的解法指引。
// 容器内以 uid 10001 运行，宿主机挂载目录属主不同是首次部署最常见的失败点；
// 裸的 "permission denied" 只说不行、不说改什么，用户只能去翻 README。
// 非权限错误原样返回，原错误用 %w 包裹，上层仍可用 errors.Is 分类。
func permHint(err error) error {
	if err == nil || !errors.Is(err, fs.ErrPermission) {
		return err
	}
	return fmt.Errorf("%w\n（Docker 部署：容器内用户对宿主机挂载目录无写权限。解法任选："+
		"1) 以本机 uid 运行容器：PUID=$(id -u) PGID=$(id -g) docker compose up -d；"+
		"2) sudo chown -R 10001:10001 ./auths ./data ./config.json；"+
		"3) compose 设 user: \"0:0\" 以 root 运行）", err)
}

func validPath(path string) error {
	base := filepath.Base(path)
	if path == "" || !strings.HasPrefix(base, "codearts-") || !strings.HasSuffix(base, ".json") {
		return fmt.Errorf("invalid credential file path")
	}
	if st, err := os.Lstat(path); err == nil && (st.Mode()&os.ModeSymlink != 0 || !st.Mode().IsRegular()) {
		return fmt.Errorf("credential path is not a regular file")
	} else if err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// RemoveFile prevents an in-flight refresh from recreating a removed credential.
func (a *Auth) RemoveFile() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := validPath(a.path); err != nil {
		return err
	}
	if err := os.Remove(a.path); err != nil && !os.IsNotExist(err) {
		return err
	}
	a.deleted = true
	return nil
}

// Refreshable requires the original proof and verifier, not merely a refresh token.
func (a *Auth) Refreshable() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.RefreshToken == "" || a.CodeVerifier == "" || a.DPoPPrivateKey["kty"] != "EC" || a.DPoPPrivateKey["crv"] != "P-256" {
		return false
	}
	values := map[string][]byte{}
	for _, key := range []string{"x", "y", "d"} {
		raw, err := base64.RawURLEncoding.DecodeString(a.DPoPPrivateKey[key])
		if err != nil || len(raw) != 32 {
			return false
		}
		values[key] = raw
	}
	curve := elliptic.P256()
	d := new(big.Int).SetBytes(values["d"])
	if d.Sign() <= 0 || d.Cmp(curve.Params().N) >= 0 {
		return false
	}
	x, y := curve.ScalarBaseMult(values["d"])
	return x.Cmp(new(big.Int).SetBytes(values["x"])) == 0 && y.Cmp(new(big.Int).SetBytes(values["y"])) == 0
}

// ReplaceFrom preserves the account's Auth pointer while replacing its credential.
func (a *Auth) ReplaceFrom(next *Auth) {
	if a == next {
		return
	}
	next.mu.Lock()
	defer next.mu.Unlock()
	a.mu.Lock()
	defer a.mu.Unlock()
	a.CloudDragonTok, a.AccessKeyID, a.SecretAccessKey = next.CloudDragonTok, next.AccessKeyID, next.SecretAccessKey
	a.Expiration, a.RefreshToken, a.CodeVerifier = next.Expiration, next.RefreshToken, next.CodeVerifier
	a.ClientID, a.DPoPPrivateKey, a.UpdatedAt = next.ClientID, next.DPoPPrivateKey, next.UpdatedAt
	a.path, a.deleted = next.path, false
}
