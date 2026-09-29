package api

import (
	"context"
	"sync"

	"github.com/zennanzhoudev-pixel/ai-interview-platform/internal/knowledge"
)

// knowledgeCache 按租户缓存知识库。
//
// 为什么需要缓存: 每次面试抽题、每次追问都要用到索引, 而重建一份索引
// 要遍历题库并计算向量。不缓存意味着"每个问题都重建一遍" ——
// 那会让追问延迟从毫秒级变成百毫秒级, 而追问延迟正是语音面试最敏感
// 的指标。
//
// 为什么按租户分开: 题库是租户数据。全局单例会把 A 公司的题问给 B 公司的
// 候选人, 这既是数据泄漏, 也让人无法解释"这道题是谁定的"。
type knowledgeCache struct {
	mu       sync.RWMutex
	byTenant map[string]*knowledge.Corpus
	build    func(ctx context.Context, tenant string) (*knowledge.Corpus, error)
}

func newKnowledgeCache(build func(ctx context.Context, tenant string) (*knowledge.Corpus, error)) *knowledgeCache {
	return &knowledgeCache{byTenant: make(map[string]*knowledge.Corpus), build: build}
}

// get 返回该租户的知识库, 必要时构建。
//
// 构建失败时退化为纯内置题库, 而不是让面试无法开始: 自定义题目读不出来
// 是一个可降级的故障, 而我们仍然能靠内置题库完成一场结构完整的面试。
// 这条降级路径必须显式存在, 否则数据库抖动会直接变成"面试开不了"。
func (c *knowledgeCache) get(ctx context.Context, tenant string) *knowledge.Corpus {
	c.mu.RLock()
	if corpus, ok := c.byTenant[tenant]; ok {
		c.mu.RUnlock()
		return corpus
	}
	c.mu.RUnlock()

	if c.build == nil {
		return fallbackCorpus()
	}
	corpus, err := c.build(ctx, tenant)
	if err != nil || corpus == nil {
		return fallbackCorpus()
	}

	c.mu.Lock()
	c.byTenant[tenant] = corpus
	c.mu.Unlock()
	return corpus
}

// invalidate 丢弃某个租户的缓存。题库变更后调用。
func (c *knowledgeCache) invalidate(tenant string) {
	c.mu.Lock()
	delete(c.byTenant, tenant)
	c.mu.Unlock()
}

// fallbackCorpus 返回只含内置题库的知识库。
func fallbackCorpus() *knowledge.Corpus {
	corpus, err := knowledge.Build(context.Background(), nil, nil, nil)
	if err != nil {
		// 内置题库构建失败意味着代码自身有问题(而不是环境问题),
		// 这种情况下没有可用的兜底, 只能返回 nil, 由调用方决定行为。
		return nil
	}
	return corpus
}
