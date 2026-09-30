package model

import (
	"time"

	"github.com/zeromicro/go-zero/core/logx"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// OpenDB 建立 gorm 连接。连接串形如：
// root:pass@tcp(127.0.0.1:13306)/llm_relay?charset=utf8mb4&parseTime=true&loc=Local
func OpenDB(dsn string) (*gorm.DB, error) {
	db, err := gorm.Open(mysql.Open(dsn), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Warn), // 慢查询/错误才打日志
	})
	if err != nil {
		return nil, err
	}
	sqlDB, err := db.DB()
	if err != nil {
		return nil, err
	}
	// 中转站是 IO 密集转发型服务，连接池给到够用即可，避免挤占上游连接
	sqlDB.SetMaxOpenConns(50)
	sqlDB.SetMaxIdleConns(10)
	sqlDB.SetConnMaxLifetime(time.Hour)
	return db, nil
}

// Token 虚拟令牌（lr_token），只存 sha256 不存明文
type Token struct {
	ID        int64      `gorm:"column:id;primaryKey"`
	UserID    int64      `gorm:"column:user_id"`
	Name      string     `gorm:"column:name"`
	KeyHash   string     `gorm:"column:key_hash"`
	KeyPrefix string     `gorm:"column:key_prefix"`
	Quota     int64      `gorm:"column:quota"` // -1 = 不限（受用户配额约束）
	UsedQuota int64      `gorm:"column:used_quota"`
	Models    *string    `gorm:"column:models"` // JSON 数组，NULL = 不限
	ExpiredAt *time.Time `gorm:"column:expired_at"`
	IPLimit   string     `gorm:"column:ip_limit"`
	RPMLimit  int        `gorm:"column:rpm_limit"`
	Status    int8       `gorm:"column:status"`
	CreatedAt time.Time  `gorm:"column:created_at"`
}

func (Token) TableName() string { return "lr_token" }

// User 用户（lr_user）。配额只在 Redis 原子流转，DB 是最终对账依据。
type User struct {
	ID           int64     `gorm:"column:id;primaryKey"`
	Username     string    `gorm:"column:username"`
	PasswordHash string    `gorm:"column:password_hash"`
	Role         int8      `gorm:"column:role"`
	GroupID      *int64    `gorm:"column:group_id"`
	Quota        int64     `gorm:"column:quota"`
	UsedQuota    int64     `gorm:"column:used_quota"`
	Status       int8      `gorm:"column:status"`
	CreatedAt    time.Time `gorm:"column:created_at"`
}

func (User) TableName() string { return "lr_user" }

// UserGroup 用户组计费倍率（lr_user_group）
type UserGroup struct {
	ID      int64   `gorm:"column:id;primaryKey"`
	Name    string  `gorm:"column:name"`
	Ratio   float64 `gorm:"column:ratio"`
	Enabled int8    `gorm:"column:enabled"`
}

func (UserGroup) TableName() string { return "lr_user_group" }

// ModelPricing 定价（lr_model_pricing），每 1M token 单价，quota 整数
type ModelPricing struct {
	ID              int64     `gorm:"column:id;primaryKey"`
	ModelName       string    `gorm:"column:model_name"`
	InputPerM       int64     `gorm:"column:input_per_m"`
	OutputPerM      int64     `gorm:"column:output_per_m"`
	CachedInputPerM int64     `gorm:"column:cached_input_per_m"`
	UpdatedAt       time.Time `gorm:"column:updated_at"`
}

func (ModelPricing) TableName() string { return "lr_model_pricing" }

type ModelMapping struct {
	ID         int64  `gorm:"column:id;primaryKey"`
	PublicName string `gorm:"column:public_name"`
	ChannelID  int64  `gorm:"column:channel_id"`
	RealName   string `gorm:"column:real_name"`
	Enabled    int8   `gorm:"column:enabled"`
}

func (ModelMapping) TableName() string { return "lr_model_mapping" }

// RelayLog 请求日志（lr_relay_log），走异步批量写入，见 logwriter.go
type RelayLog struct {
	ID               int64     `gorm:"column:id;primaryKey"`
	TraceID          string    `gorm:"column:trace_id"`
	UserID           int64     `gorm:"column:user_id"`
	TokenID          int64     `gorm:"column:token_id"`
	ChannelID        *int64    `gorm:"column:channel_id"`
	ModelRequest     string    `gorm:"column:model_request"`
	ModelReal        string    `gorm:"column:model_real"`
	PromptTokens     int       `gorm:"column:prompt_tokens"`
	CompletionTokens int       `gorm:"column:completion_tokens"`
	UsageEstimated   int8      `gorm:"column:usage_estimated"`
	QuotaCost        int64     `gorm:"column:quota_cost"`
	DurationMs       int       `gorm:"column:duration_ms"`
	FirstByteMs      int       `gorm:"column:first_byte_ms"`
	Stream           int8      `gorm:"column:stream"`
	Status           int8      `gorm:"column:status"` // 1成功 2上游失败 3限流拒绝 4余额不足 5鉴权失败
	HTTPStatus       int       `gorm:"column:http_status"`
	ErrorMsg         string    `gorm:"column:error_msg"`
	ClientIP         string    `gorm:"column:client_ip"`
	CreatedAt        time.Time `gorm:"column:created_at"`
}

func (RelayLog) TableName() string { return "lr_relay_log" }

// GetTokenByHash 鉴权主查询：sha256(key) → 令牌
func GetTokenByHash(db *gorm.DB, keyHash string) (*Token, error) {
	var t Token
	err := db.Where("key_hash = ? AND status = 1", keyHash).First(&t).Error
	if err != nil {
		return nil, err
	}
	return &t, nil
}

// GetUser 按主键查用户
func GetUser(db *gorm.DB, id int64) (*User, error) {
	var u User
	err := db.First(&u, id).Error
	if err != nil {
		return nil, err
	}
	return &u, nil
}

// GetPricing 查模型定价；查不到返回 (nil, nil)，由调用方走估算兜底
func GetPricing(db *gorm.DB, modelName string) (*ModelPricing, error) {
	var p ModelPricing
	err := db.Where("model_name = ?", modelName).First(&p).Error
	if err != nil {
		if err == gorm.ErrRecordNotFound {
			return nil, nil
		}
		return nil, err
	}
	return &p, nil
}

// GetModelMapping 查模型映射（对外名 → 渠道真实名）。
// P1 单渠道阶段取第一条 enabled 映射（channel_id 路由语义 P2 随 ability 表收紧）；
// 查不到返回 (nil, nil)，调用方回退到请求原名/配置覆盖。
func GetModelMapping(db *gorm.DB, publicName string) (*ModelMapping, error) {
	var m ModelMapping
	err := db.Where("public_name = ? AND enabled = 1", publicName).First(&m).Error
	if err != nil {
		if err == gorm.ErrRecordNotFound {
			return nil, nil
		}
		return nil, err
	}
	return &m, nil
}

// ApplyQuotaDelta 配额增量落库（Redis 结算后的异步 flush，P0 每笔直接异步执行；
// P3 换成内存聚合 + 批量 flush，见 01-架构设计.md 3.4④）
func ApplyQuotaDelta(db *gorm.DB, userID int64, quotaDelta, usedDelta int64) error {
	err := db.Model(&User{}).Where("id = ?", userID).
		Updates(map[string]any{
			"quota":      gorm.Expr("quota + ?", quotaDelta),
			"used_quota": gorm.Expr("used_quota + ?", usedDelta),
		}).Error
	if err != nil {
		logx.Errorf("flush user quota delta failed, user_id=%d delta=%d: %v", userID, quotaDelta, err)
	}
	return err
}

// ApplyTokenQuotaDelta 令牌级配额增量落库（quota=-1 的令牌不调用）
func ApplyTokenQuotaDelta(db *gorm.DB, tokenID int64, quotaDelta, usedDelta int64) error {
	return db.Model(&Token{}).Where("id = ?", tokenID).
		Updates(map[string]any{
			"quota":      gorm.Expr("quota + ?", quotaDelta),
			"used_quota": gorm.Expr("used_quota + ?", usedDelta),
		}).Error
}
