// cutover_sync.go：把 MIG-1 注册表快照登记进 skill 网关注册表（步骤 2「把 x-scan / like
// 的 addr 切到 social」+ 步骤 5「更新注册表」的执行面）。
//
// 与 cutover.go 的分工：
//   - Target / Stage / Complete（cutover.go）是**逐档放量**的状态机：一档一挡人工确认，
//     护栏在每一档把关，收尾时才把 skills.addr 收敛到 target_addr；
//   - 本文件只做一件事：在放量**开始之前**，把「收口后的实现」登记成一条零流量的 draft
//     版本（config.addr = social），让 Stage 的首档有版本可用、让 board 的 skill 页能看到
//     「新实现已登记、流量 0%」。
//
// 权限边界（红线，全部由单测钉住）：
//   - 绝不写 skills.addr：基线 addr 是生产路由，只有 Complete（收尾）/ Rollback（回滚）
//     有权限改；同步顺手写它 = 一次注册就把流量打到没跑过的实现上。
//   - 绝不抬 traffic_split：新建行恒为 0，放量只能由 Stage 做（带时长 + 质量护栏）。
//   - 不改写已登记的版本行：库里同名版本的 config.addr 与快照不一致时整批拒绝（让人改快照
//     或换新版本号），静默改写等于绕过护栏改路由。
//   - 整批语义：任一 skill 对不上（未注册 / addr 漂移 / config 不可解析 / 无 live 基线）
//     就一条都不写。半批写入会让运维在 board 上看到「x-scan 已登记、like 没有」，无法判断
//     是同步失败还是只同步了一半。
package migration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/wetee/ai-workflow/internal/db"
	"gorm.io/gorm"
)

// SyncResult 是一次注册表同步的结果。
type SyncResult struct {
	Snapshot *RegistrySnapshot `json:"snapshot"`
	Changes  []SyncChange      `json:"changes"`
	DryRun   bool              `json:"dry_run"`
}

// SyncChange 是一条 skill 的同步结果。
type SyncChange struct {
	Skill       string `json:"skill"`
	Version     string `json:"version"`
	Action      string `json:"action"` // created | unchanged | would_create | rejected
	Addr        string `json:"addr"`
	SkillID     uint   `json:"skill_id,omitempty"`
	VersionID   uint   `json:"version_id,omitempty"`
	Description string `json:"description,omitempty"`
}

// 同步动作。
const (
	SyncCreated    = "created"
	SyncUnchanged  = "unchanged"
	SyncWouldCrate = "would_create"
	SyncRejected   = "rejected"
)

// SyncRegistry 把注册表快照登记进 skill 网关注册表（dryRun = true 只算不写）。
func (s *RegistrySnapshot) SyncRegistry(ctx context.Context, gdb *gorm.DB, dryRun bool) (*SyncResult, error) {
	if gdb == nil {
		return nil, errors.New("migration: SyncRegistry 需要 MySQL（注册表就是数据库）")
	}
	res := &SyncResult{Snapshot: s, DryRun: dryRun}

	for _, sk := range s.Skills {
		ch, err := s.planOne(ctx, gdb, sk)
		if err != nil {
			ch.Action = SyncRejected
			ch.Description = err.Error()
		}
		res.Changes = append(res.Changes, *ch)
	}
	var rejected []string
	for _, ch := range res.Changes {
		if ch.Action == SyncRejected {
			rejected = append(rejected, fmt.Sprintf("%s: %s", ch.Skill, ch.Description))
		}
	}
	if len(rejected) > 0 {
		return res, fmt.Errorf("注册表同步被拒（整批未写入）：%s", strings.Join(rejected, "；"))
	}
	if dryRun {
		return res, nil
	}

	for i := range res.Changes {
		if res.Changes[i].Action != SyncWouldCrate {
			res.Changes[i].Action = SyncUnchanged
			continue
		}
		if err := s.applyOne(ctx, gdb, s.Skills[i], &res.Changes[i]); err != nil {
			return res, err
		}
		res.Changes[i].Action = SyncCreated
	}
	return res, nil
}

// planOne 算出一条 skill 的同步动作（不写库），并顺带完成「可写入性」校验：
// 无 live 基线 / 基线 config 不可解析 / 缺 image 都在这里判，让 SyncRegistry 能在写第一行
// 之前看到全部拒绝项（整批语义要求校验与写入分离，否则第一条写成功、第二条才发现没基线）。
func (s *RegistrySnapshot) planOne(ctx context.Context, gdb *gorm.DB, sk RegistrySkill) (*SyncChange, error) {
	var skill db.Skill
	if err := gdb.WithContext(ctx).Where("name = ?", sk.Name).First(&skill).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return &SyncChange{Skill: sk.Name, Version: sk.Version},
				fmt.Errorf("skill %s 未注册（先注册基线版本，再登记切流目标版本）", sk.Name)
		}
		return &SyncChange{Skill: sk.Name, Version: sk.Version}, fmt.Errorf("query skill %s: %w", sk.Name, err)
	}
	ch := &SyncChange{Skill: sk.Name, Version: sk.Version, SkillID: skill.ID, Addr: strings.TrimSuffix(strings.TrimSpace(sk.Addr), "/")}

	var v db.SkillVersion
	err := gdb.WithContext(ctx).Where("skill_id = ? AND version = ?", skill.ID, sk.Version).First(&v).Error
	switch {
	case err == nil:
		ch.SkillID, ch.VersionID = skill.ID, v.ID
		addr, aErr := configAddrStrict(v.Config)
		if aErr != nil {
			return ch, fmt.Errorf("skill %s v%s 的 config 不可解析（%v），拒绝改写", sk.Name, sk.Version, aErr)
		}
		if addr != ch.Addr {
			return ch, fmt.Errorf("skill %s v%s 已登记 addr=%q，与快照 %q 不一致：请改快照或换新版本号，不改写已登记版本",
				sk.Name, sk.Version, addr, ch.Addr)
		}
		ch.Action = SyncUnchanged
		return ch, nil
	case !errors.Is(err, gorm.ErrRecordNotFound):
		return ch, fmt.Errorf("query skill version %s v%s: %w", sk.Name, sk.Version, err)
	}

	// 新建路径的可写入性校验（与 applyOne 同一口径，先判后写才能整批拒绝）。
	var live db.SkillVersion
	lerr := gdb.WithContext(ctx).Model(&db.SkillVersion{}).
		Select("id", "skill_id", "version", "image", "traffic_split", "status", "config").
		Where("skill_id = ? AND status = ? AND traffic_split > 0", skill.ID, "live").
		Order("id DESC").First(&live).Error
	if errors.Is(lerr, gorm.ErrRecordNotFound) {
		return ch, fmt.Errorf("skill %s 无 live 基线版本，无法继承镜像/config（先注册基线版本）", sk.Name)
	}
	if lerr != nil {
		return ch, fmt.Errorf("query live version of %s: %w", sk.Name, lerr)
	}
	if _, cErr := mergeConfigAddr(live.Config, ch.Addr); cErr != nil {
		return ch, fmt.Errorf("skill %s 的基线 config 不可解析（%v），拒绝继承", sk.Name, cErr)
	}
	if sk.Image == "" && live.Image == "" {
		return ch, fmt.Errorf("skill %s v%s 缺 image（快照与 live 基线都没有）", sk.Name, sk.Version)
	}
	ch.Action = SyncWouldCrate
	return ch, nil
}

// applyOne 新建一条零流量的 draft 版本行（config.addr = 快照 addr）。
func (s *RegistrySnapshot) applyOne(ctx context.Context, gdb *gorm.DB, sk RegistrySkill, ch *SyncChange) error {
	var live db.SkillVersion
	err := gdb.WithContext(ctx).Model(&db.SkillVersion{}).
		Select("id", "skill_id", "version", "image", "traffic_split", "status", "config").
		Where("skill_id = ? AND status = ? AND traffic_split > 0", ch.SkillID, "live").
		Order("id DESC").First(&live).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return fmt.Errorf("skill %s 无 live 基线版本，无法继承镜像/config（先注册基线版本）", sk.Name)
	}
	if err != nil {
		return fmt.Errorf("query live version of %s: %w", sk.Name, err)
	}
	cfg, err := mergeConfigAddr(live.Config, ch.Addr)
	if err != nil {
		return err
	}
	image := sk.Image
	if image == "" {
		image = live.Image
	}
	if image == "" {
		return fmt.Errorf("skill %s v%s 缺 image（快照与 live 基线都没有）", sk.Name, sk.Version)
	}
	row := &db.SkillVersion{
		SkillID:      ch.SkillID,
		Version:      sk.Version,
		Image:        image,
		TrafficSplit: 0, // 零流量：放量只能由 Stage 做（带护栏）
		Status:       "draft",
		Config:       cfg,
	}
	// Select 显式点名 traffic_split：该列带 gorm `default:100`，GORM 的默认写入策略
	// (InsertOnZero) 会跳过带 default 标签的零值 → 落库变 100。与 cutover.go 的
	// ensureTargetVersion 同一处理口径（那里连注释一起解释了为什么还要补一次 UPDATE）。
	if err := gdb.WithContext(ctx).
		Select("skill_id", "version", "image", "config", "traffic_split", "status").
		Create(row).Error; err != nil {
		return fmt.Errorf("create skill version %s v%s: %w", sk.Name, sk.Version, err)
	}
	if err := gdb.WithContext(ctx).Model(row).Update("traffic_split", 0).Error; err != nil {
		return fmt.Errorf("zero traffic_split for %s v%s: %w", sk.Name, sk.Version, err)
	}
	row.TrafficSplit = 0
	ch.VersionID = row.ID
	return nil
}

// configAddrStrict 取版本 config 里的 addr，并区分「解析失败」与「没有 addr 字段」：
// 同步路径必须能拒绝「config 不可解析」的版本行（对一份读不懂的 config 改写 addr，等于
// 把运维看不见的运行参数原样带上生产路由），cutover.go 的 configAddrOf 只判有无。
func configAddrStrict(config string) (string, error) {
	if strings.TrimSpace(config) == "" {
		return "", nil
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(config), &m); err != nil {
		return "", err
	}
	if a, ok := m["addr"].(string); ok {
		return strings.TrimSuffix(strings.TrimSpace(a), "/"), nil
	}
	return "", nil
}

// LoadRegistrySnapshotFromWorkdir 从当前工作目录向上找 monorepo 根，装载并校验快照（CLI 用）。
func LoadRegistrySnapshotFromWorkdir() (*RegistrySnapshot, string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return nil, "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "deploy", "docker-compose.yml")); err == nil {
			snap, err := LoadRegistrySnapshot(dir)
			if err != nil {
				return nil, dir, err
			}
			if err := snap.Validate(dir); err != nil {
				return nil, dir, err
			}
			return snap, dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return nil, "", fmt.Errorf("仓库根不在 %q 之上（找不到 deploy/docker-compose.yml）", dir)
		}
		dir = parent
	}
}
