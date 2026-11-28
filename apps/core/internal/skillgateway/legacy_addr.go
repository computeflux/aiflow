// legacy_addr.go：MIG-1（工作项 wi-1795853890）「pycraper / pyreplay → social 收口」的
// 网关侧地址口径。
//
// M4-1 把切流状态机（internal/migration）建起来了：登记目标 addr → 10%→50%→100% 逐档
// 放量 → 收尾把 skills.addr 收敛到目标。本文件补上收尾链的**最后一段**：MIG-1 步骤 5
// 「更新注册表」要的是一个可重复执行的注册表快照，它必须能回答
// 「x-scan / like 现在到底打到哪个 addr」。答案只有一个来源：本文件。
//
// 为什么把常量放在 skillgateway 而不是 migration：addr 是网关的路由事实（skills.addr 是
// 基线服务地址，deploy.VersionManager.baseService 靠它的 host 找回 compose 服务）。
// migration 与 deploy 两侧都引本包，两侧才不会各写一份地址、各自漂移 —— 一旦漂移，
// 「切流说切到 social、下线计划说旧服务还有流量」这类矛盾在审计里根本查不出来。
package skillgateway

// MIG-1 收口后的目标实现（social 服务）地址。
//
// social 容器在 workflow-net 内的监听端口是 8010（main.go 的 SOCIAL_ADDR 缺省值），
// 网关与版本容器都只在内部网络里访问它，所以 host 用容器名 social。
//
// 路径段（/x-scan、/like）是「能力标识」：它落进 skills.addr / skill_versions.config.addr，
// 网关的 social 转发（social_invoker.go）先取基址（scheme://host:port）再拼 social 的
// 渠道路由，所以旧实现（pycraper:9504 / pyreplay:9500）与 S5 灰度版本实例的 addr 不会
// 被误转发。
const (
	// SocialAddr 是 social 服务的基址（无尾斜杠）。
	SocialAddr = "http://social:8010"
	// SocialXScanAddr 是 x-scan 能力在 social 上的调用 addr。
	SocialXScanAddr = SocialAddr + "/x-scan"
	// SocialLikeAddr 是 like 能力在 social 上的调用 addr。
	SocialLikeAddr = SocialAddr + "/like"
)

// LegacyServiceForSkill 返回某 skill 的旧实现 compose 服务名（非 MIG-1 收口范围返回空串）。
func LegacyServiceForSkill(skillName string) string {
	switch skillName {
	case SkillXScan:
		return "pycraper"
	case SkillLike:
		return "pyreplay"
	default:
		return ""
	}
}

// LegacyAddrForSkill 返回某 skill 旧实现的 invoke addr（收口前的基线，回滚时恢复它）。
func LegacyAddrForSkill(skillName string) string {
	switch skillName {
	case SkillXScan:
		return "http://pycraper:9504/scan"
	case SkillLike:
		return "http://pyreplay:9500/like"
	default:
		return ""
	}
}

// CutoverTargetForSkill 返回某 skill 的 MIG-1 切流目标 addr（不在收口范围返回空串）。
func CutoverTargetForSkill(skillName string) string {
	switch skillName {
	case SkillXScan:
		return SocialXScanAddr
	case SkillLike:
		return SocialLikeAddr
	default:
		return ""
	}
}
