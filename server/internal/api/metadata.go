package api

import (
	"fmt"
	"net/http"
	"strconv"

	"github.com/venera-app/venera-nas/internal/jobs"
)

// handleMetadataBackfill 补全 comics 表里缺失的元数据（作者/上传者/简介/标签）。
//
// 为什么会有这个接口：comics 的 author/description 只在**下载任务**里写
// （downloader.RunJob → LoadDetail），而绝大多数条目只是通过收藏/历史
// 同步落库的——那些 op 里根本没有 author/description 字段。
// 结果就是"ehentai 117 本全部没有作者和简介"这类存量缺口：
// 数据在源站上取得到，本地却永远是空的，App 的 NAS 兜底详情页和
// 管理面板都显示不全。
//
// 语义：
//   - GET  只返回"还剩多少条待补"（面板按钮上的角标），不改数据；
//   - POST 真正执行补全，返回 MetaBackfillResult 统计。
//
// 参数：
//   - source  只补某个源（可空 = 全部源）
//   - limit   最多处理多少条（可空 = 全部）
//
// 单次执行是串行的（源站多有速率限制），因此大库会跑得久一些。
// 面板与 App 都不做长轮询：一次请求跑完再返回，客户端看到 loading 结束
// 就是补全完成——这比再引入一套任务队列简单得多。
func (s *Server) handleMetadataBackfill(w http.ResponseWriter, r *http.Request) {
	src := r.URL.Query().Get("source")
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))

	bf := jobs.NewMetaBackfiller(s.st, s.src, func(format string, args ...any) {
		if s.log != nil {
			s.log.Info("metadata backfill: " + fmt.Sprintf(format, args...))
		}
	})

	// GET：只读，返回待补数量
	if r.Method == http.MethodGet {
		n, err := bf.Remaining()
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		res := map[string]any{"remaining": n, "dryRun": true}
		if src != "" {
			res["source"] = src
		}
		writeJSON(w, http.StatusOK, res)
		return
	}

	if src != "" && !s.src.Has(src) {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": "source not installed: " + src,
		})
		return
	}

	result, err := bf.Run(src, limit)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, result)
}
