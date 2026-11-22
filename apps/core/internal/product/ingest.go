package product

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/wetee/ai-workflow/internal/llm"
	"github.com/wetee/ai-workflow/internal/qdrant"
)

// IngestInput 是内部产品信息的入库输入（S7 §3.1：POST /ingest source=internal / knowledge-sync）。
type IngestInput struct {
	SourceType      string `json:"source_type"` // product_doc | tech_doc | meeting_minutes | ticket
	Content         string `json:"content"`
	URL             string `json:"url"`
	Confidentiality string `json:"confidentiality"` // public | internal | secret
	DocTitle        string `json:"doc_title"`
}

// IngestInternal 清洗 → 分块 → 向量化 → 入库 Qdrant（带 source_type/confidentiality/doc_title/updated_at metadata）。
// 幂等：分块 ID 由 (doc_title, 内容 hash, 下标) 确定性派生，重复入库覆盖同一点。
// 返回入库的 chunk 数。空内容返回错误。
func IngestInternal(ctx context.Context, store *Store, qc *qdrant.Client, llmClient *llm.Client, in IngestInput) (int, error) {
	if !llmClient.EmbeddingsEnabled() {
		return 0, errors.New("embedding disabled: EMBEDDING_MODEL not set")
	}
	conf := NormalizeConfidentiality(in.Confidentiality)
	content := CleanContent(in.Content)
	if strings.TrimSpace(content) == "" {
		return 0, errors.New("empty content after cleaning")
	}
	if in.DocTitle == "" {
		in.DocTitle = in.SourceType
	}
	hash := ContentHash(content)
	chunks := ChunkContent(content, 512, 50)
	if len(chunks) == 0 {
		return 0, errors.New("no chunks produced")
	}

	vectors, err := llmClient.EmbedBatch(ctx, chunks)
	if err != nil {
		return 0, fmt.Errorf("embed chunks: %w", err)
	}
	now := time.Now()
	qcChunks := make([]qdrant.Chunk, len(chunks))
	for i, c := range chunks {
		qcChunks[i] = qdrant.Chunk{
			ID:              DeterministicChunkID(in.DocTitle, hash, i),
			Content:         c,
			Source:          "internal",
			URL:             in.URL,
			PublishedAt:     &now,
			SourceType:      in.SourceType,
			Confidentiality: conf,
			DocTitle:        in.DocTitle,
			UpdatedAt:       &now,
		}
	}
	if err := qc.UpsertChunks(ctx, qcChunks, vectors); err != nil {
		return 0, fmt.Errorf("upsert chunks: %w", err)
	}
	// 记去重基线（knowledge-sync 增量用；/ingest 直传也记录）
	_ = store.UpsertKnowledgeDoc(ctx, &KnowledgeDoc{
		Path:            knowledgeDocPath(in),
		ContentHash:     hash,
		SourceType:      in.SourceType,
		Confidentiality: conf,
		Title:           in.DocTitle,
	})
	return len(chunks), nil
}

// knowledgeDocPath 为 /ingest 直传构造去重 key（无文件路径时用 url / title+hash）。
func knowledgeDocPath(in IngestInput) string {
	if in.URL != "" {
		return "url:" + in.URL
	}
	return "title:" + in.DocTitle
}

// NormalizeConfidentiality 归一密级（缺省 internal；非法值 → internal）。
func NormalizeConfidentiality(c string) string {
	switch strings.ToLower(strings.TrimSpace(c)) {
	case "public", "secret":
		return strings.ToLower(strings.TrimSpace(c))
	default:
		return "internal"
	}
}

// secretRoles 是可见 secret 级内容的白名单角色（S7 §3.1 分层授权）。
var secretRoles = []string{"secret", "owner"}

// ConfidentialityLevelsFor 按用户角色返回可见的密级集合（总纲 §8.3 分层授权）：
// 普通用户 → [public]；admin → [public, internal]；白名单角色 → [public, internal, secret]。
func ConfidentialityLevelsFor(roles []string) []string {
	if hasAnyRole(roles, secretRoles...) {
		return []string{"public", "internal", "secret"}
	}
	if hasRole(roles, "admin") {
		return []string{"public", "internal"}
	}
	return []string{"public"}
}

func hasRole(roles []string, want string) bool {
	for _, r := range roles {
		if r == want {
			return true
		}
	}
	return false
}

func hasAnyRole(roles []string, wants ...string) bool {
	for _, w := range wants {
		if hasRole(roles, w) {
			return true
		}
	}
	return false
}

// SecretMask 把 secret 级内容脱敏（红线：外部 LLM provider 只能拿到脱敏 chunk）。
// 用于把 secret 命中放入给 LLM 的上下文时替换为占位（保留来源标注，不保留正文）。
func SecretMask(chunk qdrant.Chunk) string {
	return fmt.Sprintf("[secret 内容已脱敏，来源：(%s, %s)]", chunk.DocTitle, chunk.Source)
}

// ---- 清洗 / 分块 / hash ----

var (
	reMarkdownLink = regexp.MustCompile(`\[([^\]]+)\]\([^)]+\)`) // [text](url) → text
	reCodeFence    = regexp.MustCompile("```[a-zA-Z]*")          // 代码围栏标记
	reHeading      = regexp.MustCompile(`^\s{0,3}#{1,6}\s*`)     // # 标题
	reBoldItalic   = regexp.MustCompile(`\*{1,3}`)               // **bold** / *italic*（只去 * 标记，保留 _ 以保 snake_case）
	reWhitespace   = regexp.MustCompile(`[ \t]+`)
	reBlankLines   = regexp.MustCompile(`\n{3,}`)
)

// CleanContent 去格式化（S7 §3.1：去格式化）：剥离 markdown 链接/围栏/标题/加粗、压缩空白。
func CleanContent(s string) string {
	s = reCodeFence.ReplaceAllString(s, "")
	s = reMarkdownLink.ReplaceAllString(s, "$1")
	s = reHeading.ReplaceAllString(s, "")
	s = reBoldItalic.ReplaceAllString(s, "")
	s = reWhitespace.ReplaceAllString(s, " ")
	s = reBlankLines.ReplaceAllString(s, "\n\n")
	return strings.TrimSpace(s)
}

// ContentHash 计算内容的 sha256 hex（去重 key）。
func ContentHash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// DeterministicChunkID 由 (docTitle, hash, 下标) 派生稳定 UUID（uuid5），重复入库覆盖同一点。
func DeterministicChunkID(docTitle, hash string, idx int) string {
	return uuid.NewSHA1(uuid.NameSpaceURL, []byte(fmt.Sprintf("%s|%s|%d", docTitle, hash, idx))).String()
}

// ChunkContent 把文本切成约 tokenSize token 的分块，相邻块重叠 overlap token。
// 用字符预算近似 token（中英混排约 2.5 字符/token）；优先在句子/段落边界切分。
func ChunkContent(s string, tokenSize, overlap int) []string {
	const charsPerToken = 2.5
	budget := int(float64(tokenSize) * charsPerToken)
	ovlChars := int(float64(overlap) * charsPerToken)
	if budget <= 0 {
		budget = 2000
	}
	if ovlChars <= 0 {
		ovlChars = 200
	}

	segments := splitSentences(s)
	var chunks []string
	var cur strings.Builder
	for _, seg := range segments {
		if cur.Len() > 0 && cur.Len()+len(seg) > budget {
			chunks = append(chunks, strings.TrimSpace(cur.String()))
			// 重叠：保留末尾 ovlChars 字符作为下一块的开头
			tail := cur.String()
			if len(tail) > ovlChars {
				cur.Reset()
				cur.WriteString(tail[len(tail)-ovlChars:])
			} else {
				cur.Reset()
			}
		}
		cur.WriteString(seg)
		cur.WriteString("\n")
	}
	if strings.TrimSpace(cur.String()) != "" {
		chunks = append(chunks, strings.TrimSpace(cur.String()))
	}
	return chunks
}

// splitSentences 按句子/段落边界切分（保留分隔符），用于在边界处分块。
// 手动扫描（RE2 不支持 lookbehind），按中英文句末标点切句并保留标点。
func splitSentences(s string) []string {
	dels := map[rune]bool{'。': true, '！': true, '？': true, '!': true, '?': true, '.': true}
	var segs []string
	for _, para := range strings.Split(s, "\n") {
		para = strings.TrimSpace(para)
		if para == "" {
			continue
		}
		var cur strings.Builder
		for _, r := range para {
			cur.WriteRune(r)
			if dels[r] {
				if strings.TrimSpace(cur.String()) != "" {
					segs = append(segs, strings.TrimSpace(cur.String()))
				}
				cur.Reset()
			}
		}
		if strings.TrimSpace(cur.String()) != "" {
			segs = append(segs, strings.TrimSpace(cur.String()))
		}
	}
	return segs
}
