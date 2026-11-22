package deploy

// asset_model.go：S5 软件资产画像（原 internal/asset，批次 G 并入 deploy 域）。

import (
	"time"

	"gorm.io/gorm"
)

// SoftwareAsset 是软件资产画像（对应表 software_assets）。
// kind：go-service | python-worker | vue-app | react-app | docker-app | cli。
// start_method：compose（统一 compose 服务）| dynamic（版本容器，S5 §3.4）| external（不纳管）。
// JSON 列（ports / interfaces / profile）以 JSON 字符串存储，空值归一为合法 JSON。
type SoftwareAsset struct {
	ID          uint      `gorm:"primaryKey" json:"id"`
	Name        string    `gorm:"size:128;uniqueIndex;not null" json:"name"`
	Path        string    `gorm:"size:255;not null" json:"path"`
	Kind        string    `gorm:"size:32;not null" json:"kind"`
	Language    string    `gorm:"size:32" json:"language"`
	Ports       string    `gorm:"type:json" json:"ports"`
	StartMethod string    `gorm:"size:32;not null" json:"start_method"`
	Service     string    `gorm:"size:128" json:"service"`
	Interfaces  string    `gorm:"type:json" json:"interfaces"`
	Owner       string    `gorm:"size:64;not null" json:"owner"`
	Profile     string    `gorm:"type:json;not null" json:"profile"`
	AutoManaged int8      `gorm:"not null;default:0" json:"auto_managed"`
	UpdatedAt   time.Time `json:"updated_at"`
}

func (SoftwareAsset) TableName() string { return "software_assets" }

// BeforeSave 把空的 JSON 列归一为合法 JSON（MySQL 的 JSON 列不接受空串）。
func (a *SoftwareAsset) BeforeSave(*gorm.DB) error {
	if a.Ports == "" {
		a.Ports = "[]"
	}
	if a.Interfaces == "" {
		a.Interfaces = "[]"
	}
	if a.Profile == "" {
		a.Profile = "{}"
	}
	return nil
}

// DeployPlan 是依赖序部署计划（对应表 deploy_plans，S5 §3.2）。
// plan 为 JSON 数组：[{target, kind, depends_on, health, action}]。
type DeployPlan struct {
	ID         uint       `gorm:"primaryKey" json:"id"`
	Name       string     `gorm:"size:128;not null" json:"name"`
	Plan       string     `gorm:"type:json;not null" json:"plan"`
	Status     string     `gorm:"size:32;not null;default:idle" json:"status"`
	LastRunAt  *time.Time `json:"last_run_at"`
	LastResult string     `gorm:"type:json" json:"last_result"`
	CreatedAt  time.Time  `json:"created_at"`
}

func (DeployPlan) TableName() string { return "deploy_plans" }

// BeforeSave 把空的 JSON 列归一为合法 JSON。
func (p *DeployPlan) BeforeSave(*gorm.DB) error {
	if p.Plan == "" {
		p.Plan = "[]"
	}
	if p.LastResult == "" {
		p.LastResult = "null"
	}
	return nil
}
