package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/wetee/ai-workflow/internal/db"
)

// ---- 人工 PR 自动化（审批通过 → 推分支 + 建 GitHub PR，2026-10-08）----

// maybeCreatePR 扫已审批通过/已上线、尚未建 PR 的任务 → 推 evolve/<task_no> + 建 PR。
// 幂等：pr_url 非空（已建）或 pr_error 非空（已失败/跳过）即不再重试。
// 红线不变：绝不 push/merge **主分支**——只推 feature 分支供人工评审合并。
func (d *driver) createPRs() {
	if !d.prEnabled {
		return // GITHUB_TOKEN 未配置：启动时已一次性提示（不逐轮刷错误）
	}
	var tasks []db.EvolveTask
	if err := d.db.Where("status IN ? AND approval_id IS NOT NULL AND (pr_url IS NULL OR pr_url = '') AND (pr_error IS NULL OR pr_error = '')", []string{"approving", "released"}).Find(&tasks).Error; err != nil {
		log.Printf("WARN: scan tasks for PR: %v", err)
		return
	}
	for i := range tasks {
		t := &tasks[i]
		var apr db.Approval
		if err := d.db.First(&apr, *t.ApprovalID).Error; err != nil || apr.Status != "approved" {
			continue // 还没批 / 审批单查不到
		}
		if err := d.pushAndCreatePR(t); err != nil {
			log.Printf("WARN: PR %s: %v", t.TaskNo, err)
			d.db.Model(&db.EvolveTask{}).Where("task_no = ?", t.TaskNo).Update("pr_error", tail(err.Error(), 4000))
			d.store.Log(d.ctx(), "evolve-driver", "pr_failed", t.TaskNo, map[string]any{"error": err.Error()})
		}
	}
}

// pushAndCreatePR 推分支 + 建 PR 并回写 pr_url。
func (d *driver) pushAndCreatePR(task *db.EvolveTask) error {
	token := strings.TrimSpace(os.Getenv("GITHUB_TOKEN"))
	if token == "" {
		return fmt.Errorf("GITHUB_TOKEN 未配置")
	}
	owner, repo, err := githubOwnerRepo(gitRemoteURL())
	if err != nil {
		return err
	}
	branch := "evolve/" + task.TaskNo

	// 0. 推之前先 pull main：提交 worktree 改动 → 把 main 合入分支（有冲突先解决再提交）→ 再推送。
	//    这样 PR 里已经是「与 main 无冲突」的状态，人工合并时不会在最后一步才暴露冲突。
	workdir := d.cfg.RepoDir + "/" + d.cfg.EvolveCacheDir + "/" + task.TaskNo
	if _, statErr := os.Stat(workdir); statErr == nil {
		if err := d.commitWorktree(workdir, task); err != nil {
			return err
		}
		if merged, err := d.syncBranchWithBase(d.cfg.RepoDir, workdir, branch, d.cfg.BaseBranch); err != nil {
			return err
		} else if merged {
			d.store.Log(d.ctx(), "evolve-driver", "branch_synced_with_base", task.TaskNo, map[string]any{"base": d.cfg.BaseBranch})
		}
	} else {
		log.Printf("WARN: PR %s: worktree 不存在（%s），跳过 pull %s", task.TaskNo, workdir, d.cfg.BaseBranch)
	}

	// 1. push（凭据走 header，不进日志/audit）
	cmd := exec.Command("git", "-C", d.cfg.RepoDir,
		"-c", "http.extraheader=AUTHORIZATION: bearer "+token,
		"push", "origin", branch)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git push: %v: %s", err, redactSecret(string(out), token))
	}

	// 2. 建 PR（已存在则取回已有 PR）
	url, err := ghCreatePR(token, owner, repo, d.cfg.BaseBranch, branch, prTitle(task), prBody(task))
	if err != nil {
		return err
	}
	if err := d.db.Model(&db.EvolveTask{}).Where("task_no = ?", task.TaskNo).Update("pr_url", url).Error; err != nil {
		return fmt.Errorf("回写 pr_url: %w", err)
	}
	d.store.Log(d.ctx(), "evolve-driver", "pr_created", task.TaskNo, map[string]any{"url": url, "branch": branch})
	log.Printf("evolve %s: PR created %s", task.TaskNo, url)
	return nil
}

// githubOwnerRepo 从远端 URL 解析 owner/repo（支持 https://host/owner/repo[.git]）。
func githubOwnerRepo(remote string) (string, string, error) {
	s := strings.TrimSuffix(strings.TrimSuffix(remote, "/"), ".git")
	parts := strings.Split(s, "/")
	if len(parts) < 2 {
		return "", "", fmt.Errorf("无法解析远端 owner/repo: %s", remote)
	}
	return parts[len(parts)-2], parts[len(parts)-1], nil
}

func prTitle(task *db.EvolveTask) string {
	ev := parseEvidence(task.Evidence)
	if t, _ := ev["task"].(string); t != "" {
		return fmt.Sprintf("[evolve %s] %s", task.TaskNo, firstLine(t, 120))
	}
	return fmt.Sprintf("[evolve %s] %s", task.TaskNo, task.TargetName)
}

func prBody(task *db.EvolveTask) string {
	ev := parseEvidence(task.Evidence)
	taskText, _ := ev["task"].(string)
	return fmt.Sprintf("自动进化单 `%s`（目标：%s）——已经 reviewer + 沙箱 + CI + 人工审批 + 灰度验证。\n\n"+
		"任务：\n%s\n\n请评审后合并；合并后重建静态镜像（`docker compose up -d --build <svc>`）以收收拢部署。",
		task.TaskNo, task.TargetName, tail(taskText, 1500))
}

// ghCreatePR 建 PR（POST /pulls）；已存在（422）→ 查回已有 PR 链接。
func ghCreatePR(token, owner, repo, base, head, title, body string) (string, error) {
	client := &http.Client{Timeout: 30 * time.Second}
	api := fmt.Sprintf("https://api.github.com/repos/%s/%s", owner, repo)
	payload, _ := json.Marshal(map[string]any{"title": title, "head": head, "base": base, "body": body})
	req, err := http.NewRequest("POST", api+"/pulls", bytes.NewReader(payload))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("github api: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode >= 300 {
		// 已存在同名 PR → 查回链接
		if resp.StatusCode == 422 && strings.Contains(string(raw), "already exists") {
			if url := ghFindPR(client, api, token, owner, head); url != "" {
				return url, nil
			}
		}
		return "", fmt.Errorf("github PR HTTP %d: %s", resp.StatusCode, tail(string(raw), 300))
	}
	var out struct {
		HTMLURL string `json:"html_url"`
	}
	if err := json.Unmarshal(raw, &out); err != nil || out.HTMLURL == "" {
		return "", fmt.Errorf("github PR 响应异常: %s", tail(string(raw), 200))
	}
	return out.HTMLURL, nil
}

// ghFindPR 查该分支已存在的 PR 链接。
func ghFindPR(client *http.Client, api, token, owner, head string) string {
	req, err := http.NewRequest("GET", fmt.Sprintf("%s/pulls?state=all&head=%s:%s", api, owner, head), nil)
	if err != nil {
		return ""
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := client.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	var prs []struct {
		HTMLURL string `json:"html_url"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&prs); err != nil || len(prs) == 0 {
		return ""
	}
	return prs[0].HTMLURL
}

// redactSecret 从输出里抹掉 token（防泄漏到日志/audit）。
func redactSecret(s, secret string) string {
	if secret == "" {
		return s
	}
	return strings.ReplaceAll(s, secret, "***")
}

// firstLine 取首行并截断（PR 标题用）。
func firstLine(s string, n int) string {
	s = strings.TrimSpace(strings.SplitN(s, "\n", 2)[0])
	return tail(s, n)
}

// skipReviewRequested 是否要求跳过 reviewer。
// 必须每次从库里读最新值：标记一般是任务跑着的时候在看板上点的，
// 而 task 结构体是 runTask 开头加载的，缓存值会漏掉。
func (d *driver) skipReviewRequested(taskNo string) bool {
	var flag int
	if err := d.db.Raw("SELECT COALESCE(skip_review,0) FROM evolve_tasks WHERE task_no = ?", taskNo).Scan(&flag).Error; err != nil {
		log.Printf("WARN: check skip_review for %s: %v", taskNo, err)
		return false
	}
	return flag == 1
}

// ensureEvolveColumns 幂等补列（MySQL 8.0 无 ADD COLUMN IF NOT EXISTS → 查 INFORMATION_SCHEMA）。
