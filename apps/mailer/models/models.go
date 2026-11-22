package models

import (
	"fmt"
	"time"

	"github.com/computeflux/mailer/config"

	mysqldriver "github.com/go-sql-driver/mysql"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

type DB struct {
	*gorm.DB
}

const (
	StatusPending   = "pending"
	StatusApproved  = "approved"
	StatusRejected  = "rejected"
	StatusPublished = "published"
	StatusFailed    = "failed"
	StatusQueued    = "queued"
	StatusSending   = "sending"
	StatusSkipped   = "skipped"
)

type EmailBatch struct {
	ID            uint       `gorm:"primaryKey" json:"id"`
	OrgName       string     `gorm:"index;not null" json:"org_name"`
	Status        string     `gorm:"index;not null;default:pending" json:"status"`
	WorkflowID    string     `json:"workflow_id"`
	Total         int        `json:"total"`
	Sent          int        `json:"sent"`
	Analysis      string     `gorm:"type:text" json:"analysis"`
	ErrorMsg      string     `gorm:"type:text" json:"error_msg"`
	CcList        string     `gorm:"type:text" json:"cc_list"`
	CustomSubject string     `gorm:"type:text" json:"custom_subject"`
	CustomContent string     `gorm:"type:text" json:"custom_content"`
	EmailSenderID *uint      `gorm:"index" json:"email_sender_id"`
	CreatedAt     time.Time  `json:"created_at"`
	ApprovedAt    *time.Time `json:"approved_at"`
	SentAt        *time.Time `json:"sent_at"`
}

func (EmailBatch) TableName() string { return "email_batches" }

type EmailRecipient struct {
	ID        uint       `gorm:"primaryKey" json:"id"`
	BatchID   uint       `gorm:"index;not null" json:"batch_id"`
	Email     string     `gorm:"index;not null" json:"email"`
	Name      string     `json:"name"`
	Repo      string     `json:"repo"`
	Reason    string     `json:"reason"`
	Selected  bool       `gorm:"default:true" json:"selected"`
	Status    string     `gorm:"default:pending" json:"status"`
	SentAt    *time.Time `json:"sent_at"`
	ErrorMsg  string     `json:"error_msg"`
	CreatedAt time.Time  `json:"created_at"`
}

func (EmailRecipient) TableName() string { return "email_recipients" }

type EmailSendLog struct {
	ID        uint      `gorm:"primaryKey" json:"id"`
	Email     string    `gorm:"uniqueIndex;not null;type:varchar(320)" json:"email"`
	Count     int       `gorm:"default:0" json:"count"`
	LastSent  time.Time `json:"last_sent"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

func (EmailSendLog) TableName() string { return "email_send_logs" }

type EmailTemplate struct {
	ID          uint      `gorm:"primaryKey" json:"id"`
	Name        string    `gorm:"not null" json:"name"`
	Subject     string    `gorm:"not null" json:"subject"`
	Body        string    `gorm:"type:text;not null" json:"body"`
	ContentType string    `gorm:"default:text" json:"content_type"`
	IsDefault   bool      `gorm:"default:false" json:"is_default"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

func (EmailTemplate) TableName() string { return "email_templates" }

type EmailSender struct {
	ID          uint       `gorm:"primaryKey" json:"id"`
	Name        string     `gorm:"size:128;not null" json:"name"`
	Host        string     `gorm:"size:255;not null" json:"host"`
	Port        int        `gorm:"default:465" json:"port"`
	User        string     `gorm:"size:255;not null" json:"user"`
	Password    string     `gorm:"size:512;not null" json:"password"`
	FromAddress string     `gorm:"size:320;not null" json:"from_address"`
	FromName    string     `gorm:"size:128" json:"from_name"`
	DailyLimit  int        `gorm:"default:0" json:"daily_limit"`
	UsedToday   int        `gorm:"default:0" json:"used_today"`
	LastUsedAt  *time.Time `json:"last_used_at"`
	Enabled     bool       `gorm:"default:true" json:"enabled"`
	IsDefault   bool       `gorm:"default:false" json:"is_default"`
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
}

func (EmailSender) TableName() string { return "email_senders" }

type ErrorLog struct {
	ID         uint       `gorm:"primaryKey" json:"id"`
	System     string     `gorm:"size:64;index;not null" json:"system"`
	Level      string     `gorm:"size:16;index;default:error" json:"level"`
	Source     string     `gorm:"size:128;index" json:"source"`
	WorkflowID string     `gorm:"size:255;index" json:"workflow_id"`
	RunID      string     `gorm:"size:255" json:"run_id"`
	TraceID    string     `gorm:"size:64;index;default:''" json:"trace_id"`
	Message    string     `gorm:"type:text;not null" json:"message"`
	Stack      string     `gorm:"type:longtext" json:"stack"`
	Context    string     `gorm:"type:longtext" json:"context"`
	Resolved   bool       `gorm:"index;default:false" json:"resolved"`
	CreatedAt  time.Time  `gorm:"index" json:"created_at"`
	ResolvedAt *time.Time `json:"resolved_at"`
}

func (ErrorLog) TableName() string { return "error_logs" }

func Open(cfg *config.Config) (*DB, error) {
	mysqlCfg := mysqldriver.Config{
		User:         cfg.MySQLUser,
		Passwd:       cfg.MySQLPassword,
		Net:          "tcp",
		Addr:         fmt.Sprintf("%s:%s", cfg.MySQLHost, cfg.MySQLPort),
		DBName:       cfg.MySQLDatabase,
		Params:       map[string]string{"charset": "utf8mb4"},
		ParseTime:    true,
		Timeout:      5 * time.Second,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 30 * time.Second,
	}
	gormDB, err := gorm.Open(mysql.Open(mysqlCfg.FormatDSN()), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		return nil, fmt.Errorf("open mysql: %w", err)
	}

	sqlDB, err := gormDB.DB()
	if err != nil {
		return nil, fmt.Errorf("get sql db: %w", err)
	}
	sqlDB.SetMaxOpenConns(20)
	sqlDB.SetMaxIdleConns(5)
	sqlDB.SetConnMaxLifetime(3 * time.Minute)

	if err := gormDB.AutoMigrate(
		&EmailBatch{},
		&EmailRecipient{},
		&EmailSendLog{},
		&EmailTemplate{},
		&EmailSender{},
		&ErrorLog{},
	); err != nil {
		return nil, fmt.Errorf("migrate db: %w", err)
	}

	return &DB{gormDB}, nil
}
