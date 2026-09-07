const state = { key: localStorage.getItem('wb2api-api-key') || '', models: [] };
const $ = (id) => document.getElementById(id);

function headers() { return state.key ? { Authorization: `Bearer ${state.key}` } : {}; }
async function api(path, options = {}) {
  const response = await fetch(path, { ...options, headers: { ...headers(), ...(options.headers || {}) } });
  const text = await response.text();
  let data; try { data = text ? JSON.parse(text) : {}; } catch { data = { raw: text }; }
  if (!response.ok) throw new Error(data?.error?.message || `${response.status} ${response.statusText}`);
  return data;
}
function setHealth(ok) {
  const pill = $('health-pill'); pill.className = `health-pill ${ok ? 'ok' : 'bad'}`;
  pill.querySelector('span:last-child').textContent = ok ? '服务可用' : '无可用账号';
}
function accountState(account) {
  if (account.disabled) return ['disabled', '已禁用'];
  if (account.cooling) return ['cooling', account.cool_kind === 'hard_credit' ? '积分冷却' : '冷却中'];
  return ['healthy', '可用'];
}
function renderAccounts(accounts = []) {
  const body = $('accounts');
  if (!accounts.length) { body.innerHTML = '<tr><td colspan="5" class="empty">暂无账号，请运行 login.sh 添加授权</td></tr>'; return; }
  body.innerHTML = accounts.map(a => {
    const [cls, label] = accountState(a);
    const total = a.success_count + a.err_total;
    const rate = total ? `${Math.round(a.success_count / total * 100)}%` : '—';
    const credit = Number(a.credits || 0).toLocaleString('zh-CN');
    return `<tr><td><div class="account-id"><span class="account-name">${escapeHtml(a.nickname || '未命名账号')}</span><span class="account-uid">${escapeHtml(a.uid || '')}</span></div></td><td><span class="state ${cls}">${label}</span></td><td class="credit"><strong>${credit}</strong><small>${a.breaker_fails ? `熔断失败 ${a.breaker_fails} 次` : '积分余额'}</small></td><td>${rate}</td><td>${a.in_flight || 0}</td></tr>`;
  }).join('');
}
function renderModels(models = []) {
  state.models = models; $('model-count').textContent = `${models.length} 个模型`;
  $('models').innerHTML = models.length ? models.map(m => `<div class="model-item"><div><div class="model-name">${escapeHtml(m.id)}</div><div class="model-meta">上下文 ${Number(m.context_length || 0).toLocaleString()} tokens</div></div><span class="model-badge">${escapeHtml(m.owned_by || 'workbuddy')}</span></div>`).join('') : '<div class="empty">暂无模型</div>';
  $('model').innerHTML = models.map(m => `<option value="${escapeAttr(m.id)}">${escapeHtml(m.id)}</option>`).join('');
}
function escapeHtml(value) { return String(value).replace(/[&<>"']/g, c => ({ '&':'&amp;', '<':'&lt;', '>':'&gt;', '"':'&quot;', "'":'&#39;' }[c])); }
function escapeAttr(value) { return escapeHtml(value); }
async function refresh() {
  $('last-updated').textContent = '同步中…';
  try {
    const [status, health, models] = await Promise.allSettled([api('/status'), fetch('/healthz').then(r => r.json()), api('/v1/models')]);
    if (status.status === 'fulfilled') {
      const d = status.value; $('total').textContent = d.total ?? 0; $('healthy').textContent = d.healthy ?? 0; $('cooling').textContent = d.cooling ?? 0; $('disabled').textContent = d.disabled ?? 0; $('healthy-detail').textContent = `${d.in_flight_full || 0} 个账号达到并发上限`; $('redis-mode').textContent = d.redis_mode === 'upstash' ? 'Upstash Redis' : '内存模式'; renderAccounts(d.accounts); }
    if (health.status === 'fulfilled') setHealth(health.value.healthy > 0);
    if (models.status === 'fulfilled') renderModels(models.value.data || []);
    if (status.status === 'rejected' && models.status === 'rejected') throw status.reason;
    $('last-updated').textContent = `更新于 ${new Date().toLocaleTimeString('zh-CN', { hour12: false })}`;
  } catch (error) { setHealth(false); if (error.message.includes('frontend password')) { $('lock-dialog').showModal(); } $('last-updated').textContent = error.message; }
}
async function sendMessage() {
  const prompt = $('prompt').value.trim(), model = $('model').value, output = $('response');
  if (!prompt || !model) return;
  output.className = 'response-box pending'; output.textContent = '请求中…'; $('request-state').textContent = '请求中'; $('send').disabled = true;
  try {
    const stream = $('stream').checked;
    const response = await fetch('/v1/chat/completions', { method: 'POST', headers: { 'Content-Type': 'application/json', ...headers() }, body: JSON.stringify({ model, messages: [{ role: 'user', content: prompt }], stream }) });
    if (!response.ok) { const error = await response.json().catch(() => ({})); throw new Error(error?.error?.message || `请求失败 (${response.status})`); }
    if (!stream) { const data = await response.json(); output.textContent = data.choices?.[0]?.message?.content || JSON.stringify(data, null, 2); }
    else { output.textContent = ''; const reader = response.body.getReader(), decoder = new TextDecoder(); let buffer = ''; while (true) { const { value, done } = await reader.read(); if (done) break; buffer += decoder.decode(value, { stream: true }); const lines = buffer.split('\n'); buffer = lines.pop() || ''; for (const line of lines) { if (!line.startsWith('data:')) continue; const payload = line.slice(5).trim(); if (payload === '[DONE]') continue; try { const data = JSON.parse(payload); output.textContent += data.choices?.[0]?.delta?.content || ''; output.scrollTop = output.scrollHeight; } catch {} } } }
    output.className = 'response-box'; $('request-state').textContent = '完成';
  } catch (error) { output.className = 'response-box error'; output.textContent = error.message; $('request-state').textContent = '失败'; }
  finally { $('send').disabled = false; }
}
$('refresh').addEventListener('click', refresh); $('send').addEventListener('click', sendMessage);
$('settings').addEventListener('click', () => { $('api-key').value = state.key; $('settings-dialog').showModal(); });
$('settings-form').addEventListener('submit', event => { if (event.submitter?.id === 'save-key') { state.key = $('api-key').value.trim(); localStorage.setItem('wb2api-api-key', state.key); refresh(); } });
$('endpoint').textContent = `${location.origin}/v1`;
refresh(); setInterval(refresh, 30000);

async function loadAdminConfig() {
  try { const c = await api('/admin/config'); $('checkin-hours').value = (c.schedule?.checkin_hours || []).join(','); $('keepalive-hours').value = (c.schedule?.keepalive_hours || []).join(','); $('config-state').textContent = '已加载'; }
  catch (e) { $('config-state').textContent = e.message; if (e.message.includes('frontend password')) $('lock-dialog').showModal(); }
}
function hours(value) { return value.split(',').map(v => Number(v.trim())).filter(v => Number.isInteger(v)); }
$('save-config').addEventListener('click', async () => { const body = { checkin_hours: hours($('checkin-hours').value), keepalive_hours: hours($('keepalive-hours').value) }; try { const result = await api('/admin/config', { method:'POST', headers:{'Content-Type':'application/json'}, body:JSON.stringify(body) }); $('config-state').textContent = result.restart_required ? '已保存，待重启' : '已保存'; } catch (e) { $('config-state').textContent = e.message; } });
$('run-checkin').addEventListener('click', async () => { $('run-checkin').disabled = true; try { const result = await api('/admin/checkin', { method:'POST' }); $('config-state').textContent = result.message || '签到已启动'; } catch (e) { $('config-state').textContent = e.message; } finally { $('run-checkin').disabled = false; } });
$('start-login').addEventListener('click', async () => { $('start-login').disabled = true; try { const result = await api('/admin/account/url', { method:'POST' }); const link = $('login-link'); link.href = result.url; link.textContent = result.url; link.hidden = false; $('poll-login').disabled = false; $('account-state').textContent = '等待授权'; window.open(result.url, '_blank', 'noopener'); } catch (e) { $('account-state').textContent = e.message; } finally { $('start-login').disabled = false; } });
$('poll-login').addEventListener('click', async () => { $('poll-login').disabled = true; try { const result = await api('/admin/account/poll', { method:'POST' }); $('account-state').textContent = `已添加 ${result.nickname || result.uid}`; await refresh(); } catch (e) { $('account-state').textContent = e.message; $('poll-login').disabled = false; } });
$('unlock-form').addEventListener('submit', async event => { event.preventDefault(); const button = $('unlock'); button.disabled = true; $('unlock-error').textContent = ''; try { const response = await fetch('/admin/unlock', { method:'POST', headers:{'Content-Type':'application/json'}, body:JSON.stringify({password:$('frontend-password').value}) }); const result = await response.json().catch(() => ({})); if (!response.ok) throw new Error(result.error || '密码错误'); $('lock-dialog').close(); $('frontend-password').value = ''; await refresh(); loadAdminConfig(); } catch (e) { $('unlock-error').textContent = e.message; } finally { button.disabled = false; } });
loadAdminConfig();
