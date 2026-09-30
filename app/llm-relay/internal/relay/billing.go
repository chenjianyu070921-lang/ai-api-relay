package relay

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/redis/go-redis/v9"
	"gorm.io/gorm"

	"llm-relay/app/llm-relay/internal/model"
)

// 计费实现对齐 01-架构设计.md 3.4：
//   预扣费 TryReserve —— Redis Lua 原子扣减，防并发超扣；
//   结算   ApplyDelta —— 一次 HINCRBY 完成 返还/补扣 + 累计已用；
//   落库   ApplyQuotaDelta —— DB 是最终对账依据，异步增量 flush。
// 缓存 key：lr:userquota:{user_id} / lr:tokenquota:{token_id}（02 文档第 8 节）。
// Schema 版本校验防止脏缓存误扣：管理员调整配额时递增 Schema，旧缓存自动失效回源。

const (
	userQuotaCachePrefix  = "lr:userquota:"
	tokenQuotaCachePrefix = "lr:tokenquota:"
	quotaSchema           = "v1" // P3 管理后台调整配额时需要 bump 并删旧 key
)

// userReserveScript TryReserve：
//
//	返回  1 扣减成功
//	返回  0 余额不足
//	返回 -1 缓存脏/不存在（调用方回源 DB 重建后重试一次）
//
// ARGV: 1=冻结量 2=用户ID 3=Schema
const reserveScript = `
if redis.call('HGET', KEYS[1], 'Id') ~= ARGV[2]
  or redis.call('HGET', KEYS[1], 'Schema') ~= ARGV[3]
  or redis.call('HGET', KEYS[1], 'Quota') == false then
  return -1
end
local quota = tonumber(redis.call('HGET', KEYS[1], 'Quota'))
if quota < tonumber(ARGV[1]) then
  return 0
end
redis.call('HINCRBY', KEYS[1], 'Quota', -tonumber(ARGV[1]))
return 1`

// deltaScript ApplyDelta：ARGV 1=配额增量(正=返还/负=补扣) 2=已用增量 3=ID 4=Schema
const deltaScript = `
if redis.call('HGET', KEYS[1], 'Id') ~= ARGV[3]
  or redis.call('HGET', KEYS[1], 'Schema') ~= ARGV[4]
  or redis.call('HGET', KEYS[1], 'Quota') == false then
  return -1
end
redis.call('HINCRBY', KEYS[1], 'Quota', ARGV[1])
redis.call('HINCRBY', KEYS[1], 'UsedQuota', ARGV[2])
return 1`

var ErrInsufficientQuota = errors.New("insufficient quota")

type Billing struct {
	db       *gorm.DB
	rdb      redis.UniversalClient
	cacheTTL time.Duration
}

func NewBilling(db *gorm.DB, rdb redis.UniversalClient, quotaCacheSec int) *Billing {
	if quotaCacheSec <= 0 {
		quotaCacheSec = 604800 // 默认 7d
	}
	return &Billing{db: db, rdb: rdb, cacheTTL: time.Duration(quotaCacheSec) * time.Second}
}

// CalcCost 实际费用 = (prompt×输入价 + completion×输出价) / 1M × 分组倍率
// pricing 为 nil（没配定价）时费用记 0，usage 照常入日志
func CalcCost(p *model.ModelPricing, promptTokens, completionTokens int, groupRatio float64) int64 {
	if p == nil {
		return 0
	}
	if groupRatio <= 0 {
		groupRatio = 1.0
	}
	raw := float64(promptTokens)*float64(p.InputPerM) + float64(completionTokens)*float64(p.OutputPerM)
	return int64(math.Round(raw / 1e6 * groupRatio))
}

// CalcFreeze 预扣金额 = (prompt估算×输入价 + maxTokens×输出价) / 1M × 分组倍率
func CalcFreeze(p *model.ModelPricing, promptEstimate, maxTokens int, groupRatio float64) int64 {
	if p == nil {
		return 0
	}
	if groupRatio <= 0 {
		groupRatio = 1.0
	}
	raw := float64(promptEstimate)*float64(p.InputPerM) + float64(maxTokens)*float64(p.OutputPerM)
	return int64(math.Round(raw / 1e6 * groupRatio))
}

// EstimateTokens 字符数/4 的粗估（对齐 01 文档 3.4⑤，不追求精确够对账即可）
func EstimateTokens(body []byte) int {
	return len(body) / 4
}

// Reserve 预扣费：user 级必扣；token 级独立配额（quota>=0）也扣。
// 任一级失败则整体失败（已扣的一级自动返还）。
func (b *Billing) Reserve(ctx context.Context, user *model.User, token *TokenInfo, freeze int64) error {
	if freeze <= 0 {
		return nil // 没配定价的模型暂不计费，直接放行（usage 照记）
	}
	if err := b.reserveOne(ctx, userQuotaCachePrefix+fmt.Sprint(user.ID), freeze, user.ID, user.Quota); err != nil {
		return err
	}
	if token != nil && token.Quota >= 0 {
		if err := b.reserveOne(ctx, tokenQuotaCachePrefix+fmt.Sprint(token.ID), freeze, token.ID, token.Quota); err != nil {
			// token 级失败，返还 user 级
			b.applyDelta(ctx, userQuotaCachePrefix+fmt.Sprint(user.ID), freeze, 0, user.ID)
			return err
		}
	}
	return nil
}

func (b *Billing) reserveOne(ctx context.Context, cacheKey string, freeze, ownerID, dbQuota int64) error {
	res, err := b.evalReserve(ctx, cacheKey, freeze, ownerID)
	if err == errCacheDirty && b.db != nil {
		// 回源 DB 重建缓存后重试一次（对齐 01 文档 3.4②）
		if rebuildErr := b.rebuildCache(ctx, cacheKey, ownerID, dbQuota); rebuildErr == nil {
			res, err = b.evalReserve(ctx, cacheKey, freeze, ownerID)
		}
	}
	if err != nil {
		return err
	}
	if res == 0 {
		return ErrInsufficientQuota
	}
	return nil
}

func (b *Billing) evalReserve(ctx context.Context, cacheKey string, freeze, ownerID int64) (int64, error) {
	if b.rdb == nil {
		return 1, nil // 无 Redis 时降级为不限流（仅开发环境），靠 DB 事后对账
	}
	res, err := b.rdb.Eval(ctx, reserveScript, []string{cacheKey},
		freeze, fmt.Sprint(ownerID), quotaSchema).Int64()
	if err != nil {
		return 0, err
	}
	if res == -1 {
		return 0, errCacheDirty
	}
	return res, nil
}

var errCacheDirty = errors.New("quota cache dirty")

// rebuildCache 从 DB 读最新配额重建缓存
func (b *Billing) rebuildCache(ctx context.Context, cacheKey string, ownerID, _ int64) error {
	var quota, used int64
	switch {
	case len(cacheKey) > len(userQuotaCachePrefix) && cacheKey[:len(userQuotaCachePrefix)] == userQuotaCachePrefix:
		u, err := model.GetUser(b.db, ownerID)
		if err != nil {
			return err
		}
		quota, used = u.Quota, u.UsedQuota
	default: // token 级
		var t model.Token
		if err := b.db.First(&t, ownerID).Error; err != nil {
			return err
		}
		quota, used = t.Quota, t.UsedQuota
	}
	if err := b.rdb.HSet(ctx, cacheKey, map[string]any{
		"Id": fmt.Sprint(ownerID), "Schema": quotaSchema, "Quota": quota, "UsedQuota": used,
	}).Err(); err != nil {
		return err
	}
	return b.rdb.Expire(ctx, cacheKey, b.cacheTTL).Err()
}

// Settle 结算：delta = freeze - cost（正=返还 负=补扣），缓存与 DB 异步落库
func (b *Billing) Settle(ctx context.Context, user *model.User, token *TokenInfo, freeze, cost int64) {
	delta := freeze - cost
	if freeze <= 0 && cost <= 0 {
		return
	}
	// 结算必须完成：客户端断连后请求 ctx 已取消，Redis Eval 会直接失败，
	// 导致冻结量永久卡死在缓存。这里统一换独立 context（DB/Redis 同理）。
	bg := context.Background()
	userKey := userQuotaCachePrefix + fmt.Sprint(user.ID)
	b.applyDelta(bg, userKey, delta, cost, user.ID)

	if token != nil && token.Quota >= 0 {
		tokenKey := tokenQuotaCachePrefix + fmt.Sprint(token.ID)
		b.applyDelta(bg, tokenKey, delta, cost, token.ID)
	}

	// DB 增量落库（P0 每笔异步直写；P3 换内存聚合批量 flush，见 01 文档 3.4④）
	// 注意：DB 侧从没扣过冻结量（冻结只存在于 Redis），所以一笔请求对 DB 的
	// 净效果是 quota -= cost / used += cost，而不是缓存侧的 delta=freeze-cost
	if b.db != nil {
		go func() {
			_ = model.ApplyQuotaDelta(b.db, user.ID, -cost, cost)
			if token != nil && token.Quota >= 0 {
				_ = model.ApplyTokenQuotaDelta(b.db, token.ID, -cost, cost)
			}
		}()
	}
}

func (b *Billing) applyDelta(ctx context.Context, cacheKey string, quotaDelta, usedDelta, ownerID int64) {
	if b.rdb == nil {
		return
	}
	_ = b.rdb.Eval(ctx, deltaScript, []string{cacheKey},
		quotaDelta, usedDelta, fmt.Sprint(ownerID), quotaSchema).Err()
}
