"use strict";
const $ = id => document.getElementById(id);
let token = "", nodes = [], jobs = [], metadata = {}, selected = null, timer = null, loading = false, requestID = "";
let telemetry = null;
let sessionGeneration = 0;
let loadingSession = -1;
async function api(path, method = "GET", payload) {
  const generation = sessionGeneration;
  const response = await fetch("/v1/capacity/" + path, {method, cache:"no-store", headers:{Authorization:"Bearer " + token, "Content-Type":"application/json"}, body:payload === undefined ? undefined : JSON.stringify(payload)});
  if (generation !== sessionGeneration) throw new Error("登录状态已变更，请重新刷新。");
  if (response.status === 401) { logout(); $("login-error").textContent = "页面令牌无效，请重新登录。"; throw new Error("页面令牌无效，请重新登录。"); }
  const data = await response.json();
  if (generation !== sessionGeneration) throw new Error("登录状态已变更，请重新刷新。");
  if (!response.ok) throw new Error(data.message || data.error || "请求失败");
  return data;
}
function notice(message, failed = false) { $("notice").hidden = false; $("notice").textContent = message; $("notice").classList.toggle("failure", failed); }
function logout() { sessionGeneration++; token = ""; nodes = []; jobs = []; telemetry = null; metadata = {}; clearInterval(timer); $("dashboard").hidden = true; $("login").hidden = false; $("logout").hidden = true; $("connection").textContent = "尚未连接"; $("reclaim-dialog").close(); $("detail-dialog").close(); $("onboard-dialog").close(); clearCredentials(); }
$("logout").onclick = logout;
$("login-form").onsubmit = async event => { event.preventDefault(); const generation = ++sessionGeneration; token = $("token").value.trim(); $("login-error").textContent = ""; try { metadata = await api("console"); await refresh(true); if(generation !== sessionGeneration) return; $("token").value = ""; $("login").hidden = true; $("dashboard").hidden = false; $("logout").hidden = false; clearInterval(timer); timer = setInterval(() => refresh(), 3000); } catch(error) { if(generation !== sessionGeneration) return; token = ""; $("login-error").textContent = error.message; } };
async function refresh(throwError = false) {
  const generation = sessionGeneration;
  if ((loading && loadingSession === generation) || !token) return; loading = true; loadingSession = generation;
  try { const [result, onboarding, metrics] = await Promise.all([api("console/instances"),api("console/onboarding"),api("console/telemetry")]); nodes = result.instances || []; jobs = onboarding.jobs || []; telemetry=metrics.telemetry; render(); renderJobs(); renderTelemetry(metrics); $("onboard").disabled = !metadata.onboarding_enabled; $("onboard").title = metadata.onboarding_enabled ? "" : "控制节点尚未配置自动安装制品"; $("connection").textContent = "● 已连接" + (metadata.partner_id ? " · " + metadata.partner_id : ""); $("updated").textContent = "更新于 " + new Date().toLocaleTimeString("zh-CN"); }
  catch(error) { if(generation !== sessionGeneration) return; $("connection").textContent = "连接中断 · 数据可能已过期"; for(const prefix of ["p","d"]) {for(const suffix of ["rate","pressure","chart-value"])$(prefix+"-"+suffix).textContent="—";$(prefix+"-valid").textContent="等待连接";$(prefix+"-load").textContent="当前指标未知";} $("metrics-time").textContent="连接中断，当前指标未知"; if (throwError) throw error; notice(error.message, true); }
  finally { if(loadingSession === generation) loading = false; }
}
function status(node) {
  if (node.instance_state === "RELEASED") return ["已回收", ""];
  if (node.pending_release) return [node.last_error ? "回收重试中" : node.service_state === "DRAINING" ? "摘流中" : node.instance_state === "RELEASING" ? "释放中" : "等待回收", node.last_error ? "red" : "amber"];
  if (node.instance_state === "LOST") return ["节点失联", "red"];
  if (node.instance_state === "RELEASING") return [node.service_state === "DRAINING" ? "摘流中" : "释放中", "amber"];
  if (node.service_state === "NONE") return ({ALLOCATING:["接入中","blue"],PREPARING:["准备中","blue"],IDLE:["空闲",""]})[node.instance_state] || ["未知状态","red"];
  return ({SERVING:["服务中","green"],STARTING:["启动中","blue"],HEALTHY:["待注册","blue"],REGISTERING:["注册中","blue"],DRAINING:["摘流中","amber"],FAILED:["异常","red"]})[node.service_state] || ["未知状态","red"];
}
function lifecycleLabel(state) {
  return ({ALLOCATING:"接入中",PREPARING:"准备中",IDLE:"已就绪",RELEASING:"释放中",RELEASED:"已回收",LOST:"失联"})[state] || "未知";
}
function element(tag, text, className) { const el = document.createElement(tag); if(text !== undefined) el.textContent = text; if(className) el.className = className; return el; }
function render() {
  const active = nodes.filter(n => n.instance_state !== "RELEASED"), serving = active.filter(n => n.instance_state === "IDLE" && n.service_state === "SERVING" && !n.pending_release);
  $("count-active").textContent = active.length; $("count-prefill").textContent = serving.filter(n => n.role === "prefill").length; $("count-decode").textContent = serving.filter(n => n.role === "decode").length; $("count-pending").textContent = active.filter(n => n.pending_release).length; $("count-all").textContent = nodes.length;
  $("model").textContent = metadata.model || "未配置"; $("model").title = metadata.model || "";
  const query = $("search").value.trim().toLowerCase();
  const shown = nodes.filter(n => ($("show-released").checked || n.instance_state !== "RELEASED") && [n.id,n.partner_id,n.service_endpoint,n.endpoint].join(" ").toLowerCase().includes(query));
  $("nodes").replaceChildren(); $("empty").hidden = shown.length !== 0;
  for(const node of shown) {
    const row = element("tr"), name = element("td"); name.append(element("strong",node.id),element("small",node.partner_id)); row.append(name);
    row.append(element("td",node.role === "prefill" ? "P / Prefill" : node.role === "decode" ? "D / Decode" : "—","role"));
    const gpu=element("td",node.spec?.gpu ? node.spec.gpu + " × " + node.spec.gpu_count : "—"); gpu.append(element("small","容器资源规格；非服务用卡数")); row.append(gpu);
    const state = element("td"), [label, color] = status(node); state.append(element("span",label,"badge " + color)); if(node.last_error) state.title = node.last_error; row.append(state);
    row.append(element("td",node.service_endpoint || node.endpoint,"endpoint"));
    const actions = element("td",undefined,"actions"), detail = element("button","详情"), reclaim = element("button",node.instance_state === "RELEASED" ? "已回收" : node.pending_release ? "处理中" : "回收","reclaim");
    detail.onclick = () => showDetail(node); reclaim.disabled = node.instance_state === "RELEASED" || node.pending_release; reclaim.onclick = () => openReclaim(node); actions.append(detail,reclaim); if(node.instance_state === "RELEASED") { const online=element("button","重新上线"); online.disabled=!metadata.onboarding_enabled; online.onclick=()=>openOnboard({id:node.id,partner_id:node.partner_id}); actions.append(online); } row.append(actions); $("nodes").append(row);
  }
  $("table-summary").textContent = "显示 " + shown.length + " 个节点 · 共 " + nodes.length + " 条记录";
}
$("refresh").onclick = () => refresh(); $("search").oninput = render; $("show-released").onchange = render;
function openReclaim(node) {
  selected = node; $("reclaim-id").textContent = node.id; $("grace").value = metadata.drain_grace_seconds ?? 60; $("reclaim-error").textContent = ""; $("confirm-reclaim").disabled = false;
  const last = node.role && nodes.filter(n => n.instance_state === "IDLE" && !n.pending_release && n.role === node.role && n.service_state === "SERVING").length <= 1;
  $("impact").hidden = !last; $("impact").textContent = "这是当前最后一个 " + (node.role === "prefill" ? "Prefill" : "Decode") + " 节点。回收后，P/D 推理可能暂时不可用。";
  $("release-note").textContent = metadata.release_modes?.[node.partner_id] === "static" ? "当前为本地容量管理：将停止推理服务并标记已回收，平台容器会保留。" : "服务停止后，Talea 将调用合作商的容量释放接口。"; $("reclaim-dialog").showModal();
}
$("cancel-reclaim").onclick = () => $("reclaim-dialog").close();
$("reclaim-form").onsubmit = async event => { event.preventDefault(); const generation = sessionGeneration, id = selected.id; $("confirm-reclaim").disabled = true; try { await api("console/instances/"+encodeURIComponent(id)+"/reclaim","POST",{grace_seconds:Number($("grace").value)}); if(generation !== sessionGeneration) return; $("reclaim-dialog").close(); notice(id + " 的回收请求已保存，Talea 将自动完成后续操作。"); await refresh(); } catch(error) { if(generation === sessionGeneration) $("reclaim-error").textContent = error.message; } finally { if(generation === sessionGeneration) $("confirm-reclaim").disabled = false; } };
async function showDetail(node) {
  const generation = sessionGeneration;
  $("detail-title").textContent = node.id; $("detail-status").textContent = "正在读取…"; $("audit").replaceChildren(); $("detail-dialog").showModal();
  try { const [result, log] = await Promise.all([api("console/instances/"+encodeURIComponent(node.id)),api("console/instances/"+encodeURIComponent(node.id)+"/audit")]); if(generation !== sessionGeneration) return; const current = result.instance; $("detail-status").textContent = "节点状态：" + status(current)[0] + "\n容器生命周期：" + lifecycleLabel(current.instance_state) + "（" + current.instance_state + "）" + "\n服务状态：" + current.service_state + "\n角色：" + (current.role || "未分配") + "\n回收请求：" + (current.pending_release ? "已提交" : "无") + "\n最近错误：" + (current.last_error || "无");
    for(const entry of (log.entries || []).slice(0,15)) { const item = element("div",entry.action,"audit-entry"); item.append(element("small",new Date(entry.occurred_at).toLocaleString("zh-CN"))); if(entry.details) item.append(element("small",Object.entries(entry.details).map(([k,v])=>k+": "+v).join(" · "))); $("audit").append(item); } if(!log.entries?.length) $("audit").textContent = "暂无操作记录";
  } catch(error) { if(generation === sessionGeneration) $("detail-status").textContent = error.message; }
}
$("close-detail").onclick = () => $("detail-dialog").close();

function clearCredentials() { $("onboard-password").value=""; $("onboard-key").value=""; }
function newRequestID() { const bytes=new Uint8Array(16); crypto.getRandomValues(bytes); return Array.from(bytes,b=>b.toString(16).padStart(2,"0")).join(""); }
function authFields() { const key=$("onboard-auth").value==="key"; $("password-label").hidden=key; $("onboard-password").hidden=key; $("onboard-password").required=!key; $("key-label").hidden=!key; $("onboard-key").hidden=!key; $("onboard-key").required=key; clearCredentials(); }
function openOnboard(previous={}) {
  $("onboard-form").reset(); clearCredentials(); authFields(); requestID=newRequestID(); $("onboard-error").textContent=""; $("confirm-onboard").disabled=false;
  $("onboard-partner").replaceChildren(); for(const id of Object.keys(metadata.release_modes||{})) { const option=element("option",id); option.value=id; $("onboard-partner").append(option); }
  $("onboard-partner").disabled=!!metadata.partner_id;
  for(const [field,key] of [["id","id"],["partner","partner_id"],["host","host"],["port","port"],["user","user"],["advertise","advertise_host"]]) { if(previous[key]) $("onboard-"+field).value=previous[key]; }
  $("onboard-dialog").showModal(); $("onboard-id").focus();
}
$("onboard").onclick=()=>openOnboard(); $("onboard-auth").onchange=authFields;
$("cancel-onboard").onclick=()=>$("onboard-dialog").close(); $("onboard-dialog").onclose=clearCredentials;
$("onboard-form").onsubmit=async event=>{
  event.preventDefault(); const generation = sessionGeneration; $("confirm-onboard").disabled=true; $("onboard-error").textContent="";
  const payload={request_id:requestID,id:$("onboard-id").value.trim(),partner_id:$("onboard-partner").value,host:$("onboard-host").value.trim(),port:Number($("onboard-port").value),user:$("onboard-user").value.trim(),advertise_host:$("onboard-advertise").value.trim(),host_key:$("onboard-fingerprint").value.trim()};
  if($("onboard-auth").value==="key")payload.private_key=$("onboard-key").value; else payload.password=$("onboard-password").value;
  try { await api("console/onboarding","POST",payload); if(generation !== sessionGeneration) return; $("onboard-dialog").close(); clearCredentials(); notice(payload.id+" 的上线任务已保存，Talea 正在自动接管。"); await refresh(); }
  catch(error) { if(generation === sessionGeneration) $("onboard-error").textContent=error.message; }
  finally { if(generation === sessionGeneration) $("confirm-onboard").disabled=false; }
};
function jobStatus(job, node) {
  if(job.state !== "MANAGED") return [job.stage,job.state === "FAILED" ? "red" : "blue"];
  if(!job.lease_id) return ["已完成",""];
  if(!node) return ["已完成",""];
  if(node.lease_id !== job.lease_id || node.instance_state === "RELEASED") return ["已下线",""];
  return status(node);
}
function canRetryJob(job, node) {
  return job.state === "FAILED" && (!node || node.instance_state === "RELEASED") &&
    !jobs.some(other => other.partner_id === job.partner_id && other.id === job.id && ["QUEUED","RUNNING"].includes(other.state));
}
function renderJobs() {
  $("onboarding-panel").hidden=jobs.length===0; $("onboarding-jobs").replaceChildren();
  for(const job of jobs.slice(0,10)) {
    const row=element("div",undefined,"job-row"), info=element("div",undefined,"job-info"), state=element("div",undefined,"job-state");
    info.append(element("strong",job.id),element("small",job.partner_id+" · "+job.host+":"+job.port));
    if(job.created_at) info.append(element("small","提交于 "+new Date(job.created_at).toLocaleString("zh-CN",{hour12:false}),"job-time"));
    const node=nodes.find(n=>n.id===job.id && n.partner_id===job.partner_id), current=jobStatus(job,node);
    state.append(element("span",current[0],"badge "+current[1]));
    if(job.error)info.append(element("p",job.error,"error"));
    if(canRetryJob(job,node)) {const retry=element("button","修改并重试","job-retry");retry.type="button";retry.onclick=()=>openOnboard(job);state.append(retry);}
    row.append(info,state);$("onboarding-jobs").append(row);
  }
}

function fmt(value,digits=0) {return typeof value==="number"&&Number.isFinite(value)?value.toFixed(digits):"—";}
function renderTelemetry(result) {
  const t=result.telemetry; $("planner-mode").textContent="固定配比 "+result.gear;
  $("metrics-time").textContent=t.at&&!t.at.startsWith("0001")?"采样于 "+new Date(t.at).toLocaleTimeString("zh-CN")+" · 每 3 秒采集 · 最近 5 分钟":"等待采样";
  for(const [prefix,r] of [["p",t.prefill],["d",t.decode]]) {
    $(prefix+"-rate").textContent=typeof r.tokens_per_second==="number"?r.tokens_per_second.toLocaleString("zh-CN",{minimumFractionDigits:1,maximumFractionDigits:1}):"—";
    const pressure=r.pressure==null?"—":fmt(r.pressure*100)+"%";
    $(prefix+"-pressure").textContent=pressure;$(prefix+"-chart-value").textContent=pressure;
    $(prefix+"-valid").textContent=r.valid+" / "+r.nodes+" 节点可用";
    $(prefix+"-load").replaceChildren();
    for(const [label,value] of [["运行",r.running],["等待",r.queue],["KV 在途",r.transfer]]) {const item=element("span",label);item.append(element("b",fmt(value)));$(prefix+"-load").append(item);}
  }
  $("metrics-nodes").replaceChildren();
  for(const n of t.nodes||[]) {const row=element("tr"),label=element("td",n.id+" / "+(n.observed_role||"待核实"));label.append(element("small","Router: "+(n.router_role||"待核实")));row.append(label);for(const v of [n.tp?"TP="+n.tp+" · DP="+n.dp:"—",n.available?fmt(n.running)+" / "+fmt(n.max_running):"—",n.available?fmt(n.queue==null||n.prealloc==null?null:n.queue+n.prealloc)+" / "+fmt(n.transfer):"—",n.available&&n.cache!=null?fmt(n.cache*100,1)+"%":"—",fmt(n.router_load),n.available?"正常":n.error||"未知"])row.append(element("td",v));$("metrics-nodes").append(row);}
  renderPressureChart(t.history||[]);
}
