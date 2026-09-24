package api

import (
	"net/http"
)

// M1 指标出口：JSON 供程序化读取；/view 是免构建的内嵌面板（浏览器打开即用，
// 轮询 JSON + canvas 画趋势，不动 React 前端构建链）。

func (s *Server) handleAdminMetrics(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.meters.Snapshot())
}

func (s *Server) handleAdminMetricsView(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(metricsViewHTML))
}

const metricsViewHTML = `<!doctype html>
<html lang="zh"><head><meta charset="utf-8"><title>容量指标</title>
<style>
body{font:14px/1.5 -apple-system,"PingFang SC",sans-serif;margin:24px;background:#0f1115;color:#e6e6e6}
.card{background:#171a21;border:1px solid #262b36;border-radius:10px;padding:14px 18px;margin-bottom:16px}
.big{font-size:34px;font-weight:700;margin-right:28px;display:inline-block}
.lbl{color:#8b93a7;font-size:12px;margin-right:6px}
table{border-collapse:collapse;width:100%;margin-top:8px}
td,th{padding:4px 10px;text-align:right;border-bottom:1px solid #22262f}
th:first-child,td:first-child{text-align:left}
canvas{width:100%;height:60px;margin-top:6px}
</style></head><body>
<h2>实时容量（60s 窗口 · 2s 刷新）</h2>
<div class="card">
  <span class="big" id="rpm">–</span><span class="lbl">RPM 打进来</span>
  <span class="big" id="tpm">–</span><span class="lbl">TPM tokens</span>
  <span class="big" id="inf">–</span><span class="lbl">在飞</span>
  <span class="big" id="rej">–</span><span class="lbl">4xx/5xx RPM</span>
  <span class="big" id="rpr">–</span><span class="lbl">重试放大</span>
  <canvas id="spark" width="900" height="60"></canvas>
  <div class="lbl">RPM 趋势（3 分钟，每 10s 一点）</div>
</div>
<div class="card">按模型
<table><thead><tr><th>模型</th><th>RPM</th><th>成功</th><th>失败</th><th>prompt tpm</th><th>completion tpm</th><th>重试/req</th></tr></thead>
<tbody id="models"></tbody></table></div>
<script>
const fmt=n=>n>=1e6?(n/1e6).toFixed(1)+"M":n>=1e3?(n/1e3).toFixed(1)+"k":n;
async function tick(){
 try{
  const d=await (await fetch('/api/admin/metrics')).json();
  rpm.textContent=d.global.rpm; tpm.textContent=fmt(d.global.tpm);
  inf.textContent=d.inflight; rej.textContent=d.global.rejected_rpm;
  rpr.textContent=d.global.retry_per_req.toFixed(2);
  models.innerHTML=Object.entries(d.by_model).sort((a,b)=>b[1].rpm-a[1].rpm).map(function(p){
   var m=p[0],s=p[1];
   return '<tr><td>'+m+'</td><td>'+s.rpm+'</td><td>'+s.served_rpm+'</td><td>'+s.rejected_rpm+'</td><td>'+fmt(s.prompt_tpm)+'</td><td>'+fmt(s.completion_tpm)+'</td><td>'+s.retry_per_req.toFixed(2)+'</td></tr>';
  }).join('');
  draw(d.rpm_trend);
 }catch(e){}
}
function draw(arr){
 const c=spark.getContext('2d'),W=spark.width,H=spark.height;
 c.clearRect(0,0,W,H); const max=Math.max(10,...arr);
 c.strokeStyle='#4f8cff';c.lineWidth=2;c.beginPath();
 arr.forEach((v,i)=>{const x=i/(arr.length-1)*W,y=H-4-(v/max)*(H-8);i?c.lineTo(x,y):c.moveTo(x,y)});
 c.stroke();
 c.fillStyle='#4f8cff';c.font='11px sans-serif';c.fillText(max+' rpm',4,12);
}
tick(); setInterval(tick,2000);
</script></body></html>`
