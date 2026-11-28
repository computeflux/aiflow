package main

import (
	"log"
	"strconv"
	"time"

	"github.com/wetee/ai-workflow/internal/db"
	"github.com/wetee/ai-workflow/internal/project"
)

// syncTaskTargetFromCard 用卡上最新目标覆盖进化单（及关联工作项）的目标。
//
// 背景（2026-11-27 卡 #10）：卡的目标从 social 扩成 social,core,core-ui 后，工作项/进化单
// 仍保留 hand-off 时的旧快照 social → evolve-driver 按旧目标做写范围校验，coder 对 core 的
// 合法改动被判越界打回（"已勾选 core" 却无法继续）。
// 卡是目标选择的唯一入口（S9.8 §7.10），工作项/进化单只是建单时快照：每轮范围校验前以卡上的
// 当前目标为准刷新本任务，保证后补勾选的目标立即生效。
//
// 卡不存在 / 卡上目标为空 → 保留任务原目标（不清空，避免中断在跑任务）。
// 返回是否发生了更新。
func (d *driver) syncTaskTargetFromCard(task *db.EvolveTask) bool {
	ev := parseEvidence(task.Evidence)
	itemNo, _ := ev["work_item_no"].(string)
	cardID := d.cardIDForTask(task.Source, ev, itemNo)
	if cardID == 0 {
		return false
	}
	var card db.BoardCard
	if err := d.db.Select("id", "target_name").First(&card, cardID).Error; err != nil {
		return false
	}
	target, changed := effectiveTaskTarget(task.TargetName, card.TargetName)
	if !changed {
		return false
	}
	if err := d.db.Model(&db.EvolveTask{}).Where("task_no = ?", task.TaskNo).
		Update("target_name", target).Error; err != nil {
		log.Printf("WARN: sync task %s target from card %d: %v", task.TaskNo, cardID, err)
		return false
	}
	// 工作项目标同源刷新（看板展示；后续按工作项建单时不再陈旧）
	if itemNo != "" {
		if err := d.db.Model(&db.WorkItem{}).Where("no = ?", itemNo).
			Updates(map[string]any{"target_name": target, "updated_at": time.Now()}).Error; err != nil {
			log.Printf("WARN: sync work item %s target from card %d: %v", itemNo, cardID, err)
		}
	}
	prev := task.TargetName
	task.TargetName = target
	d.store.Log(d.ctx(), "evolve-driver", "target_synced_from_card", task.TaskNo, map[string]any{
		"card_id": cardID, "from": prev, "to": target,
	})
	log.Printf("evolve %s: target %s → %s（以卡 %d 当前目标为准）", task.TaskNo, prev, target, cardID)
	return true
}

// effectiveTaskTarget 计算执行目标：卡目标优先（归一 + 去重），空则保留任务快照。
func effectiveTaskTarget(taskTarget, cardTarget string) (string, bool) {
	card := project.NormalizeTargets(cardTarget)
	if card == "" {
		return taskTarget, false
	}
	if cur := project.NormalizeTargets(taskTarget); cur == card {
		return cur, false
	}
	return card, true
}

// cardIDForTask 解析进化单关联的卡：source=card 用 evidence.card_id；
// source=work_item 经 evidence.work_item_no → work_items.card_id。
func (d *driver) cardIDForTask(source string, ev map[string]any, itemNo string) uint {
	if source == "card" {
		return uintFromAny(ev["card_id"])
	}
	if source != "work_item" || itemNo == "" {
		return 0
	}
	var item db.WorkItem
	if err := d.db.Select("card_id").Where("no = ?", itemNo).First(&item).Error; err != nil {
		return 0
	}
	if item.CardID == nil {
		return 0
	}
	return *item.CardID
}

// uintFromAny 把 evidence 里 JSON 解出的 card_id（float64 / string）转 uint。
func uintFromAny(v any) uint {
	switch n := v.(type) {
	case float64:
		return uint(n)
	case string:
		i, _ := strconv.Atoi(n)
		return uint(i)
	}
	return 0
}
