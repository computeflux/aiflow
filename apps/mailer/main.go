package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"math/rand"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/computeflux/mailer/config"
	"github.com/computeflux/mailer/errorsink"
	"github.com/computeflux/mailer/mailer"
	"github.com/computeflux/mailer/models"

	"github.com/gin-contrib/cors"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"go.temporal.io/sdk/client"
	"gorm.io/gorm"
)

func main() {
	cfg := config.Load()
	database, err := models.Open(cfg)
	if err != nil {
		log.Fatalf("open db: %v", err)
	}

	// 初始化：如果数据库中尚无发信配置且环境变量中配有 SMTP，自动导入一条默认发信配置
	var senderCount int64
	if err := database.Model(&models.EmailSender{}).Count(&senderCount).Error; err == nil && senderCount == 0 {
		if cfg.SMTPHost != "" && cfg.SMTPUser != "" {
			initialSender := models.EmailSender{
				Name:        "Default SMTP (" + cfg.SMTPUser + ")",
				Host:        cfg.SMTPHost,
				Port:        cfg.SMTPPort,
				User:        cfg.SMTPUser,
				Password:    cfg.SMTPPassword,
				FromAddress: cfg.SMTPFrom,
				FromName:    "ComputeFlux",
				DailyLimit:  0,
				Enabled:     true,
				IsDefault:   true,
			}
			if err := database.Create(&initialSender).Error; err != nil {
				log.Printf("[mailer-init] seed default sender failed: %v", err)
			} else {
				log.Printf("[mailer-init] seeded default sender from env: %s", initialSender.FromAddress)
			}
		}
	}

	var (
		temporalMu     sync.Mutex
		temporalClient client.Client
	)
	getTemporal := func() client.Client {
		temporalMu.Lock()
		defer temporalMu.Unlock()
		return temporalClient
	}
	dialTemporal := func() bool {
		tc, err := client.Dial(client.Options{
			HostPort:  cfg.TemporalHostPort,
			Namespace: cfg.TemporalNamespace,
		})
		if err != nil {
			log.Printf("[mailer-service] temporal dial note: %v (workflow trigger disabled, will retry)", err)
			return false
		}
		temporalMu.Lock()
		if old := temporalClient; old != nil {
			old.Close()
		}
		temporalClient = tc
		temporalMu.Unlock()
		log.Printf("[mailer-service] temporal client connected: %s/%s", cfg.TemporalHostPort, cfg.TemporalNamespace)
		return true
	}
	if cfg.TemporalHostPort != "" {
		if !dialTemporal() {
			// 启动时 Temporal 短暂不可用（如 Docker 内置 DNS 故障）不应永久禁用工作流触发，后台重试直到连上
			go func() {
				ticker := time.NewTicker(30 * time.Second)
				defer ticker.Stop()
				for range ticker.C {
					if dialTemporal() {
						return
					}
				}
			}()
		}
	}

	defaultMailer := mailer.New(cfg)

	r := gin.New()
	r.Use(gin.Logger())
	r.Use(gin.Recovery())
	r.Use(cors.New(cors.Config{
		AllowOrigins:     []string{"*"},
		AllowMethods:     []string{"GET", "POST", "PUT", "DELETE", "OPTIONS"},
		AllowHeaders:     []string{"Origin", "Content-Type", "Accept", "Authorization"},
		AllowCredentials: true,
		MaxAge:           12 * time.Hour,
	}))

	r.GET("/health", func(ctx *gin.Context) {
		ctx.JSON(http.StatusOK, gin.H{"status": "ok", "service": "mailer"})
	})

	// 素材库中已有数据的 GitHub 组织名列表（raw_items 中 source=github 的去重 keyword）
	r.GET("/email/github-orgs", func(ctx *gin.Context) {
		var orgs []string
		if err := database.Table("raw_items").Where("source = ? AND keyword <> ''", "github").Distinct("keyword").Pluck("keyword", &orgs).Error; err != nil {
			ctx.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		sort.Strings(orgs)
		ctx.JSON(http.StatusOK, gin.H{"orgs": orgs})
	})

	// 发起邮件分析工作流
	r.POST("/email", func(ctx *gin.Context) {
		var input struct {
			OrgName string `json:"org_name"`
		}
		if err := ctx.ShouldBindJSON(&input); err != nil {
			ctx.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		tc := getTemporal()
		if tc == nil {
			ctx.JSON(http.StatusServiceUnavailable, gin.H{"error": "temporal client not available"})
			return
		}
		wr, err := tc.ExecuteWorkflow(ctx, client.StartWorkflowOptions{
			TaskQueue: cfg.TemporalTaskQueue,
			ID:        fmt.Sprintf("email-%s-%s", input.OrgName, uuid.NewString()),
		}, "EmailWorkflow", input)
		if err != nil {
			ctx.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		ctx.JSON(http.StatusOK, gin.H{"workflow_id": wr.GetID(), "run_id": wr.GetRunID()})
	})

	// 邮件批次列表
	r.GET("/email-batches", func(ctx *gin.Context) {
		var batches []models.EmailBatch
		database.Order("id desc").Limit(50).Find(&batches)
		for i := range batches {
			b := &batches[i]
			if b.Status == "analyzing" && getTemporal() != nil {
				if b.WorkflowID == "" {
					database.Model(b).Updates(map[string]any{"status": "failed", "error_msg": "workflow unknown"})
					b.Status = "failed"
					continue
				}
				desc, err := getTemporal().DescribeWorkflowExecution(ctx, b.WorkflowID, "")
				if err != nil {
					log.Printf("[email-batches] describe workflow %s failed: %v", b.WorkflowID, err)
				} else {
					s := desc.WorkflowExecutionInfo.Status.String()
					if s == "WORKFLOW_EXECUTION_STATUS_FAILED" || s == "WORKFLOW_EXECUTION_STATUS_TERMINATED" || s == "WORKFLOW_EXECUTION_STATUS_TIMED_OUT" {
						database.Model(b).Updates(map[string]any{"status": "failed", "error_msg": "workflow " + s})
						b.Status = "failed"
					} else if s == "WORKFLOW_EXECUTION_STATUS_COMPLETED" {
						var fresh models.EmailBatch
						if err := database.First(&fresh, b.ID).Error; err == nil && fresh.Status != "" {
							*b = fresh
						}
					}
				}
			}
		}
		ctx.JSON(http.StatusOK, gin.H{"batches": batches})
	})

	r.GET("/email-batches/:id", func(ctx *gin.Context) {
		id, _ := strconv.Atoi(ctx.Param("id"))
		var batch models.EmailBatch
		if err := database.First(&batch, id).Error; err != nil {
			ctx.JSON(http.StatusNotFound, gin.H{"error": "not found"})
			return
		}
		var recipients []models.EmailRecipient
		database.Where("batch_id = ?", id).Find(&recipients)
		ctx.JSON(http.StatusOK, gin.H{"batch": batch, "recipients": recipients})
	})

	r.POST("/email-batches/:id/approve", func(ctx *gin.Context) {
		id, _ := strconv.Atoi(ctx.Param("id"))
		var batch models.EmailBatch
		if err := database.First(&batch, id).Error; err != nil {
			ctx.JSON(http.StatusNotFound, gin.H{"error": "not found"})
			return
		}
		if batch.Status != models.StatusPending {
			ctx.JSON(http.StatusBadRequest, gin.H{"error": "batch not pending"})
			return
		}
		now := time.Now()
		database.Model(&models.EmailRecipient{}).Where("batch_id = ? AND status = ?", id, models.StatusPending).Update("status", models.StatusQueued)
		database.Model(&batch).Updates(map[string]any{"status": models.StatusSending, "approved_at": &now})
		ctx.JSON(http.StatusOK, gin.H{"status": models.StatusSending})
	})

	r.POST("/email-batches/:id/send", func(ctx *gin.Context) {
		id, _ := strconv.Atoi(ctx.Param("id"))
		var input struct {
			Content       string `json:"content"`
			Subject       string `json:"subject"`
			CcList        string `json:"cc_list"`
			RecipientIDs  []uint `json:"recipient_ids"`
			EmailSenderID *uint  `json:"email_sender_id"`
		}
		if err := ctx.ShouldBindJSON(&input); err != nil {
			ctx.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		updates := map[string]any{}
		if input.CcList != "" {
			updates["cc_list"] = input.CcList
		}
		if input.Subject != "" {
			updates["custom_subject"] = input.Subject
		}
		if input.Content != "" {
			updates["custom_content"] = input.Content
		}
		if input.EmailSenderID != nil {
			updates["email_sender_id"] = *input.EmailSenderID
		}
		database.Model(&models.EmailBatch{}).Where("id = ?", id).Updates(updates)

		if len(input.RecipientIDs) > 0 {
			database.Model(&models.EmailRecipient{}).Where("batch_id = ? AND id IN ?", id, input.RecipientIDs).UpdateColumns(map[string]any{"status": models.StatusQueued, "selected": true})
			database.Model(&models.EmailRecipient{}).Where("batch_id = ? AND id NOT IN ? AND status = ?", id, input.RecipientIDs, models.StatusPending).UpdateColumns(map[string]any{"selected": false, "status": "skipped"})
		}
		ctx.JSON(http.StatusOK, gin.H{"status": models.StatusSending})
	})

	r.GET("/email-batches/:id/send-stats", func(ctx *gin.Context) {
		id, _ := strconv.Atoi(ctx.Param("id"))
		var recipients []models.EmailRecipient
		database.Where("batch_id = ?", id).Find(&recipients)

		emails := make([]string, 0, len(recipients))
		for _, r := range recipients {
			emails = append(emails, r.Email)
		}
		var logs []models.EmailSendLog
		if len(emails) > 0 {
			database.Where("email IN ?", emails).Find(&logs)
		}
		logMap := make(map[string]int)
		for _, l := range logs {
			logMap[l.Email] = l.Count
		}
		type RecipientStat struct {
			ID       uint   `json:"id"`
			Email    string `json:"email"`
			Name     string `json:"name"`
			Repo     string `json:"repo"`
			Reason   string `json:"reason"`
			Selected bool   `json:"selected"`
			Status   string `json:"status"`
			Count    int    `json:"count"`
		}
		stats := make([]RecipientStat, 0, len(recipients))
		for _, r := range recipients {
			stats = append(stats, RecipientStat{
				ID: r.ID, Email: r.Email, Name: r.Name, Repo: r.Repo, Reason: r.Reason,
				Selected: r.Selected, Status: r.Status, Count: logMap[r.Email],
			})
		}
		ctx.JSON(http.StatusOK, gin.H{"recipients": stats})
	})

	r.POST("/email-batches/:id/reject", func(ctx *gin.Context) {
		id, _ := strconv.Atoi(ctx.Param("id"))
		database.Model(&models.EmailBatch{}).Where("id = ? AND status = ?", id, models.StatusPending).Update("status", models.StatusRejected)
		ctx.JSON(http.StatusOK, gin.H{"status": models.StatusRejected})
	})

	r.DELETE("/email-batches/:id", func(ctx *gin.Context) {
		id, _ := strconv.Atoi(ctx.Param("id"))
		var batch models.EmailBatch
		if err := database.First(&batch, id).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				ctx.JSON(http.StatusNotFound, gin.H{"error": "not found"})
				return
			}
			ctx.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		if err := database.Where("batch_id = ?", id).Delete(&models.EmailRecipient{}).Error; err != nil {
			ctx.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		if err := database.Delete(&models.EmailBatch{}, id).Error; err != nil {
			ctx.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		ctx.JSON(http.StatusOK, gin.H{"message": "deleted"})
	})

	// 邮件模板管理
	r.GET("/email-templates", func(ctx *gin.Context) {
		var templates []models.EmailTemplate
		database.Order("is_default desc, id desc").Find(&templates)
		ctx.JSON(http.StatusOK, gin.H{"templates": templates})
	})
	r.POST("/email-templates", func(ctx *gin.Context) {
		var t models.EmailTemplate
		if err := ctx.ShouldBindJSON(&t); err != nil {
			ctx.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		if t.Name == "" || t.Subject == "" {
			ctx.JSON(http.StatusBadRequest, gin.H{"error": "name and subject required"})
			return
		}
		if t.IsDefault {
			database.Model(&models.EmailTemplate{}).Where("is_default = ?", true).Update("is_default", false)
		}
		database.Create(&t)
		ctx.JSON(http.StatusCreated, gin.H{"template": t})
	})
	r.PUT("/email-templates/:id", func(ctx *gin.Context) {
		id, _ := strconv.Atoi(ctx.Param("id"))
		var t models.EmailTemplate
		if err := database.First(&t, id).Error; err != nil {
			ctx.JSON(http.StatusNotFound, gin.H{"error": "not found"})
			return
		}
		var input models.EmailTemplate
		if err := ctx.ShouldBindJSON(&input); err != nil {
			ctx.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		if input.IsDefault {
			database.Model(&models.EmailTemplate{}).Where("is_default = ?", true).Update("is_default", false)
		}
		database.Model(&t).Updates(map[string]any{
			"name": input.Name, "subject": input.Subject, "body": input.Body,
			"content_type": input.ContentType, "is_default": input.IsDefault, "updated_at": time.Now(),
		})
		ctx.JSON(http.StatusOK, gin.H{"template": t})
	})
	r.DELETE("/email-templates/:id", func(ctx *gin.Context) {
		id, _ := strconv.Atoi(ctx.Param("id"))
		database.Delete(&models.EmailTemplate{}, id)
		ctx.JSON(http.StatusOK, gin.H{"message": "deleted"})
	})

	// 邮件服务商管理
	maskSender := func(s models.EmailSender) gin.H {
		return gin.H{
			"id":           s.ID,
			"name":         s.Name,
			"host":         s.Host,
			"port":         s.Port,
			"user":         s.User,
			"password":     maskSecret(s.Password),
			"from_address": s.FromAddress,
			"from_name":    s.FromName,
			"daily_limit":  s.DailyLimit,
			"used_today":   s.UsedToday,
			"last_used_at": s.LastUsedAt,
			"enabled":      s.Enabled,
			"is_default":   s.IsDefault,
			"created_at":   s.CreatedAt,
			"updated_at":   s.UpdatedAt,
		}
	}
	r.GET("/email-senders", func(ctx *gin.Context) {
		var senders []models.EmailSender
		database.Order("is_default desc, id desc").Find(&senders)
		out := make([]gin.H, 0, len(senders))
		for _, s := range senders {
			out = append(out, maskSender(s))
		}
		ctx.JSON(http.StatusOK, gin.H{"senders": out})
	})
	r.POST("/email-senders", func(ctx *gin.Context) {
		var input struct {
			Name        string `json:"name"`
			Host        string `json:"host"`
			Port        int    `json:"port"`
			User        string `json:"user"`
			Password    string `json:"password"`
			FromAddress string `json:"from_address"`
			FromName    string `json:"from_name"`
			DailyLimit  int    `json:"daily_limit"`
			Enabled     *bool  `json:"enabled"`
			IsDefault   *bool  `json:"is_default"`
		}
		if err := ctx.ShouldBindJSON(&input); err != nil {
			ctx.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		if input.Name == "" || input.Host == "" || input.User == "" || input.Password == "" || input.FromAddress == "" {
			ctx.JSON(http.StatusBadRequest, gin.H{"error": "name, host, user, password and from_address are required"})
			return
		}
		if input.Port == 0 {
			input.Port = 465
		}
		sender := models.EmailSender{
			Name:        input.Name,
			Host:        input.Host,
			Port:        input.Port,
			User:        input.User,
			Password:    input.Password,
			FromAddress: input.FromAddress,
			FromName:    input.FromName,
			DailyLimit:  input.DailyLimit,
			Enabled:     true,
			IsDefault:   false,
		}
		if input.Enabled != nil {
			sender.Enabled = *input.Enabled
		}
		if input.IsDefault != nil && *input.IsDefault {
			sender.IsDefault = true
			database.Model(&models.EmailSender{}).Where("is_default = ?", true).Update("is_default", false)
		}
		if err := database.Create(&sender).Error; err != nil {
			ctx.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		ctx.JSON(http.StatusCreated, maskSender(sender))
	})
	r.PUT("/email-senders/:id", func(ctx *gin.Context) {
		id, _ := strconv.Atoi(ctx.Param("id"))
		var sender models.EmailSender
		if err := database.First(&sender, id).Error; err != nil {
			ctx.JSON(http.StatusNotFound, gin.H{"error": "sender not found"})
			return
		}
		var input struct {
			Name        *string `json:"name"`
			Host        *string `json:"host"`
			Port        *int    `json:"port"`
			User        *string `json:"user"`
			Password    *string `json:"password"`
			FromAddress *string `json:"from_address"`
			FromName    *string `json:"from_name"`
			DailyLimit  *int    `json:"daily_limit"`
			Enabled     *bool   `json:"enabled"`
			IsDefault   *bool   `json:"is_default"`
		}
		if err := ctx.ShouldBindJSON(&input); err != nil {
			ctx.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		updates := map[string]any{"updated_at": time.Now()}
		if input.Name != nil {
			updates["name"] = *input.Name
		}
		if input.Host != nil {
			updates["host"] = *input.Host
		}
		if input.Port != nil {
			updates["port"] = *input.Port
		}
		if input.User != nil {
			updates["user"] = *input.User
		}
		if input.Password != nil && *input.Password != "" {
			updates["password"] = *input.Password
		}
		if input.FromAddress != nil {
			updates["from_address"] = *input.FromAddress
		}
		if input.FromName != nil {
			updates["from_name"] = *input.FromName
		}
		if input.DailyLimit != nil {
			updates["daily_limit"] = *input.DailyLimit
		}
		if input.Enabled != nil {
			updates["enabled"] = *input.Enabled
		}
		if input.IsDefault != nil {
			updates["is_default"] = *input.IsDefault
			if *input.IsDefault {
				database.Model(&models.EmailSender{}).Where("id != ? AND is_default = ?", id, true).Update("is_default", false)
			}
		}
		database.Model(&sender).Updates(updates)
		database.First(&sender, id)
		ctx.JSON(http.StatusOK, maskSender(sender))
	})
	r.DELETE("/email-senders/:id", func(ctx *gin.Context) {
		id, _ := strconv.Atoi(ctx.Param("id"))
		database.Delete(&models.EmailSender{}, id)
		ctx.JSON(http.StatusOK, gin.H{"message": "deleted"})
	})
	r.POST("/email-senders/:id/test", func(ctx *gin.Context) {
		id, _ := strconv.Atoi(ctx.Param("id"))
		var sender models.EmailSender
		if err := database.First(&sender, id).Error; err != nil {
			ctx.JSON(http.StatusNotFound, gin.H{"error": "sender not found"})
			return
		}
		var req struct {
			To string `json:"to"`
		}
		_ = ctx.ShouldBindJSON(&req)
		toEmail := req.To
		if toEmail == "" {
			toEmail = sender.FromAddress
		}
		testMailer := mailer.NewCustom(sender.Host, sender.Port, sender.User, sender.Password, sender.FromAddress, sender.FromName)
		title := fmt.Sprintf("[%s] 测试发信通知", sender.Name)
		body := fmt.Sprintf("这是一封来自 %s (%s:%d) 的测试邮件，表明发信服务商配置正确。发送时间: %s", sender.Name, sender.Host, sender.Port, time.Now().Format("2006-01-02 15:04:05"))
		if err := testMailer.Send([]string{toEmail}, nil, title, body, "text"); err != nil {
			ctx.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("测试发信失败: %v", err)})
			return
		}
		ctx.JSON(http.StatusOK, gin.H{"message": fmt.Sprintf("测试邮件已成功发送至 %s", toEmail)})
	})

	// 后台邮件发送队列处理器
	log.Printf("[mailer-service] starting background sender (host=%s user=%s from=%s)", cfg.SMTPHost, cfg.SMTPUser, cfg.SMTPFrom)
	go func() {
		for {
			var recipients []models.EmailRecipient
			database.Where("status = ?", models.StatusQueued).Order("id").Limit(10).Find(&recipients)

			if len(recipients) == 0 {
				var pendingBatches []models.EmailBatch
				database.Where("status IN ?", []string{models.StatusPending, models.StatusSending}).Find(&pendingBatches)
				for _, b := range pendingBatches {
					var queued int64
					database.Model(&models.EmailRecipient{}).Where("batch_id = ? AND status = ?", b.ID, models.StatusQueued).Count(&queued)
					if queued > 0 && b.Status == models.StatusPending {
						database.Model(&b).Updates(map[string]any{"status": models.StatusSending, "approved_at": time.Now()})
					}
					if queued == 0 {
						var sent int64
						database.Model(&models.EmailRecipient{}).Where("batch_id = ? AND status = ?", b.ID, "sent").Count(&sent)
						if sent > 0 {
							var pendingCount int64
							database.Model(&models.EmailRecipient{}).Where("batch_id = ? AND status = ?", b.ID, models.StatusPending).Count(&pendingCount)
							if pendingCount == 0 {
								log.Printf("[mailer-service] batch #%d complete (%d sent)", b.ID, sent)
								database.Model(&b).Updates(map[string]any{"status": "sent", "sent": sent, "sent_at": time.Now()})
							}
						}
					}
				}
				time.Sleep(3 * time.Second)
				continue
			}

			log.Printf("[mailer-service] picked up %d queued recipient(s)", len(recipients))

			var tpl models.EmailTemplate
			if err := database.Where("is_default = ?", true).First(&tpl).Error; err != nil {
				log.Printf("[mailer-service] WARNING: no default template, sending anyway")
			}

			var allSenders []models.EmailSender
			database.Where("enabled = ?", true).Find(&allSenders)
			senderMap := make(map[uint]models.EmailSender)
			var availableSenders []models.EmailSender
			now := time.Now()

			for _, s := range allSenders {
				// 跨天自动重置发送计数
				if s.LastUsedAt != nil && (now.Year() != s.LastUsedAt.Year() || now.YearDay() != s.LastUsedAt.YearDay()) {
					s.UsedToday = 0
					_ = database.Model(&models.EmailSender{}).Where("id = ?", s.ID).Update("used_today", 0).Error
				}
				senderMap[s.ID] = s
				if s.DailyLimit <= 0 || s.UsedToday < s.DailyLimit {
					availableSenders = append(availableSenders, s)
				}
			}

			for _, r := range recipients {
				log.Printf("[mailer-service] sending to %s...", r.Email)
				var batch models.EmailBatch
				database.First(&batch, r.BatchID)

				subject := batch.CustomSubject
				body := batch.CustomContent

				if subject == "" {
					subject = tpl.Subject
				}
				if body == "" {
					body = tpl.Body
				}

				name := r.Name
				if name == "" {
					name = r.Email
				}
				subject = strings.ReplaceAll(subject, "{{.OrgName}}", batch.OrgName)
				subject = strings.ReplaceAll(subject, "{{.Name}}", name)
				body = strings.ReplaceAll(body, "{{.OrgName}}", batch.OrgName)
				body = strings.ReplaceAll(body, "{{.Name}}", name)

				var ccList []string
				if batch.CcList != "" {
					for _, cc := range strings.Split(batch.CcList, ",") {
						cc = strings.TrimSpace(cc)
						if cc != "" {
							ccList = append(ccList, cc)
						}
					}
				}

				activeMailer := defaultMailer
				var senderRecord *models.EmailSender

				// 1. 若批次指定了特定的发件人
				if batch.EmailSenderID != nil && *batch.EmailSenderID > 0 {
					if s, ok := senderMap[*batch.EmailSenderID]; ok {
						if s.DailyLimit > 0 && s.UsedToday >= s.DailyLimit {
							database.Model(&r).Updates(map[string]any{"status": models.StatusSkipped, "error_msg": "sender daily limit reached"})
							continue
						}
						activeMailer = mailer.NewCustom(s.Host, s.Port, s.User, s.Password, s.FromAddress, s.FromName)
						senderRecord = &s
					}
				}

				// 2. 若批次未指定发件人，则从数据库中所有可用（启用且未达配额）的发件邮箱中随机选取一个发送
				if senderRecord == nil && len(availableSenders) > 0 {
					chosen := availableSenders[rand.Intn(len(availableSenders))]
					activeMailer = mailer.NewCustom(chosen.Host, chosen.Port, chosen.User, chosen.Password, chosen.FromAddress, chosen.FromName)
					senderRecord = &chosen
				}

				if err := activeMailer.Send([]string{r.Email}, ccList, subject, body, tpl.ContentType); err != nil {
					log.Printf("[mailer-service] FAIL %s: %v", r.Email, err)
					errorsink.Record(database, errorsink.Entry{
						System:  "mailer-service",
						Source:  "mailer",
						Message: fmt.Sprintf("send email to %s failed: %v", r.Email, err),
						Context: fmt.Sprintf(`{"batch_id":%d,"recipient_id":%d,"email":"%s"}`, r.BatchID, r.ID, r.Email),
					})
					database.Model(&r).Updates(map[string]any{"status": models.StatusFailed, "error_msg": err.Error()})
				} else {
					log.Printf("[mailer-service] OK %s", r.Email)
					now := time.Now()
					database.Model(&r).Updates(map[string]any{"status": "sent", "sent_at": &now})
					database.Where("email = ?", r.Email).
						Assign(map[string]any{"count": gorm.Expr("count + 1"), "last_sent": now, "updated_at": now}).
						FirstOrCreate(&models.EmailSendLog{Email: r.Email})
					if senderRecord != nil {
						database.Model(&models.EmailSender{}).Where("id = ?", senderRecord.ID).
							Updates(map[string]any{"used_today": gorm.Expr("used_today + 1"), "last_used_at": now})
					}
				}
			}
		}
	}()

	port := cfg.AppPort
	if port == "" {
		port = "8002"
	}
	addr := fmt.Sprintf("%s:%s", cfg.AppHost, port)
	srv := &http.Server{
		Addr:              addr,
		Handler:           r,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	go func() {
		log.Printf("Mailer service listening on %s", addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("run mailer service: %v", err)
		}
	}()
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, os.Interrupt, syscall.SIGTERM)
	<-quit
	log.Printf("shutting down Mailer service")
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Fatalf("graceful shutdown: %v", err)
	}
}

func maskSecret(s string) string {
	if len(s) <= 8 {
		if s == "" {
			return ""
		}
		return "****"
	}
	return s[:4] + "****" + s[len(s)-4:]
}
