'use strict';
'require view';
'require rpc';
'require ui';
'require poll';

var callStatus = rpc.declare({ object: 'openwrt-mcp-admin', method: 'status', expect: { '': {} } });
var callOpenClash = rpc.declare({ object: 'openwrt-mcp-admin', method: 'openclash', expect: { '': {} } });
var callInterfaces = rpc.declare({ object: 'network.interface', method: 'dump', expect: { '': {} } });
var callDevices = rpc.declare({ object: 'network.device', method: 'status', expect: { '': {} } });
var callLeases = rpc.declare({ object: 'luci-rpc', method: 'getDHCPLeases', expect: { '': {} } });
var callHostHints = rpc.declare({ object: 'luci-rpc', method: 'getHostHints', expect: { '': {} } });
var methods = {};
['create','revoke','query','limit','grant','ungrant','pause','resume','approve','reject','confirm','rollback'].forEach(function(name) {
	methods[name] = rpc.declare({ object: 'openwrt-mcp-admin', method: name, params: ['payload'], expect: { '': {} } });
});

var groups = [['system','系统信息','负载、内存与固件信息'],['network','网络状态与流量','WAN / LAN、接口与流量计数'],['devices','在线设备','设备名称、DHCP 租约与无线客户端'],['logs','系统日志','过滤敏感字段后的系统日志']];
var operations = [['node','切换 OpenClash 节点'],['mode','切换代理模式']];
var tabs = [['overview','概览'],['clients','客户端'],['permissions','权限配置'],['approvals','待审批'],['audit','审计记录']];
var activeTab = 'overview', selectedClient = '', selectedRequest = '', state = null, openclash = null, networkState = null, deviceState = null, leaseState = null, hostHints = null, trafficPrevious = {}, trafficRates = {}, serverOffset = 0, busy = false, dialogOpen = false, lastError = '', searchText = '', auditClient = 'all', auditResult = 'all';

function esc(s) { return String(s == null ? '' : s).replace(/[&<>\"']/g, function(c) { return ({'&':'&amp;','<':'&lt;','>':'&gt;','\"':'&quot;',"'":'&#39;'})[c]; }); }
function el(tag, attrs, children) { return E(tag, attrs || {}, children || []); }
function text(tag, value, cls) { return el(tag, { 'class': cls || '' }, [document.createTextNode(String(value == null ? '' : value))]); }
function btn(label, action, attrs) { attrs = Object.assign({ 'class':'btn', click:function(ev) { ev.preventDefault(); action(); } }, attrs || {}); return el('button', attrs, [document.createTextNode(label)]); }
function payload(name, data) {
	if (busy) return Promise.resolve();
	busy = true;
	return methods[name](JSON.stringify(data || {})).then(function(reply) {
		busy = false;
		if (!reply || reply.ok !== true) throw new Error(reply && reply.error || '管理请求失败');
		lastError = '';
		return refresh().then(function() { notify(name === 'create' ? '客户端已创建' : '操作已完成'); return reply.data; });
	}).catch(function(err) { busy = false; lastError = err.message || String(err); render(); notify('操作失败：' + lastError); return null; });
}
function notify(s) { ui.addNotification(null, E('p', {}, [document.createTextNode(s)])); }
function refresh() {
	return callStatus().then(function(reply) {
		if (!reply || reply.ok !== true || !reply.data || !reply.data.state) throw new Error(reply && reply.error || 'MCP 管理服务不可用');
		var next = reply.data.state;
		next.query_modes = reply.data.query_modes || {};
		if (dialogOpen || (document.activeElement && /^(INPUT|SELECT|TEXTAREA)$/.test(document.activeElement.tagName))) return;
		state = next; serverOffset = Date.parse(next.now || new Date().toISOString()) - Date.now();
		if (!selectedClient && next.clients.length) selectedClient = next.clients[0].id;
		if (!next.clients.some(function(c) { return c.id === selectedClient; })) selectedClient = next.clients.length ? next.clients[0].id : '';
		if (!selectedRequest || !next.requests.some(function(r) { return r.id === selectedRequest; })) {
			var pending = next.requests.find(function(r) { return r.status === 'pending' || r.status === 'waiting'; });
			selectedRequest = pending ? pending.id : (next.requests.length ? next.requests[next.requests.length - 1].id : '');
		}
		render();
	}).catch(function(err) { lastError = err.message || String(err); if (!state) render(); });
}
function requestOpenClash() { callOpenClash().then(function(x) { openclash = x && x.ok ? x.data : null; }).catch(function() { openclash = null; }); }
function requestRouterState() {
	callInterfaces().then(function(x){networkState=x;}).catch(function(){networkState=null;});
	callDevices().then(function(x){var now=Date.now(), rates={}; if(deviceState&&x&&typeof x==='object') Object.keys(x).forEach(function(name){var cur=x[name]&&x[name].statistics,prev=trafficPrevious[name];if(cur&&prev&&now>prev.time){var seconds=(now-prev.time)/1000;rates[name]={rx:Math.max(0,Number(cur.rx_bytes-prev.rx)/seconds),tx:Math.max(0,Number(cur.tx_bytes-prev.tx)/seconds)};}});trafficRates=rates;trafficPrevious={};if(x&&typeof x==='object')Object.keys(x).forEach(function(name){var s=x[name]&&x[name].statistics;if(s)trafficPrevious[name]={rx:Number(s.rx_bytes),tx:Number(s.tx_bytes),time:now};});deviceState=x;}).catch(function(){deviceState=null;trafficRates={};});
	callLeases().then(function(x){leaseState=x;}).catch(function(){leaseState=null;});
	callHostHints().then(function(x){hostHints=x;}).catch(function(){hostHints=null;});
}
function badge(label, tone) { return E('span', { 'class':'badge ' + (tone || '') }, [document.createTextNode(label)]); }
function card(title, body, extra) { return E('section', { 'class':'card ' + (extra || '') }, [E('h2', {}, [document.createTextNode(title)]), body]); }
function rows(items, headers) {
	var head = E('tr', {}, headers.map(function(h) { return E('th', {}, [document.createTextNode(h)]); }));
	return E('div', { 'class':'table-wrap' }, [E('table', {}, [E('thead', {}, [head]), E('tbody', {}, items)])]);
}
function fmtDate(v) { var d = new Date(v); return isNaN(+d) ? '—' : d.toLocaleString('zh-CN', {hour12:false}); }
function remain(v) { var n = Math.max(0, Math.ceil((Date.parse(v) - (Date.now() + serverOffset))/1000)); return Math.floor(n/60) + ' 分 ' + (n%60) + ' 秒'; }
function clientName(id) { var c = state.clients.find(function(x) { return x.id === id; }); return c ? c.name : id || '系统'; }
function statusLabel(s) { return ({pending:'等待审批',executing:'执行中',waiting:'等待人工确认',done:'已完成',kept:'已确认保留',rolledback:'已回滚',rejected:'已拒绝',expired:'已过期',cancelled:'已取消',failed:'失败'})[s] || s; }
function statusTone(s) { return s === 'done' || s === 'kept' ? 'green' : (s === 'pending' || s === 'waiting' || s === 'executing' ? 'amber' : (s === 'failed' || s === 'rolledback' || s === 'rejected' ? 'red' : '')); }
function header(title, desc, action) { return E('div', { 'class':'page-head' }, [E('div', {}, [E('h1', {}, [document.createTextNode(title)]), E('p', {}, [document.createTextNode(desc)])]), action || E('span')]); }
function modal(title, content, buttons) {
	dialogOpen = true;
	var footer = E('div', { 'class':'right mcp-modal-actions' }, buttons || []);
	return ui.showModal(title, E('div', { 'class':'mcp-modal' }, [content, footer]));
}
function copyToken(value, field, trigger) {
	function copied() {
		trigger.textContent = '已复制';
		notify('令牌已复制到剪贴板');
	}
	function fallback() {
		field.focus();
		field.select();
		field.setSelectionRange(0, field.value.length);
		var ok = false;
		try { ok = document.execCommand('copy'); } catch (e) {}
		if (ok) copied();
		else notify('自动复制失败，请手动选择令牌复制');
	}
	if (navigator.clipboard && navigator.clipboard.writeText)
		navigator.clipboard.writeText(value).then(copied).catch(fallback);
	else
		fallback();
}
function summary() {
	var active = state.clients.filter(function(c) { return !c.revoked; }).length;
	var pend = state.requests.filter(function(r) { return r.status === 'pending'; }).length;
	var waiting = state.requests.filter(function(r) { return r.status === 'waiting'; }).length;
	return E('div', { 'class':'stats' }, [
		E('div', { 'class':'stat' }, [text('span', '可用客户端','muted'), text('strong', active)]),
		E('div', { 'class':'stat' }, [text('span', '待审批','muted'), text('strong', pend)]),
		E('div', { 'class':'stat' }, [text('span', '待确认恢复','muted'), text('strong', waiting)]),
		E('div', { 'class':'stat' }, [text('span', '操作总开关','muted'), badge(state.paused ? '已停止' : '运行中', state.paused ? 'red' : 'green')])
	]);
}
function bytes(v) { var n=Number(v); if(!Number.isFinite(n)||n<0)return '—'; var unit=['B','KB','MB','GB','TB'],i=0; while(n>=1024&&i<unit.length-1){n/=1024;i++;} return (i===0?String(Math.round(n)):n.toFixed(1))+' '+unit[i]; }
function rate(v) { return bytes(v)+'/s'; }
function interfaceRows() {
	var items=networkState&&Array.isArray(networkState.interface)?networkState.interface:[];
	if(!items.length)return [E('tr',{},[E('td',{colspan:'4'},[document.createTextNode('接口状态暂不可用')])])];
	return items.map(function(i){var addrs=(i['ipv4-address']||[]).map(function(a){return a.address+'/'+a.mask;}).join(', ')||'—';var dev=i.l3_device||i.device||'',stats=deviceState&&deviceState[dev]&&deviceState[dev].statistics||{};var r=trafficRates[dev];var flow='↓ '+bytes(stats.rx_bytes)+(r?' ('+rate(r.rx)+')':'')+' / ↑ '+bytes(stats.tx_bytes)+(r?' ('+rate(r.tx)+')':'');return E('tr',{},[text('td',i.interface||dev||'未知'),E('td',{},[badge(i.up?'已连接':'未连接',i.up?'green':'')]),text('td',addrs,'mono'),text('td',flow)]);});
}
function leaseList() {
	var raw=leaseState&&((leaseState.dhcp_leases)||(leaseState.leases)||(leaseState.hosts));
	if(!Array.isArray(raw))return {count:'—',rows:[E('tr',{},[E('td',{colspan:'3'},[document.createTextNode('在线设备数据暂不可用')])])]};
	var rows=raw.slice(0,30).map(function(d){var mac=(d.macaddr||d.mac||'').toLowerCase(),hint=hostHints&&hostHints[mac],name=(hint&&(hint.name||hint.hostname))||d.hostname||d.name||'未命名设备';return E('tr',{},[text('td',name),text('td',d.ipaddr||d.ip||'—','mono'),text('td',d.macaddr||d.mac||'—','mono')]);});
	if(!rows.length)rows=[E('tr',{},[E('td',{colspan:'3'},[document.createTextNode('当前没有活动 DHCP 租约')])])];
	return {count:String(raw.length),rows:rows};
}
function pageOverview() {
	var waiting = state.requests.filter(function(r) { return r.status === 'waiting'; });
	var list = waiting.length ? waiting.map(function(r) { return E('li', {}, [E('div', {}, [text('strong', r.preview.summary), text('p', '客户端 '+clientName(r.client)+' · '+remain(r.confirm_deadline), 'muted')]), badge('请人工确认', 'amber')]); }) : [E('li', {}, [text('span','当前没有待确认的敏感变更','muted')])];
	var stop = btn(state.paused?'恢复 MCP 操作':'紧急停止 MCP 操作',function(){var method=state.paused?'resume':'pause';if(!confirm(state.paused?'恢复接受 MCP 操作？':'停止所有 MCP 写操作？临时授权和待审批申请将清除，未确认的恢复仍会继续。'))return;payload(method,{});},{'class':'btn '+(state.paused?'primary':'danger')});
	var leases=leaseList();
	return [header('MCP 权限管理','管理已配对客户端的查询权限、临时授权与逐笔审批。',stop), summary(), card('授权边界', E('div', { 'class':'callout' }, [text('p','查询组可长期授权；继承的旧策略仍受原方法范围、到期时间和 MFA 限制。普通操作需要限时授权，敏感操作必须逐笔审批。'), text('p','总开关只停止 MCP 操作，不会撤销 root SSH 维护通道。','muted') ])), card('网络接口',rows(interfaceRows(),['接口','状态','IPv4 地址','流量计数器'])), card('在线设备 · 活动 DHCP 租约 '+leases.count,rows(leases.rows,['设备','IP 地址','MAC 地址'])), card('需要人工确认', E('ul', { 'class':'simple-list' }, list)), card('OpenClash 状态', E('div', { 'class':'openclash' }, [openclash ? text('p','当前模式：'+(openclash.mode || '未知')) : text('p','未能读取 OpenClash 实时状态；可稍后重试。','muted')]))];
}
function pageClients() {
	var createBtn = btn('新增客户端', function() {
		var form = E('div', {}, [E('label', {for:'client-name'}, [document.createTextNode('客户端名称')]), E('input', {id:'client-name',maxlength:'96',placeholder:'例如 Codex'}), E('label', {for:'client-id'}, [document.createTextNode('客户端 ID（可选）')]), E('input', {id:'client-id',maxlength:'40',placeholder:'留空自动生成'})]);
		modal('新增 MCP 客户端', form, [
			btn('取消',function(){dialogOpen=false;ui.hideModal();refresh();}),
			btn('创建并显示令牌一次',function(){ var name=document.getElementById('client-name').value.trim(), id=document.getElementById('client-id').value.trim(); if(!name)return notify('请填写客户端名称'); payload('create',{id:id,name:name}).then(function(data){ if(!data)return; var area=E('textarea',{readonly:'readonly',rows:'4','class':'mcp-token-value',spellcheck:'false'},[document.createTextNode(data.token)]); var copy=btn('一键复制令牌',function(){copyToken(data.token,area,copy);}); modal('请立即复制此令牌',E('div',{},[text('p','令牌只显示这一次，请安全保存。'),area]),[copy,btn('已安全保存',function(){dialogOpen=false;ui.hideModal();refresh();})]); }); },{'class':'btn primary'})
		]);
	});
	var items = state.clients.map(function(c) { return E('tr', {}, [E('td', {}, [text('strong',c.name),text('small',c.id,'mono')]), E('td', {}, [badge(c.revoked?'已撤销':'可用',c.revoked?'red':'green')]), E('td', {}, [text('span',Object.keys(c.queries||{}).filter(function(k){return c.queries[k];}).length+' 项查询')]), E('td', {}, [text('span',String(c.limit)+' 次/分钟')]), E('td', {}, [btn('管理权限',function(){selectedClient=c.id;activeTab='permissions';render();}), c.revoked?E('span'):btn('撤销',function(){ if(confirm('撤销 '+c.name+' 的令牌并清除其待执行授权？')) payload('revoke',{id:c.id}); },{'class':'btn danger'})])]); });
	return [header('客户端','令牌仅在创建时显示一次，可单独撤销。',createBtn), card('已配对客户端',rows(items,['客户端','状态','查询权限','频率上限','操作']))];
}
function pagePermissions() {
	var c = state.clients.find(function(x){return x.id===selectedClient;});
	if(!c) return [header('权限配置','请选择一个客户端。')];
	var select = E('select',{change:function(ev){selectedClient=ev.target.value;render();}},state.clients.map(function(x){return E('option',{value:x.id,selected:x.id===c.id?'selected':null},[document.createTextNode(x.name+' · '+x.id)]);}));
	var queryRows = groups.map(function(g){ var mode=(state.query_modes[c.id]||{})[g[0]]||'off',enabled=mode==='explicit'||mode==='legacy'; var label=mode==='explicit'?'长期开放':mode==='legacy'?'继承旧策略（原范围 / 到期 / MFA 仍生效）':mode==='expired'?'旧策略已过期，当前拒绝':'默认关闭'; var tone=mode==='explicit'?'green':mode==='legacy'?'amber':mode==='expired'?'red':''; return E('tr',{},[E('td',{},[text('strong',g[1]),text('small',g[2],'muted')]),E('td',{},[badge(label,tone)]),E('td',{},[btn(enabled?'关闭':'开放',function(){payload('query',{id:c.id,group:g[0],enabled:!enabled});})])]); });
	var grantRows = operations.map(function(op){var until=c.grants&&c.grants[op[0]];return E('tr',{},[E('td',{},[text('strong',op[1])]),E('td',{},[until&&Date.parse(until)>Date.now()+serverOffset?badge('剩余 '+remain(until),'amber'):badge('未授权')]),E('td',{},[btn('临时开放 15 分钟',function(){if(!confirm('为 '+c.name+' 临时开放 '+op[1]+' 15 分钟？'))return;payload('grant',{id:c.id,kind:op[0],minutes:15});}),until?btn('收回',function(){payload('ungrant',{id:c.id,kind:op[0]});}):E('span')])]);});
	var limit = E('input',{type:'number',min:'1',max:'600',value:String(c.limit),id:'rate-limit'});
	return [header('权限配置','默认仅开放只读查询。重启和配置修改始终逐笔审批。'),card('当前客户端',E('div',{'class':'field-row'},[select, badge(c.revoked?'已撤销':'可用',c.revoked?'red':'green')])),card('查询权限',rows(queryRows,['权限组','当前状态','操作'])),card('普通操作临时授权',rows(grantRows,['操作','授权状态','操作'])),card('调用频率',E('div',{'class':'field-row'},[limit,text('span','次 / 分钟'),btn('保存上限',function(){var v=Number(document.getElementById('rate-limit').value);payload('limit',{id:c.id,limit:v});},{'class':'btn primary'})])),card('安全说明',text('p','管理页只提交预设操作。敏感配置不会在此开放长期权限，也不会接受任意命令。','muted'))];
}
function diffTable(r) {
	var before=r.preview.before||{}, after=r.preview.after||{}, keys=Array.from(new Set(Object.keys(before).concat(Object.keys(after))));
	return rows(keys.map(function(k){return E('tr',{},[text('td',k,'mono'),text('td',before[k]==null?'—':before[k],'mono'),text('td',after[k]==null?'—':after[k],'mono')]);}),['项目','变更前','变更后']);
}
function pageApprovals() {
	var active = state.requests.filter(function(r){return ['pending','waiting','executing'].includes(r.status);});
	var items = active.map(function(r){return E('tr',{'class':selectedRequest===r.id?'selected':''},[E('td',{},[btn(r.preview.summary,function(){selectedRequest=r.id;render();},{'class':'link-button'})]),text('td',clientName(r.client)),E('td',{},[badge(statusLabel(r.status),statusTone(r.status))]),text('td',r.status==='pending'?remain(r.expires_at):r.status==='waiting'?remain(r.confirm_deadline):'处理中')]);});
	var selected=state.requests.find(function(r){return r.id===selectedRequest;});
	var detail=text('p','选择一笔申请查看具体影响。','muted');
	if(selected){var children=[E('div',{'class':'request-meta'},[badge(statusLabel(selected.status),statusTone(selected.status)),text('span','客户端：'+clientName(selected.client)),text('span','有效期：'+(selected.status==='pending'?remain(selected.expires_at):'—'))]),text('p',selected.preview.summary),diffTable(selected)];
		if(selected.preview.sensitive) children.push(E('div',{'class':'callout warning'},[text('strong','敏感变更'),text('p','请核对每一项变更；此批准仅适用于当前内容和这一笔请求。')]));
		var actions=[];
		if(selected.status==='pending'){var cb=E('input',{type:'checkbox',id:'approve-risk'});actions.push(E('label',{'class':'checkline'},[cb,text('span','我已核对上述变更内容，并明确批准执行这一次操作。')]));actions.push(btn('批准并执行',function(){if(selected.preview.sensitive&&!document.getElementById('approve-risk').checked)return notify('请先勾选逐笔批准确认');if(!confirm('批准并执行这笔操作？'))return;payload('approve',{id:selected.id,digest:selected.digest});},{'class':'btn primary'}));actions.push(btn('拒绝',function(){if(confirm('拒绝此申请？'))payload('reject',{id:selected.id});},{'class':'btn danger'}));}
		if(selected.status==='waiting'){children.push(E('div',{'class':'callout warning'},[text('strong','恢复检查窗口 · '+remain(selected.confirm_deadline)),text('p','只有您从新的管理连接确认设备仍可访问后，才能保留变更。到期会自动恢复。')]));var ack=E('input',{type:'checkbox',id:'confirm-reachable'});actions.push(E('label',{'class':'checkline'},[ack,text('span','我已从新的管理连接重新访问路由器，并确认管理入口正常。')]));actions.push(btn('确认保留变更',function(){if(!document.getElementById('confirm-reachable').checked)return notify('请先确认新的管理连接可用');if(confirm('保留当前变更并删除恢复快照？'))payload('confirm',{id:selected.id});},{'class':'btn primary'}));actions.push(btn('立即回滚',function(){if(confirm('立即恢复原配置？'))payload('rollback',{id:selected.id});},{'class':'btn danger'}));}
		children.push(E('div',{'class':'actions'},actions));detail=E('div',{},children);
	}
	return [header('待审批','每笔请求单独核对。AI 客户端无法批准、确认或取消恢复。'),card('等待处理的申请',rows(items,['申请内容','客户端','状态','剩余时间'])),card('申请详情',detail)];
}
function pageAudit() {
	var clients=['all'].concat(state.clients.map(function(c){return c.id;}));
	var sel=E('select',{change:function(e){auditClient=e.target.value;render();}},clients.map(function(id){return E('option',{value:id,selected:id===auditClient?'selected':null},[document.createTextNode(id==='all'?'所有客户端':clientName(id))]);}));
	var search=E('input',{type:'search',placeholder:'搜索操作或结果',value:searchText,input:function(e){searchText=e.target.value;}});
	var exportBtn=btn('导出 JSON',function(){var blob=new Blob([JSON.stringify(state.audit,null,2)],{type:'application/json'}),a=document.createElement('a');a.href=URL.createObjectURL(blob);a.download='mcp-audit.json';a.click();URL.revokeObjectURL(a.href);});
	var entries=(state.audit||[]).filter(function(e){return (auditClient==='all'||e.client===auditClient)&&(!searchText||(e.action+' '+e.result).toLowerCase().includes(searchText.toLowerCase()));});
	var list=entries.slice().reverse().map(function(e){return E('tr',{},[text('td',fmtDate(e.time)),text('td',clientName(e.client)),text('td',e.action),E('td',{},[badge(e.result,e.result==='allowed'?'green':'')])]);});
	return [header('审计记录','仅展示管理动作摘要；审计不保存令牌或操作参数。',exportBtn),card('事件筛选',E('div',{'class':'field-row'},[sel,search])),card('最近事件',rows(list,['时间','客户端','操作','结果'])),text('p','历史按服务器保留策略自动清理。','muted')];
}
function render() {
	var main=document.querySelector('#view'); if(!main)return;
	if(!state){main.replaceChildren(card('正在连接 MCP 管理服务',text('p',lastError||'正在读取权限状态…','muted')));return;}
	var tab=activeTab==='overview'?pageOverview():activeTab==='clients'?pageClients():activeTab==='permissions'?pagePermissions():activeTab==='approvals'?pageApprovals():pageAudit();
	var nav=E('nav',{'class':'tabs'},tabs.map(function(t){return btn(t[1],function(){activeTab=t[0];render();},{'class':'tab '+(activeTab===t[0]?'active':'')});}));
	main.replaceChildren(nav);
	if(lastError) main.appendChild(E('div',{'class':'error'},[document.createTextNode(lastError)]));
	tab.forEach(function(n){main.appendChild(n);});
}

return view.extend({
	load:function(){ return Promise.all([callStatus(),callOpenClash()]); },
	render:function(){
		var root=E('div',{'id':'view','class':'mcp-console'},[E('link',{rel:'stylesheet',href:L.resource('openwrt-mcp.css')})]);
		requestOpenClash();
		requestRouterState();
		refresh();
		poll.add(function(){ if(!dialogOpen){refresh();if(activeTab==='overview'){requestRouterState();requestOpenClash();}} });
		return root;
	},
	handleSaveApply:null, handleSave:null, handleReset:null
});

