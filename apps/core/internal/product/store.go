// Package product 是 S7 产品情报（docs/plan/S7-产品情报.md）：
// 产品信息自动汇聚进 RAG（分层授权 + secret 红线）、产品 Agent 对话并生成
// 产品文档/变更需求（product 角色，业务智能体，不写代码）、对话/会议自动拆解为任务卡
// （分流：skill/app → ready_for_evolve；core → 仅人工 PR 建议单）。
// 只读现有数据 + 只写 product_docs / knowledge_docs / board 卡 / Qdrant，不触碰运行链路。
package product

import (
	"context"
	"fmt"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// Store 是 product_docs / knowledge_docs 两表访问层。直接取 *gorm.DB
// （不走 internal/db 的重 config：knowledge-sync 只需 MySQL + Qdrant + LLM）。
type Store struct {
	database *gorm.DB
}

// NewStore 构造产品情报域（product_docs / knowledge_docs）表访问层。
func NewStore(database *gorm.DB) *Store { return &Store{database: database} }

// ProductDoc 是产品 Agent 生成的文档 / 变更需求（对应表 product_docs，S7 §3.2）。
// doc_type：product_doc | change_request | meeting_minutes；
// sources 为可溯源来源（JSON 数组，RAG 命中文档 + 时间戳）；card_id 为拆解后回填的任务卡。
type ProductDoc struct {
	ID        uint      `gorm:"primaryKey" json:"id"`
	BoardID   uint      `gorm:"not null" json:"board_id"`
	Title     string    `gorm:"size:255;not null" json:"title"`
	DocType   string    `gorm:"size:32;not null" json:"doc_type"`
	Content   string    `gorm:"type:longtext;not null" json:"content"`
	Sources   string    `gorm:"type:json" json:"sources"`
	CardID    *uint     `json:"card_id"`
	Status    string    `gorm:"size:32;not null;default:draft" json:"status"` // draft | accepted | rejected
	CreatedAt time.Time `json:"created_at"`
}

func (ProductDoc) TableName() string { return "product_docs" }

// BeforeSave 把空的 sources JSON 列归一为合法 JSON（MySQL JSON 列不接受空串）。
func (d *ProductDoc) BeforeSave(*gorm.DB) error {
	if d.Sources == "" {
		d.Sources = "[]"
	}
	if d.Status == "" {
		d.Status = "draft"
	}
	return nil
}

// KnowledgeDoc 是 knowledge-sync 增量入库的去重基线（对应表 knowledge_docs，S7 §3.1）。
// 按 path + content_hash 做 diff：内容未变则跳过，变更则重新入库。
type KnowledgeDoc struct {
	ID              uint      `gorm:"primaryKey" json:"id"`
	Path            string    `gorm:"size:512;not null;uniqueIndex" json:"path"`
	ContentHash     string    `gorm:"size:64;not null" json:"content_hash"`
	SourceType      string    `gorm:"size:32;not null" json:"source_type"`
	Confidentiality string    `gorm:"size:16;not null;default:internal" json:"confidentiality"`
	Title           string    `gorm:"size:255;not null" json:"title"`
	IngestedAt      time.Time `json:"ingested_at"`
}

func (KnowledgeDoc) TableName() string { return "knowledge_docs" }

// ensureTables 两表 DDL：与 apps/core/hacks/sql/product_docs.sql / knowledge_docs.sql 保持一致。
var ensureTables = []string{
	`CREATE TABLE IF NOT EXISTS product_docs (
  id BIGINT AUTO_INCREMENT PRIMARY KEY,
  board_id BIGINT NOT NULL,
  title VARCHAR(255) NOT NULL,
  doc_type VARCHAR(32) NOT NULL,
  content LONGTEXT NOT NULL,
  sources JSON,
  card_id BIGINT,
  status VARCHAR(32) NOT NULL DEFAULT 'draft',
  created_at DATETIME DEFAULT CURRENT_TIMESTAMP
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
	`CREATE TABLE IF NOT EXISTS knowledge_docs (
  id BIGINT AUTO_INCREMENT PRIMARY KEY,
  path VARCHAR(512) NOT NULL UNIQUE,
  content_hash CHAR(64) NOT NULL,
  source_type VARCHAR(32) NOT NULL,
  confidentiality VARCHAR(16) NOT NULL DEFAULT 'internal',
  title VARCHAR(255) NOT NULL,
  ingested_at DATETIME DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
}

// EnsureSchema 建表。幂等，可在进程启动时调用。
func (s *Store) EnsureSchema() error {
	for _, ddl := range ensureTables {
		if err := s.database.Exec(ddl).Error; err != nil {
			return fmt.Errorf("ensure product table: %w", err)
		}
	}
	return nil
}

// ---- product_docs ----

// CreateDoc 新建产品文档。
func (s *Store) CreateDoc(ctx context.Context, d *ProductDoc) error {
	return s.database.WithContext(ctx).Create(d).Error
}

// ListDocs 列出某 board 的产品文档；status 为空 = 全部。按创建时间倒序。
func (s *Store) ListDocs(ctx context.Context, boardID uint, status string) ([]ProductDoc, error) {
	q := s.database.WithContext(ctx).Where("board_id = ?", boardID).Order("id desc")
	if status != "" {
		q = q.Where("status = ?", status)
	}
	var items []ProductDoc
	if err := q.Find(&items).Error; err != nil {
		return nil, err
	}
	return items, nil
}

// Doc 按 id 取产品文档。
func (s *Store) Doc(ctx context.Context, id uint) (*ProductDoc, error) {
	var d ProductDoc
	if err := s.database.WithContext(ctx).First(&d, id).Error; err != nil {
		return nil, err
	}
	return &d, nil
}

// UpdateDocStatus 更新产品文档状态（draft/accepted/rejected）。
func (s *Store) UpdateDocStatus(ctx context.Context, id uint, status string) error {
	res := s.database.WithContext(ctx).Model(&ProductDoc{}).Where("id = ?", id).Update("status", status)
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("product doc %d not found", id)
	}
	return nil
}

// UpdateDocContent 更新产品文档正文（draft 可编辑）。
func (s *Store) UpdateDocContent(ctx context.Context, id uint, content string) error {
	return s.database.WithContext(ctx).Model(&ProductDoc{}).Where("id = ?", id).Update("content", content).Error
}

// LinkCard 回填产品文档关联的任务卡 id（拆解后）。
func (s *Store) LinkCard(ctx context.Context, docID, cardID uint) error {
	return s.database.WithContext(ctx).Model(&ProductDoc{}).Where("id = ?", docID).Update("card_id", cardID).Error
}

// ---- knowledge_docs ----

// KnowledgeDoc 按 path 取去重基线（不存在 → gorm.ErrRecordNotFound）。
func (s *Store) KnowledgeDoc(ctx context.Context, path string) (*KnowledgeDoc, error) {
	var d KnowledgeDoc
	if err := s.database.WithContext(ctx).Where("path = ?", path).First(&d).Error; err != nil {
		return nil, err
	}
	return &d, nil
}

// UpsertKnowledgeDoc 按 path 插入或刷新去重基线。
func (s *Store) UpsertKnowledgeDoc(ctx context.Context, d *KnowledgeDoc) error {
	d.IngestedAt = time.Now()
	return s.database.WithContext(ctx).Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "path"}},
		DoUpdates: clause.AssignmentColumns([]string{
			"content_hash", "source_type", "confidentiality", "title", "ingested_at",
		}),
	}).Create(d).Error
}
