package workflows

// pythonMCPTaskQueue 是 Python MCP worker（抓取/发布/推文生成）所在的任务队列。
// 注意：应与部署配置中的 TEMPORAL_PYTHON_TASK_QUEUE 保持一致，
// 若队列名变更需同步修改此常量。
const pythonMCPTaskQueue = "python-mcp-taskqueue"

// -----------------------------------------------------------------------
// Shared types used across workflow definitions
// -----------------------------------------------------------------------

// FetchInput is sent to the Python MCP worker.
type FetchInput struct {
	Source     string `json:"source"`
	Keyword    string `json:"keyword"`
	Limit      int    `json:"limit"`
	Mode       string `json:"mode"`
	WorkflowID string `json:"workflow_id"`
}

// FetchResult is returned by the Python worker after writing to MySQL cache.
type FetchResult struct {
	Total    int `json:"total"`
	Inserted int `json:"inserted"`
}

// PublishInput is sent to the Python MCP worker for publishing.
type PublishInput struct {
	Source    string `json:"source"`
	AccountID string `json:"account_id"`
	Content   string `json:"content"`
}

// AutoTweetInput configures tweet generation.
type AutoTweetInput struct {
	Hours  int    `json:"hours"`
	Count  int    `json:"count"`
	Source string `json:"source"`
}

// SocialTweet is a single generated tweet.
type SocialTweet struct {
	Score     float64  `json:"score"`
	SourceURL string   `json:"source_url"`
	Keyword   string   `json:"keyword"`
	Tweet     string   `json:"tweet"`
	Reasoning string   `json:"reasoning"`
	Reasons   []string `json:"reasons"`
}

// AutoTweetResult is returned by the social generation activity.
type AutoTweetResult struct {
	Tweets      []SocialTweet `json:"tweets"`
	TotalScored int           `json:"total_scored"`
}

// 点赞相关的载荷类型在 M3-2 收口到 skill 契约：workflow 侧用 skillcontract.LikeInput /
// skillcontract.Output 与 skill 'like' 对话，本文件不再保留发给 pyreplay worker 的
// LikeTarget / LikeBatchInput / LikeBatchResult（旧直连链路的遗留类型）。
