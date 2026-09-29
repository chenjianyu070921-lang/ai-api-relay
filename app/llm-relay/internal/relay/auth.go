package relay

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
	"gorm.io/gorm"

	"llm-relay/app/llm-relay/internal/model"
)

// TokenInfo 鉴权结果，挂在 request context 上供后续计费/日志使用
type TokenInfo struct {
	ID        int64
	UserID    int64
	Quota     int64    // -1 = 不限（受用户配额约束）
	Models    []string // 模型白名单，空 = 不限
	IPLimits  []string // IP 白名单，空 = 不限
	ExpiredAt *time.Time
	User      *model.User
}

var (
	ErrAuthFailed = errors.New("invalid api key")
	ErrUserBanned = errors.New("user disabled")
	ErrQuotaZero  = errors.New("quota exhausted")
)

func HashKey(plain string) string {
	sum := sha256.Sum256([]byte(plain))
	return hex.EncodeToString(sum[:])
}

// tokenCacheEntry Redis 缓存的令牌信息（TokenCacheSec TTL，对齐 02 文档第 8 节）
type tokenCacheEntry struct {
	ID        int64      `json:"id"`
	UserID    int64      `json:"user_id"`
	Quota     int64      `json:"quota"`
	Models    []string   `json:"models"`
	IPLimits  []string   `json:"ip_limits"`
	ExpiredAt *time.Time `json:"expired_at"`
}

const tokenCacheKeyPrefix = "lr:token:"

// Authenticate 令牌鉴权主流程：
// 1. 固定前缀快速拒绝（不是 sk-relay- 开头直接 401，不打库）
// 2. sha256(key) 查 Redis 缓存（30s）→ 未命中查 lr_token 回填缓存
// 3. 校验过期/IP 白名单/用户状态；模型白名单由 handler 解析出 model 后调 CheckModelWhitelist
func Authenticate(ctx context.Context, r *http.Request, db *gorm.DB, rdb redis.UniversalClient, cacheSec int) (*TokenInfo, error) {
	plain := extractBearer(r)
	if plain == "" || !strings.HasPrefix(plain, "sk-relay-") {
		return nil, ErrAuthFailed
	}
	keyHash := HashKey(plain)

	info, err := loadToken(ctx, db, rdb, keyHash, cacheSec)
	if err != nil {
		return nil, ErrAuthFailed
	}
	if info.ExpiredAt != nil && info.ExpiredAt.Before(time.Now()) {
		return nil, ErrAuthFailed
	}
	if !ipAllowed(clientIP(r), info.IPLimits) {
		return nil, ErrAuthFailed
	}

	user, err := model.GetUser(db, info.UserID)
	if err != nil || user.Status != 1 {
		return nil, ErrUserBanned
	}
	info.User = user
	return info, nil
}

// loadToken Redis 缓存 → DB 回源；缓存读写失败都降级为直查库，不影响鉴权
func loadToken(ctx context.Context, db *gorm.DB, rdb redis.UniversalClient, keyHash string, cacheSec int) (*TokenInfo, error) {
	cacheKey := tokenCacheKeyPrefix + keyHash

	if rdb != nil {
		if raw, err := rdb.Get(ctx, cacheKey).Result(); err == nil {
			var e tokenCacheEntry
			if json.Unmarshal([]byte(raw), &e) == nil {
				return &TokenInfo{
					ID: e.ID, UserID: e.UserID, Quota: e.Quota,
					Models: e.Models, IPLimits: e.IPLimits, ExpiredAt: e.ExpiredAt,
				}, nil
			}
		}
	}

	t, err := model.GetTokenByHash(db, keyHash)
	if err != nil {
		return nil, err
	}

	info := &TokenInfo{
		ID: t.ID, UserID: t.UserID, Quota: t.Quota,
		ExpiredAt: t.ExpiredAt, IPLimits: splitCSV(t.IPLimit),
	}
	if t.Models != nil {
		_ = json.Unmarshal([]byte(*t.Models), &info.Models)
	}

	if rdb != nil && cacheSec > 0 {
		entry := tokenCacheEntry{
			ID: t.ID, UserID: t.UserID, Quota: t.Quota,
			Models: info.Models, IPLimits: info.IPLimits, ExpiredAt: t.ExpiredAt,
		}
		if raw, err := json.Marshal(entry); err == nil {
			_ = rdb.Set(ctx, cacheKey, raw, time.Duration(cacheSec)*time.Second).Err()
		}
	}
	return info, nil
}

func extractBearer(r *http.Request) string {
	auth := r.Header.Get("Authorization")
	if auth == "" {
		return ""
	}
	const prefix = "Bearer "
	if len(auth) > len(prefix) && subtle.ConstantTimeCompare([]byte(auth[:len(prefix)]), []byte(prefix)) == 1 {
		return auth[len(prefix):]
	}
	return ""
}

func splitCSV(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func ipAllowed(ip string, allowlist []string) bool {
	if len(allowlist) == 0 {
		return true
	}
	parsed := net.ParseIP(ip)
	if parsed == nil {
		return false
	}
	for _, a := range allowlist {
		if ip == a {
			return true
		}
		if _, cidr, err := net.ParseCIDR(a); err == nil && cidr.Contains(parsed) {
			return true
		}
	}
	return false
}

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// CheckModelWhitelist 令牌模型白名单校验，handler 解析出 model 后调用
func (t *TokenInfo) CheckModelWhitelist(requestModel string) bool {
	if len(t.Models) == 0 {
		return true
	}
	for _, m := range t.Models {
		if m == requestModel {
			return true
		}
	}
	return false
}
