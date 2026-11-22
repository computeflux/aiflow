package service

import (
	"encoding/json"
	"log"
	"time"

	"github.com/computeflux/auth/model"
	"gorm.io/gorm"
)

// Audit 审计写入（异步批量：channel 缓冲，后台每 2s 或满 100 条落库）。
type Audit struct {
	db *gorm.DB
	ch chan model.AuditLog
}

// NewAudit 创建审计写入器并启动后台 flush goroutine。
func NewAudit(db *gorm.DB) *Audit {
	a := &Audit{db: db, ch: make(chan model.AuditLog, 512)}
	go a.flushLoop()
	return a
}

// Log 异步记录一条审计（detail 序列化为 JSON；channel 满时丢弃并打日志，不阻塞请求）。
func (a *Audit) Log(userID, action string, detail any, ip string) {
	var detailJSON string
	if detail != nil {
		if b, err := json.Marshal(detail); err == nil {
			detailJSON = string(b)
		}
	}
	entry := model.AuditLog{
		UserID:    userID,
		Action:    action,
		Detail:    detailJSON,
		IP:        ip,
		CreatedAt: time.Now(),
	}
	select {
	case a.ch <- entry:
	default:
		log.Printf("[auth-audit] channel full, dropping audit %s/%s", userID, action)
	}
}

func (a *Audit) flushLoop() {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	var batch []model.AuditLog
	flush := func() {
		if len(batch) == 0 {
			return
		}
		if err := a.db.Create(&batch).Error; err != nil {
			log.Printf("[auth-audit] flush failed: %v", err)
		}
		batch = nil
	}
	for {
		select {
		case e := <-a.ch:
			batch = append(batch, e)
			if len(batch) >= 100 {
				flush()
			}
		case <-ticker.C:
			flush()
		}
	}
}
