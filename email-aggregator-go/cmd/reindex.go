//go:build reindex

// Command reindex 索引重建工具（Phase 2 立项 A.3）。
// 将 Phase 1 旧 mail-<tenantId>-<accountId> 复合索引重建为 Phase 2 / ADR-009
// per-tenant 物理索引 mail-<tenantId>，文档字段保持不变（含 accountId 字段）。
//
// 用法（与 deploy/docker-compose.yml 中 OpenSearch 一起使用）：
//
//	go run -tags reindex ./cmd/reindex.go \
//	  -addr http://localhost:9200 -user admin -pass admin \
//	  -delete-old=true
//
// 流程：
//  1. GET /_cat/indices/mail-* 列出所有 mail- 开头索引
//  2. 解析索引名：mail-<tid>-<aid> → 目标索引 mail-<tid>；mail-<tid> 已合规则跳过
//  3. POST /mail-<tid>-<aid>/_search?scroll=1m 扫描所有文档
//  4. POST /_bulk 批量写入 mail-<tid>/_doc/<id>
//  5. 可选 DELETE /mail-<tid>-<aid> 删除旧索引
//
// 该工具幂等：重新跑只是再次扫描并 upsert（_doc/<id> 覆盖写）。
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

func main() {
	addr := flag.String("addr", "http://localhost:9200", "OpenSearch base URL")
	user := flag.String("user", "", "Basic auth user (empty for no auth)")
	pass := flag.String("pass", "", "Basic auth pass")
	deleteOld := flag.Bool("delete-old", false, "Delete legacy mail-<tid>-<aid> indices after reindex")
	dryRun := flag.Bool("dry-run", false, "List source/target indices and exit without writing")
	batchSize := flag.Int("batch", 200, "Bulk batch size")
	scrollKeep := flag.String("scroll", "1m", "Scroll keep-alive")
	flag.Parse()

	client := &http.Client{Timeout: 60 * time.Second}
	r := reindexer{
		addr:    strings.TrimRight(*addr, "/"),
		user:    *user,
		pass:    *pass,
		client:  client,
		batch:   *batchSize,
		scroll:  *scrollKeep,
		dryRun:  *dryRun,
		delOld:  *deleteOld,
	}

	indices, err := r.listLegacyIndices()
	if err != nil {
		die("list legacy indices: %v", err)
	}
	if len(indices) == 0 {
		fmt.Println("no legacy mail-<tid>-<aid> indices found — nothing to reindex")
		return
	}

	fmt.Printf("found %d legacy indices:\n", len(indices))
	targets := map[string][]string{} // targetIndex -> []sourceIndex
	for _, src := range indices {
		// mail-<tid>-<aid> → 提取 tid 作为目标索引名
		// 注意：sanitizeIndex 把 @ 替换为 _、小写化；这里假设旧索引名形如 mail-tenanta-acc_demo
		parts := strings.SplitN(strings.TrimPrefix(src, "mail-"), "-", 2)
		if len(parts) < 2 {
			fmt.Printf("  skip %s (already per-tenant or unrecognized)\n", src)
			continue
		}
		target := "mail-" + parts[0]
		targets[target] = append(targets[target], src)
		fmt.Printf("  %s → %s\n", src, target)
	}

	if *dryRun {
		fmt.Println("\n[dry-run] no writes performed")
		return
	}

	total := 0
	for target, srcs := range targets {
		// 确保 index template 已存在（幂等）
		if err := r.ensureTemplate(); err != nil {
			fmt.Fprintf(os.Stderr, "WARN ensure template: %v\n", err)
		}
		for _, src := range srcs {
			n, err := r.reindexOne(src, target)
			if err != nil {
				fmt.Fprintf(os.Stderr, "FAIL %s → %s: %v\n", src, target, err)
				continue
			}
			total += n
			fmt.Printf("  reindexed %d docs from %s to %s\n", n, src, target)
			if *deleteOld {
				if err := r.deleteIndex(src); err != nil {
					fmt.Fprintf(os.Stderr, "WARN delete %s: %v\n", src, err)
				} else {
					fmt.Printf("  deleted legacy index %s\n", src)
				}
			}
		}
	}
	fmt.Printf("\nDONE: reindexed %d docs total\n", total)
}

type reindexer struct {
	addr   string
	user   string
	pass   string
	client *http.Client
	batch  int
	scroll string
	dryRun bool
	delOld bool
}

// listLegacyIndices 列出所有 mail- 开头的 OpenSearch 索引（仅返回索引名）。
// 排除 mail-all 别名（由 index template 自动创建）。
func (r *reindexer) listLegacyIndices() ([]string, error) {
	req, err := http.NewRequest(http.MethodGet, r.addr+"/_cat/indices/mail-*?h=index&format=json", nil)
	if err != nil {
		return nil, err
	}
	r.setAuth(req)
	resp, err := r.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("status=%d body=%s", resp.StatusCode, b)
	}
	var rows []struct {
		Index string `json:"index"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&rows); err != nil {
		return nil, err
	}
	var out []string
	for _, row := range rows {
		// 跳过 mail-all 别名和已经是 per-tenant 的索引（mail-<tid>，仅 2 段）
		name := row.Index
		if name == "mail-all" {
			continue
		}
		if !strings.HasPrefix(name, "mail-") {
			continue
		}
		out = append(out, name)
	}
	return out, nil
}

// ensureTemplate 创建 mail-* index template（幂等）。
func (r *reindexer) ensureTemplate() error {
	tmpl := map[string]any{
		"index_patterns": []string{"mail-*"},
		"template": map[string]any{
			"settings": map[string]any{
				"index.number_of_shards":   3,
				"index.number_of_replicas": 0,
			},
			"mappings": map[string]any{
				"dynamic": false,
				"properties": map[string]any{
					"tenantId":     map[string]string{"type": "keyword"},
					"accountId":    map[string]string{"type": "keyword"},
					"subject":      map[string]string{"type": "text"},
					"from":         map[string]string{"type": "text"},
					"bodyText":     map[string]string{"type": "text"},
					"internalDate": map[string]string{"type": "long"},
				},
			},
			"aliases": map[string]any{"mail-all": map[string]any{}},
		},
		"priority": 100,
	}
	body, _ := json.Marshal(tmpl)
	req, err := http.NewRequest(http.MethodPut, r.addr+"/_index_template/mail-template", bytes.NewReader(body))
	if err != nil {
		return err
	}
	r.setAuth(req)
	req.Header.Set("Content-Type", "application/json")
	resp, err := r.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 && resp.StatusCode != http.StatusConflict {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("ensure template status=%d body=%s", resp.StatusCode, b)
	}
	return nil
}

// reindexOne 用 scroll + bulk 把 src 索引所有文档重建到 target 索引。
// 文档 _id 和 _source 字段保留不变。
func (r *reindexer) reindexOne(src, target string) (int, error) {
	// 启动 scroll
	body, _ := json.Marshal(map[string]any{
		"size": r.batch,
		"query": map[string]any{
			"match_all": map[string]any{},
		},
		"sort": []string{"_doc"},
	})
	req, err := http.NewRequest(http.MethodPost,
		r.addr+"/"+src+"/_search?scroll="+r.scroll, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	r.setAuth(req)
	req.Header.Set("Content-Type", "application/json")
	resp, err := r.client.Do(req)
	if err != nil {
		return 0, err
	}
	var scroll struct {
		ScrollID string `json:"_scroll_id"`
		Hits     struct {
			Hits []struct {
				ID     string         `json:"_id"`
				Source map[string]any `json:"_source"`
			} `json:"hits"`
		} `json:"hits"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&scroll); err != nil {
		resp.Body.Close()
		return 0, err
	}
	resp.Body.Close()

	total := 0
	for len(scroll.Hits.Hits) > 0 {
		// 构造 bulk body：每条 action + source
		var buf bytes.Buffer
		for _, h := range scroll.Hits.Hits {
			action := map[string]any{
				"index": map[string]any{
					"_index": target,
					"_id":    h.ID,
				},
			}
			b1, _ := json.Marshal(action)
			buf.Write(b1)
			buf.WriteByte('\n')
			b2, _ := json.Marshal(h.Source)
			buf.Write(b2)
			buf.WriteByte('\n')
		}
		req, err = http.NewRequest(http.MethodPost, r.addr+"/_bulk", bytes.NewReader(buf.Bytes()))
		if err != nil {
			return total, err
		}
		r.setAuth(req)
		req.Header.Set("Content-Type", "application/x-ndjson")
		resp, err = r.client.Do(req)
		if err != nil {
			return total, err
		}
		resp.Body.Close()
		if resp.StatusCode >= 300 {
			return total, fmt.Errorf("bulk status=%d", resp.StatusCode)
		}
		total += len(scroll.Hits.Hits)

		// 继续滚动
		body, _ := json.Marshal(map[string]any{
			"scroll":   r.scroll,
			"scroll_id": scroll.ScrollID,
		})
		req, err = http.NewRequest(http.MethodPost, r.addr+"/_search/scroll", bytes.NewReader(body))
		if err != nil {
			return total, err
		}
		r.setAuth(req)
		req.Header.Set("Content-Type", "application/json")
		resp, err = r.client.Do(req)
		if err != nil {
			return total, err
		}
		if err = json.NewDecoder(resp.Body).Decode(&scroll); err != nil {
			resp.Body.Close()
			return total, err
		}
		resp.Body.Close()
	}
	return total, nil
}

func (r *reindexer) deleteIndex(name string) error {
	req, err := http.NewRequest(http.MethodDelete, r.addr+"/"+name, nil)
	if err != nil {
		return err
	}
	r.setAuth(req)
	resp, err := r.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 && resp.StatusCode != http.StatusNotFound {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("delete status=%d body=%s", resp.StatusCode, b)
	}
	return nil
}

func (r *reindexer) setAuth(req *http.Request) {
	if r.user != "" {
		req.SetBasicAuth(r.user, r.pass)
	}
}

func die(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}
