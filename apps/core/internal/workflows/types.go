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

// LikeTarget is a single tweet to like, sent to the pyreplay Python worker.
type LikeTarget struct {
	TweetID  string `json:"tweet_id"`
	TweetURL string `json:"tweet_url"`
	Author   string `json:"author"`
	Content  string `json:"content"`
}

// LikeBatchInput is sent to pyreplay's LikeActivities.like_batch.
type LikeBatchInput struct {
	Targets      []LikeTarget `json:"targets"`
	DelayBetween float64      `json:"delay_between"`
}

// LikeItemResult is the outcome of liking a single tweet.
type LikeItemResult struct {
	TweetID  string `json:"tweet_id"`
	TweetURL string `json:"tweet_url"`
	Success  bool   `json:"success"`
	Error    string `json:"error"`
}

// LikeBatchResult is returned by pyreplay's LikeActivities.like_batch.
type LikeBatchResult struct {
	Total   int              `json:"total"`
	Success int              `json:"success"`
	Failed  int              `json:"failed"`
	Results []LikeItemResult `json:"results"`
}
