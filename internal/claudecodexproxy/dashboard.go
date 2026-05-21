package claudecodexproxy

import (
	"encoding/json"
	"html/template"
	"net/http"
	"strings"
)

func (p *Proxy) handleUsage(w http.ResponseWriter, r *http.Request) {
	period := strings.TrimSpace(r.URL.Query().Get("period"))
	if period == "" {
		period = "day"
	}
	summary, err := p.usageStore.QuerySummary(period)
	if err != nil {
		writeAnthropicError(w, http.StatusInternalServerError, "api_error", "failed to query usage")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(summary)
}

func (p *Proxy) handleUsageDashboard(w http.ResponseWriter, r *http.Request) {
	period := strings.TrimSpace(r.URL.Query().Get("period"))
	if period == "" {
		period = "day"
	}
	summary, err := p.usageStore.QuerySummary(period)
	if err != nil {
		http.Error(w, "failed to query usage", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	dashboardTmpl.Execute(w, struct {
		Summary usageSummary
		Period  string
	}{summary, period})
}

var dashboardTmpl = template.Must(template.New("dashboard").Parse(`<!DOCTYPE html>
<html lang="zh">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1.0">
<title>Claude Code Proxy - 使用统计</title>
<style>
body { font-family: system-ui, sans-serif; max-width: 900px; margin: 40px auto; padding: 0 20px; color: #333; background: #f9f9f9; }
h1 { font-size: 1.5rem; color: #111; }
.nav { margin-bottom: 24px; }
.nav a { margin-right: 12px; text-decoration: none; padding: 6px 14px; border-radius: 4px; background: #e0e0e0; color: #333; }
.nav a.active { background: #333; color: #fff; }
.cards { display: flex; gap: 16px; margin-bottom: 32px; flex-wrap: wrap; }
.card { background: #fff; border-radius: 8px; padding: 20px 24px; box-shadow: 0 1px 4px rgba(0,0,0,.08); flex: 1; min-width: 160px; }
.card .label { font-size: 0.8rem; color: #666; margin-bottom: 6px; }
.card .value { font-size: 1.8rem; font-weight: 600; }
table { width: 100%; border-collapse: collapse; background: #fff; border-radius: 8px; overflow: hidden; box-shadow: 0 1px 4px rgba(0,0,0,.08); }
th { text-align: left; padding: 12px 16px; background: #f4f4f4; font-size: 0.85rem; color: #555; border-bottom: 1px solid #e0e0e0; }
td { padding: 12px 16px; border-bottom: 1px solid #f0f0f0; font-size: 0.9rem; }
tr:last-child td { border-bottom: none; }
.empty { text-align: center; padding: 40px; color: #999; }
.notice { background: #fffbe6; border: 1px solid #ffe58f; border-radius: 6px; padding: 12px 16px; margin-bottom: 24px; font-size: 0.9rem; color: #7a5800; }
</style>
</head>
<body>
<h1>Claude Code Proxy 使用统计</h1>
<div class="nav">
  <a href="?period=day" {{if eq .Period "day"}}class="active"{{end}}>今日</a>
  <a href="?period=week" {{if eq .Period "week"}}class="active"{{end}}>本周</a>
  <a href="?period=month" {{if eq .Period "month"}}class="active"{{end}}>本月</a>
</div>
{{if eq .Summary.TotalEvents 0}}
<div class="notice">
  当前时间段内没有使用记录。<br>
  如需启用 Token 追踪，请使用 <code>-tags sqlite</code> 编译并配置 <code>CLAUDE_CODE_PROXY_USAGE_DB_PATH</code>。
</div>
{{else}}
<div class="cards">
  <div class="card"><div class="label">总请求次数</div><div class="value">{{.Summary.TotalEvents}}</div></div>
  <div class="card"><div class="label">输入 Token</div><div class="value">{{.Summary.TotalInput}}</div></div>
  <div class="card"><div class="label">输出 Token</div><div class="value">{{.Summary.TotalOutput}}</div></div>
</div>
<table>
<thead><tr><th>模型</th><th>请求次数</th><th>输入 Token</th><th>输出 Token</th></tr></thead>
<tbody>
{{range .Summary.ByModel}}
<tr><td>{{.Model}}</td><td>{{.Events}}</td><td>{{.InputTokens}}</td><td>{{.OutputTokens}}</td></tr>
{{end}}
</tbody>
</table>
{{end}}
</body>
</html>
`))
