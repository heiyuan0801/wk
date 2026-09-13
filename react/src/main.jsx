import React, { useEffect, useMemo, useRef, useState } from 'react';
import { createRoot } from 'react-dom/client';
import {
  Alert, App as AntApp, Button, Card, ConfigProvider, Empty, Form, Input, Layout, Menu, Modal,
  Select, Space, Statistic, Switch, Table, Tag, Tooltip, Typography, message,
} from 'antd';
import {
  ApiOutlined, CheckCircleOutlined, DashboardOutlined, FileSearchOutlined, KeyOutlined,
  ReloadOutlined, SendOutlined, DeleteOutlined, SettingOutlined, StopOutlined,
  ThunderboltOutlined, ToolOutlined, UnlockOutlined, SafetyOutlined, CloudServerOutlined,
  UserAddOutlined,
} from '@ant-design/icons';
import 'antd/dist/reset.css';
import './theme.css';
import { orderCaptchaOptions, runTencentCaptcha } from './captcha';
import AutoEnrollPage from './pages/AutoEnroll';
import ProxyPoolPage from './pages/ProxyPool';

const { Header, Sider, Content } = Layout;
const { Title, Text, Paragraph } = Typography;
const fmt = value => Number(value || 0).toLocaleString();
const fmtCredits = value => Number(value || 0).toFixed(4);
const parseHours = value => String(value || '').split(',').map(item => item.trim()).filter(Boolean).map(Number);
const initialAPIKey = sessionStorage.getItem('wb2api-api-key') || localStorage.getItem('wb2api-api-key') || '';
localStorage.removeItem('wb2api-api-key');

// 冷却原因文案：与后端 CoolKind.String() 的取值一一对应。
const COOL_KIND_LABEL = {
  hard_credit: '积分耗尽',
  soft_rate: '429 限流',
  rate_limit: '上游限流',
};

// 页面标识。用 hash 路由（#/auto-enroll）而不是给每页单独打包：
// 刷新能停在原页、地址可收藏转发，且不引入 react-router 依赖。
const SECTIONS = ['dashboard', 'models', 'playground', 'requests', 'auto-enroll', 'proxy', 'settings'];

// sectionFromHash 读取地址栏里的页面标识；非法/缺失时回落到仪表盘。
function sectionFromHash() {
  const raw = String(window.location.hash || '').replace(/^#\/?/, '').trim();
  return SECTIONS.includes(raw) ? raw : 'dashboard';
}

// fmtDuration 把秒数格式化成紧凑的中文时长文案。
function fmtDuration(seconds) {
  const total = Math.max(0, Math.floor(Number(seconds) || 0));
  if (!total) return '即将到期';
  const h = Math.floor(total / 3600);
  const m = Math.floor((total % 3600) / 60);
  const s = total % 60;
  if (h) return `${h} 小时 ${m} 分`;
  if (m) return `${m} 分 ${s} 秒`;
  return `${s} 秒`;
}

function fmtTime(value) {
  if (!value) return '-';
  const date = new Date(value);
  return Number.isNaN(date.getTime()) ? '-' : date.toLocaleString();
}

// parseDeadline 解析后端 time.Time 字段为毫秒时间戳；零值返回 0。
// 注意：Go 的 time.Time 是结构体，`omitempty` 对它无效 —— 未设置时后端会上送
// "0001-01-01T00:00:00Z"，所以必须显式识别零值，否则正常账号会被误判为"熔断中"。
function parseDeadline(value) {
  if (!value) return 0;
  const date = new Date(value);
  if (Number.isNaN(date.getTime())) return 0;
  return date.getUTCFullYear() <= 1 ? 0 : date.getTime();
}

// token 剩余有效期文案：后端给的是 Unix 秒。10 分钟内标红提示即将失效。
function tokenExpiryLabel(expiresAt) {
  if (!expiresAt) return null;
  const remaining = expiresAt - Math.floor(Date.now() / 1000);
  if (remaining <= 0) return { text: '已过期', danger: true };
  return { text: `${fmtDuration(remaining)}后过期`, danger: remaining < 600 };
}

// LiveCountdown 以「绝对截止时间」为基准每秒自减。
// 只让本组件按秒重渲染，避免整张账号表每秒重绘。
function LiveCountdown({ deadline, fallbackSec = 0 }) {
  const compute = () => (deadline
    ? Math.max(0, Math.round((deadline - Date.now()) / 1000))
    : Math.max(0, Math.round(fallbackSec)));
  const [remaining, setRemaining] = useState(compute);
  useEffect(() => {
    setRemaining(compute());
    if (!deadline) return undefined;
    const timer = setInterval(() => setRemaining(compute()), 1000);
    return () => clearInterval(timer);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [deadline, fallbackSec]);
  return <span>{fmtDuration(remaining)}</span>;
}

// coolingPortrait 统一计算账号的冷却画像。
// 关键点：后端的 cool_remaining_sec **只反映 until**，而 cooling 是
// (until 生效 || breaker_until 生效)。纯熔断账号（until 已过期、breaker 仍生效）
// 若只读剩余秒数会误显示"即将到期"，因此这里用两个绝对截止时间取较晚者。
function coolingPortrait(record) {
  const now = Date.now();
  const untilMs = parseDeadline(record.until);
  const breakerMs = parseDeadline(record.breaker_until);
  const untilActive = untilMs > now;
  const breakerActive = breakerMs > now;
  const deadline = Math.max(untilActive ? untilMs : 0, breakerActive ? breakerMs : 0);
  return {
    untilActive,
    breakerActive,
    deadline: deadline || 0,
    reason: record.reason || '',
    kind: COOL_KIND_LABEL[record.cool_kind] || record.cool_kind || '',
  };
}

function Sparkline({ values, color = '#356ae6', ariaLabel = '趋势图' }) {
  if (!values.length) return <Empty image={Empty.PRESENTED_IMAGE_SIMPLE} description="等待采样" />;
  const width = 220;
  const height = 52;
  const min = Math.min(...values);
  const max = Math.max(...values);
  const span = max - min || 1;
  const points = values.map((value, index) => {
    const x = values.length === 1 ? width / 2 : (index / (values.length - 1)) * width;
    const y = height - ((value - min) / span) * (height - 8) - 4;
    return `${x.toFixed(1)},${y.toFixed(1)}`;
  }).join(' ');
  return <svg className="sparkline" viewBox={`0 0 ${width} ${height}`} role="img" aria-label={ariaLabel}><polyline fill="none" stroke={color} strokeWidth="3" strokeLinecap="round" strokeLinejoin="round" points={points} /></svg>;
}

function Console() {
  // antd v5 的静态 message / Modal 依赖 react-dom 的 render，而 React 19 已移除它
  // （react-dom 只导出 createRoot 于 react-dom/client）。静态调用因此静默失效，
  // 表现为"点了按钮没有任何反馈"。必须改用 App 上下文提供的实例。
  const { message, modal } = AntApp.useApp();
  const [form] = Form.useForm();
  const [apiKey, setApiKey] = useState(initialAPIKey);
  const [apiKeyOpen, setApiKeyOpen] = useState(false);
  const [apiKeyDraft, setApiKeyDraft] = useState(initialAPIKey);
  const [locked, setLocked] = useState(false);
  const [password, setPassword] = useState('');
  const [activeSection, setActiveSection] = useState(() => sectionFromHash());  const [data, setData] = useState({ accounts: [], metrics: {}, total: 0, healthy: 0, cooling: 0, disabled: 0 });
  const [models, setModels] = useState([]);
  const [requestLogs, setRequestLogs] = useState([]);
  const [requestSearch, setRequestSearch] = useState('');
  const [requestModeFilter, setRequestModeFilter] = useState('all');
  const [requestStatusFilter, setRequestStatusFilter] = useState('all');
  const [config, setConfig] = useState({ checkin_hours: [9, 21], keepalive_hours: [22], request_logs: { retention_days: 30 }, region: 'cn', update: { repository: 'heiyuan0801/wk', branch: 'main' } });
  const [smsSettings, setSmsSettings] = useState({
    twoCaptchaKey: '',
    proxy: { url: '', file: '', cooldown: '30m', region: '', inject_sid: false, sticky_minutes: 30 },
    haozhuma: { user: '', pass: '', token: '', sid: '', author: '', uid: '', isp: '' },
  });
  const [updateInfo, setUpdateInfo] = useState(null);
  const [loginRegion, setLoginRegion] = useState('cn');
  const [selectedModel, setSelectedModel] = useState('');
  const [promptText, setPromptText] = useState('你好，请简短介绍一下你自己。');
  const [answer, setAnswer] = useState('');
  const [loading, setLoading] = useState(false);
  const [stream, setStream] = useState(true);
  const [requestInfo, setRequestInfo] = useState(null);
  const [metricSamples, setMetricSamples] = useState([]);
  const [creditRefreshing, setCreditRefreshing] = useState(false);
  const [checkinRunning, setCheckinRunning] = useState(false);
  const [apiKeyResetting, setApiKeyResetting] = useState(false);
  const [updateStarting, setUpdateStarting] = useState(false);
  const [accountAction, setAccountAction] = useState('');
  const [accountSearch, setAccountSearch] = useState('');
  const [accountStatusFilter, setAccountStatusFilter] = useState('all');
  const [accountRegionFilter, setAccountRegionFilter] = useState('all');
  const [loginURL, setLoginURL] = useState('');
  const [loginPendingRegion, setLoginPendingRegion] = useState('');
  const [loginPolling, setLoginPolling] = useState(false);
  // 短信直登状态：手机号 → 发码 → 验证码 → 落盘。
  const [smsMobile, setSmsMobile] = useState('');
  const [smsCode, setSmsCode] = useState('');
  const [smsSession, setSmsSession] = useState('');
  // smsCaptcha 非空表示这次发码被人机校验拦下：必须先过码才会真的发短信。
  // 形状为 { options: [{appId, cloudType}], reason, auto_attempted }。
  const [smsCaptcha, setSmsCaptcha] = useState(null);
  const [smsCaptchaBusy, setSmsCaptchaBusy] = useState(false);
  const [smsSending, setSmsSending] = useState(false);
  const [smsVerifying, setSmsVerifying] = useState(false);
  const [smsCountdown, setSmsCountdown] = useState(0);
  const loginRegionTouched = useRef(false);
  const loginPollAbort = useRef(false);
  const refreshController = useRef(null);
  const refreshSerial = useRef(0);
  const requestController = useRef(null);
  const requestSerial = useRef(0);

  const headers = useMemo(() => (apiKey ? { Authorization: `Bearer ${apiKey}` } : {}), [apiKey]);
  const api = async (path, options = {}) => {
    const response = await fetch(path, { ...options, headers: { ...headers, ...(options.headers || {}) } });
    const body = await response.json().catch(() => ({}));
    if (!response.ok) {
      // 把后端的结构化错误字段带出来：retryable 决定是否保留当前短信会话，
      // existing/disabled 用于"该手机号已在号池中"的确认弹窗。
      const error = Error(body?.error?.message || body?.error || `${response.status}`);
      error.status = response.status;
      error.retryable = body?.retryable === true;
      error.reason = body?.reason || '';
      error.existing = body?.existing === true;
      error.existingUid = body?.uid || '';
      error.existingNickname = body?.nickname || '';
      error.existingDisabled = body?.disabled === true;
      throw error;
    }
    return body;
  };

  const refresh = async () => {
    const serial = ++refreshSerial.current;
    refreshController.current?.abort();
    const controller = new AbortController();
    refreshController.current = controller;
    try {
      const [status, modelList, requestList] = await Promise.all([
        api('/status', { signal: controller.signal }),
        api('/v1/models', { signal: controller.signal }),
        api('/requests?limit=200', { signal: controller.signal }).catch(() => ({ data: [] })),
      ]);
      if (serial !== refreshSerial.current) return;
      setData(status);
      setModels(modelList.data || []);
      setRequestLogs(requestList.data || []);
      setSelectedModel(current => current || modelList.data?.[0]?.id || '');
      const metrics = status.metrics || {};
      const inputTokens = Number(metrics.input_tokens || 0);
      const cacheRate = inputTokens ? (Number(metrics.cache_read_tokens || 0) / inputTokens) * 100 : 0;
      setMetricSamples(current => [...current, { at: Date.now(), cacheRate, avgTTFB: Number(metrics.avg_ttfb_ms || 0) }].slice(-24));
      setLocked(false);
    } catch (error) {
      if (error.name === 'AbortError') return;
      if (error.message.includes('invalid') || error.message.includes('password')) setLocked(true);
      return;
    }
    try {
      const currentConfig = await api('/admin/config', { signal: controller.signal });
      if (serial !== refreshSerial.current) return;
      if (currentConfig.schedule || currentConfig.region || currentConfig.request_logs || currentConfig.sms || currentConfig.update) {
        setConfig(current => ({ ...current, ...(currentConfig.schedule || {}), request_logs: currentConfig.request_logs || current.request_logs || { retention_days: 30 }, update: (currentConfig.update && (currentConfig.update.repository || currentConfig.update.branch)) ? currentConfig.update : current.update, region: currentConfig.region || current.region || 'cn' }));
      }
      if (currentConfig.sms) {
        setSmsSettings(current => ({
          ...current,
          // Proxy URLs may contain credentials and are intentionally masked by
          // the API. Keep the local field empty so saving the form preserves
          // the server-side value instead of exposing or overwriting it.
          proxy: { ...current.proxy, ...(currentConfig.sms.proxy || {}), url: '', lines: '' },
          haozhuma: { ...current.haozhuma, ...(currentConfig.sms.haozhuma || {}) },
          twoCaptchaConfigured: currentConfig.sms.two_captcha_configured === true,
        }));
      }
      if (!loginRegionTouched.current && currentConfig.region === 'global') setLoginRegion('global');
    } catch (error) {
      if (error.name === 'AbortError') return;
      // Dashboard data can still refresh when configuration is unavailable.
    }
  };

  useEffect(() => {
    refresh();
    const timer = setInterval(refresh, 30000);
    return () => clearInterval(timer);
  }, [apiKey]);

  useEffect(() => {
    form.setFieldsValue({
      checkin: (config.checkin_hours || []).join(','),
      keepalive: (config.keepalive_hours || []).join(','),
      retention: config.request_logs?.retention_days ?? 30,
    });
  }, [config, form]);

  // hash 路由：切换页面时写回地址栏；浏览器前进/后退（hashchange）时同步
  // 回 state。两处都必要——只写不同步会让后退键失效。
  useEffect(() => {
    const next = `#/${activeSection}`;
    if (window.location.hash !== next) window.location.hash = next;
  }, [activeSection]);

  useEffect(() => {
    const onHashChange = () => setActiveSection(sectionFromHash());
    window.addEventListener('hashchange', onHashChange);
    return () => window.removeEventListener('hashchange', onHashChange);
  }, []);

  // 短信验证码重发倒计时：上游 60 秒内会拒绝重复发码。
  useEffect(() => {
    if (smsCountdown <= 0) return undefined;
    const timer = setTimeout(() => setSmsCountdown(value => value - 1), 1000);
    return () => clearTimeout(timer);
  }, [smsCountdown]);

  useEffect(() => () => {
    refreshController.current?.abort();
    requestController.current?.abort();
  }, []);

  const unlock = async () => {
    if (!password.trim() && apiKey.trim()) {
      sessionStorage.setItem('wb2api-api-key', apiKey.trim());
      setLocked(false);
      await refresh();
      return;
    }
    try {
      const response = await fetch('/admin/unlock', {
        method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ password }),
      });
      if (!response.ok) {
        const body = await response.json();
        throw Error(body.error || '密码错误');
      }
      setLocked(false);
      setPassword('');
      await refresh();
      message.success('控制台已解锁');
    } catch (error) {
      message.error(error.message);
    }
  };

  const saveAPIKey = () => {
    const next = apiKeyDraft.trim();
    setApiKey(next);
    if (next) sessionStorage.setItem('wb2api-api-key', next);
    else {
      sessionStorage.removeItem('wb2api-api-key');
      localStorage.removeItem('wb2api-api-key');
    }
    setApiKeyOpen(false);
    message.success(next ? 'API Key 已保存' : 'API Key 已清除');
  };

  const resetAPIKey = async () => {
    setApiKeyResetting(true);
    try {
      const result = await api('/admin/api-key/reset', { method: 'POST' });
      const next = String(result.api_key || '');
      if (!next) throw Error('服务器没有返回新 API Key');
      setApiKeyDraft(next);
      setApiKey(next);
      sessionStorage.setItem('wb2api-api-key', next);
      message.success('服务器 API Key 已重置并保存');
    } catch (error) {
      message.error(`API Key 重置失败：${error.message}`);
    } finally {
      setApiKeyResetting(false);
    }
  };

  const updateService = async () => {
    setUpdateStarting(true);
    try {
      const result = await api('/admin/update', { method: 'POST' });
      message.success(result.message || '更新任务已启动');
    } catch (error) {
      message.error(`更新未启动：${error.message}`);
    } finally {
      setUpdateStarting(false);
    }
  };

  const checkUpdate = async () => {
    try {
      const result = await api('/admin/update/check');
      setUpdateInfo(result);
      message.success(result.update_available ? `发现新版本 ${result.latest.slice(0, 7)}` : '当前已是最新版本');
    } catch (error) {
      message.error(`检查 GitHub 版本失败：${error.message}`);
    }
  };

  const saveConfig = async values => {
    try {
      const nextConfig = {
        checkin_hours: parseHours(values.checkin),
        keepalive_hours: parseHours(values.keepalive),
        request_logs: { retention_days: Number(values.retention) },
        sms: {
          two_captcha_key: smsSettings.twoCaptchaKey || undefined,
          proxy: { ...smsSettings.proxy, lines: smsSettings.proxy.lines || undefined },
          haozhuma: smsSettings.haozhuma,
        },
        update: config.update,
      };
      const invalidHours = [nextConfig.checkin_hours, nextConfig.keepalive_hours].some(hours => (
        !hours.length || hours.some(hour => !Number.isInteger(hour) || hour < 0 || hour > 23)
      ));
      const invalid = invalidHours || !Number.isInteger(nextConfig.request_logs.retention_days) || nextConfig.request_logs.retention_days < 0 || nextConfig.request_logs.retention_days > 3650;
      if (invalid) {
        message.error('每项至少填写一个 0-23 的整数小时');
        return;
      }
      const result = await api('/admin/config', {
        method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(nextConfig),
      });
      setConfig(current => ({ ...current, ...(result.schedule || nextConfig), request_logs: result.request_logs || nextConfig.request_logs }));
      message.success('配置已保存；短信/自动加号参数将在服务重启后生效');
    } catch (error) {
      message.error(error.message);
    }
  };

  const refreshCredits = async () => {
    setCreditRefreshing(true);
    try {
      const result = await api('/admin/credits/refresh', { method: 'POST' });
      message.success(result.message || '上游积分刷新已启动');
      window.setTimeout(refresh, 2500);
    } catch (error) {
      message.error(error.message);
    } finally {
      setCreditRefreshing(false);
    }
  };

  const runCheckinAll = async () => {
    setCheckinRunning(true);
    try {
      const result = await api('/admin/checkin', { method: 'POST' });
      message.success(result.message || '签到任务已启动');
      window.setTimeout(refresh, 2500);
    } catch (error) {
      message.error(error.message);
    } finally {
      setCheckinRunning(false);
    }
  };

  const send = async () => {
    const serial = ++requestSerial.current;
    requestController.current?.abort();
    const controller = new AbortController();
    requestController.current = controller;
    const startedAt = performance.now();
    let firstTokenAt = 0;
    setLoading(true);
    setAnswer('');
    setRequestInfo({ state: 'running', duration: 0, ttft: 0, usage: null });
    try {
      const response = await fetch('/v1/responses', {
        method: 'POST', signal: controller.signal,
        headers: { 'Content-Type': 'application/json', ...headers },
        body: JSON.stringify({ model: selectedModel, input: promptText, stream }),
      });
      if (!response.ok) {
        const errorBody = await response.json().catch(() => ({}));
        throw Error(errorBody?.error?.message || errorBody?.error || `请求失败 (${response.status})`);
      }
      let finalResponse = null;
      if (!stream) {
        finalResponse = await response.json();
        setAnswer(finalResponse.output_text || JSON.stringify(finalResponse, null, 2));
      } else {
        const reader = response.body?.getReader();
        if (!reader) throw Error('浏览器不支持流式响应');
        const decoder = new TextDecoder();
        let buffer = '';
        let finished = false;
        while (!finished) {
          const chunk = await reader.read();
          finished = chunk.done;
          buffer += decoder.decode(chunk.value || new Uint8Array(), { stream: !finished });
          const frames = buffer.split('\n\n');
          buffer = frames.pop() || '';
          for (const frame of frames) {
            const payload = frame.split('\n')
              .filter(line => line.startsWith('data:'))
              .map(line => line.slice(5).trim())
              .join('\n');
            if (!payload || payload === '[DONE]') continue;
            let event;
            try { event = JSON.parse(payload); } catch { continue; }
            if (event.type === 'response.output_text.delta' && event.delta) {
              if (!firstTokenAt) firstTokenAt = performance.now();
              setAnswer(current => current + event.delta);
            }
            if (event.type === 'response.completed' || event.type === 'response.incomplete') {
              finalResponse = event.response;
              if (!firstTokenAt && finalResponse?.output_text) firstTokenAt = performance.now();
              if (finalResponse?.output_text) {
                setAnswer(finalResponse.output_text);
              } else if (finalResponse?.output) {
                setAnswer(JSON.stringify(finalResponse.output, null, 2));
              }
            }
            if (event.type === 'response.failed') {
              throw Error(event.response?.error?.message || '上游响应失败');
            }
          }
        }
      }
      if (serial !== requestSerial.current) return;
      setRequestInfo({
        state: finalResponse?.status || 'completed',
        duration: performance.now() - startedAt,
        ttft: firstTokenAt ? firstTokenAt - startedAt : 0,
        usage: finalResponse?.usage || null,
        responseID: finalResponse?.id || '',
      });
      await refresh();
    } catch (error) {
      if (serial !== requestSerial.current) return;
      if (error.name === 'AbortError') {
        setRequestInfo({ state: 'cancelled', duration: performance.now() - startedAt, ttft: firstTokenAt ? firstTokenAt - startedAt : 0, usage: null });
        return;
      }
      setAnswer(error.message);
      setRequestInfo({ state: 'failed', duration: performance.now() - startedAt, ttft: firstTokenAt ? firstTokenAt - startedAt : 0, usage: null, error: error.message });
    } finally {
      if (serial === requestSerial.current) setLoading(false);
    }
  };

  const runAccountAction = async (record, action) => {
    const uid = record.uid || '';
    const isDelete = action === 'delete';
    const isEnable = action === 'enable';
    setAccountAction(`${uid}:${action}`);
    try {
      const path = isDelete ? `/admin/account/${encodeURIComponent(uid)}` : `/admin/account/${encodeURIComponent(uid)}/${action}`;
      const result = await api(path, { method: isDelete ? 'DELETE' : 'POST' });
      if (isEnable) {
        setData(current => ({
          ...current,
          accounts: (current.accounts || []).map(account => account.uid === uid
            ? { ...account, disabled: false, cooling: false, reason: '' }
            : account),
          disabled: Math.max(0, Number(current.disabled || 0) - (record.disabled ? 1 : 0)),
          healthy: Number(current.healthy || 0) + (record.disabled ? 1 : 0),
        }));
      }
      const sessionWarning = isEnable && /12153|session dead/i.test(record.reason || '')
        ? '账号已启用，但凭证曾失效；如果再次被禁用，请重新授权登录。'
        : isDelete ? '账号已删除'
          : isEnable ? '账号已手动启用'
            : action === 'clear-cooldown' ? (result?.message || '冷却与熔断已清除')
              : '账号已禁用';
      message.success(sessionWarning);
      await refresh();
      return result;
    } catch (error) {
      message.error(error.message);
      return null;
    } finally {
      setAccountAction('');
    }
  };

  // runUpstreamAction 处理签到/保活：两者共用一个后端口径 —— HTTP 200 表示
  // 请求已送达，真正的结果由 ok/detail 表达。上游不可达时 ok=false 但接口是通的，
  // 因此这里按 ok 决定提示颜色，而不是按异常处理。
  const runUpstreamAction = async (record, action) => {
    const uid = record.uid || '';
    const label = action === 'checkin' ? '签到' : '保活';
    setAccountAction(`${uid}:${action}`);
    try {
      const result = await api(`/admin/account/${encodeURIComponent(uid)}/${action}`, { method: 'POST' });
      if (result.ok) message.success(`${label}成功`);
      else message.warning(`${label}未完成：${result.detail || '上游未返回成功'}`);
      await refresh();
    } catch (error) {
      message.error(`${label}失败：${error.message}`);
    } finally {
      setAccountAction('');
    }
  };

  const accountActionHandler = (record, action) => {
    // 启用是可逆操作，直接执行并立即反馈；删除和禁用仍需二次确认。
    if (action === 'enable') {
      runAccountAction(record, action);
      return;
    }
    if (action === 'checkin' || action === 'keepalive') {
      runUpstreamAction(record, action);
      return;
    }
    if (action === 'clear-cooldown') {
      runAccountAction(record, action);
      return;
    }
    const uid = record.uid || '';
    const isDelete = action === 'delete';
    modal.confirm({
      title: isDelete ? '确认删除账号？' : '确认禁用账号？',
      content: isDelete
        ? `账号 ${uid.slice(0, 12)} 将从账号池和本地授权文件中删除，删除后需要重新登录才能恢复。`
        : `账号 ${uid.slice(0, 12)} 将停止接收新请求，之后可以手动启用。`,
      okText: isDelete ? '删除' : '禁用',
      cancelText: '取消',
      okButtonProps: { danger: true },
      onOk: () => runAccountAction(record, action),
    });
  };

  // startLoginPoll 打开授权链接后自动轮询授权结果。
  // 手动轮询容易忘，而 OAuth 完成后不轮询账号不会入库；这里每 3 秒查一次，
  // 最长约 3 分钟后放弃（避免永久占用一个定时器）。
  const startLoginPoll = async (region) => {
    loginPollAbort.current = false;
    setLoginPolling(true);
    const query = `?region=${encodeURIComponent(region)}`;
    let added = false;
    try {
      for (let attempt = 0; attempt < 60; attempt += 1) {
        if (loginPollAbort.current) break;
        // 首次立刻尝试，之后每 3 秒一次。
        if (attempt > 0) await new Promise(resolve => setTimeout(resolve, 3000));
        if (loginPollAbort.current) break;
        try {
          const result = await api(`/admin/account/poll${query}`, { method: 'POST' });
          if (result.ok) {
            if (result.warning) message.warning(result.warning);
            else message.success(`${result.region === 'global' ? '海外版' : '中国区'}账号已添加`);
            added = true;
            break;
          }
        } catch {
          // 单次失败（含 HTTP 409「尚未授权」）继续轮询，不打断。
        }
      }
      if (!added && !loginPollAbort.current) {
        message.warning('自动轮询已超时，完成浏览器授权后可手动点击「轮询授权结果」。');
      }
    } finally {
      setLoginPolling(false);
      setLoginURL('');
      setLoginPendingRegion('');
      await refresh();
    }
  };

  // runSMSSend 发送短信验证码：成功后会拿到 session_id，验码时回传。
  // 同一个按钮同时承担"重发"：上游对 unexpired 的号码不会重复发短信，
  // 只会换发新的 state_token，所以重发是安全的。
  const runSMSSend = async (options = {}) => {
    const mobile = smsMobile.trim();
    if (!mobile) { message.warning('请先填写手机号'); return; }
    const isResend = options.resend === true && !!smsSession;
    setSmsSending(true);
    try {
      const result = await api('/admin/account/sms/send', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ mobile, region: 'cn', force: options.force === true }),
      });
      setSmsSession(result.session_id || '');
      setSmsCode('');
      // 被要求人机校验：还没有真正发码，先把验证码交给用户过。
      if (result.captcha) {
        setSmsCaptcha(result.captcha);
        setSmsCountdown(0);
        message.warning(result.captcha.reason || '该号码需要人机校验，请完成验证');
        return;
      }
      setSmsCaptcha(null);
      // 上游约 60 秒内不接受重复发码；用 expires_in 驱动倒计时更贴近真实窗口。
      const wait = Number(result.expires_in) > 0 ? Math.min(Number(result.expires_in), 60) : 60;
      setSmsCountdown(wait);
      if (result.status === 'unexpired') {
        // 未过期说明短信已经发过了，不会再来一条新的。
        message.success(isResend
          ? '已刷新会话，请使用已收到的那条验证码'
          : `该号码已有未过期验证码，请直接输入${result.expires_in ? `（约 ${Math.ceil(result.expires_in / 60)} 分钟内有效）` : ''}`);
      } else {
        message.success(isResend ? '验证码已重新发送，请查收短信' : '验证码已发送，请查收短信');
      }
    } catch (error) {
      if (error.status === 409 && error.existing) {
        // 该号码已在号池中。后端在发短信之前就拦下了，所以这里还能让用户反悔，
        // 不至于白消耗一条短信。
        const label = error.existingNickname || error.existingUid?.slice(0, 12) || '该账号';
        modal.confirm({
          title: '该手机号已在号池中',
          content: error.existingDisabled
            ? `账号 ${label} 已存在，且当前处于禁用状态。重新登录会更新凭证，但不会自动启用——需要你在账号列表里手动启用后才会接流量。仍要重新登录吗？`
            : `账号 ${label} 已存在。继续登录会刷新它的凭证（积分与冷却状态保持不变），不会新增账号。仍要继续吗？`,
          okText: '继续登录',
          cancelText: '取消',
          onOk: () => runSMSSend({ resend: isResend, force: true }),
        });
      } else if (error.reason === 'too_frequent') {
        // 频控：旧验证码仍然有效，不必清空会话。
        message.warning(`${error.message}（已发出的验证码仍然有效）`);
        setSmsCountdown(30);
      } else {
        message.error(error.message);
      }
    } finally {
      setSmsSending(false);
    }
  };

  // submitCaptcha 把用户完成的验证码票据回传后端，由后端继续发码。
  //
  // 上游会给出多个方案（实测 teg + tencent）。从非 codebuddy.cn 域名发起时，
  // teg 那条会被腾讯以 403 拒绝（SDK 回调带 trerror_* 错误票据），而 tencent
  // 正常。因此这里在某个方案启动失败时自动顺延到下一个，用户不必自己判断
  // 该点哪个——按钮仍然都列出来，供需要时手动选择。
  const submitCaptcha = async (option, options = {}) => {
    if (!smsSession) { message.warning('会话已失效，请重新发送验证码'); return; }
    setSmsCaptchaBusy(true);
    try {
      const { ticket, randStr, cloudType } = await runTencentCaptcha(option.appId, option.cloudType);
      const result = await api('/admin/account/sms/captcha', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
          session_id: smsSession, ticket, rand_str: randStr, cloud_type: cloudType,
        }),
      });
      // 票据过期时后端会再给一次挑战，此时保留会话让用户重试即可。
      if (result.captcha) {
        setSmsCaptcha(result.captcha);
        message.warning(result.captcha.reason || '验证码已过期，请重新完成校验');
        return;
      }
      setSmsCaptcha(null);
      setSmsCode('');
      const wait = Number(result.expires_in) > 0 ? Math.min(Number(result.expires_in), 60) : 60;
      setSmsCountdown(wait);
      message.success(result.status === 'unexpired'
        ? '校验通过，短信已发送，请查收'
        : '校验通过，验证码已发送，请查收');
    } catch (error) {
      // 用户主动关掉验证码弹窗不该报错，只提示一下即可。
      if (error.cancelled) {
        message.info(error.message);
        return;
      }
      // 该方案启动失败（如 teg 在我们域名下被拒）时自动试下一个方案。
      const rest = (smsCaptcha?.options || []).filter(o => o.appId !== option.appId);
      if (!options.lastResort && rest.length) {
        message.info(`${error.message}，正在尝试其他验证方式…`);
        await submitCaptcha(rest[0], { lastResort: rest.length === 1 });
        return;
      }
      message.error(error.message);
    } finally {
      setSmsCaptchaBusy(false);
    }
  };

  // runSMSVerify 提交验证码：后端会走完 OneID → Keycloak → Console 全部步骤并落盘。
  const runSMSVerify = async () => {
    if (!smsSession) { message.warning('请先发送验证码'); return; }
    const code = smsCode.trim();
    if (!code) { message.warning('请填写收到的验证码'); return; }
    setSmsVerifying(true);
    try {
      const result = await api('/admin/account/sms/verify', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ session_id: smsSession, code }),
      });
      // notice 用于"该账号已存在"这类需要用户知晓但不阻断流程的提示；
      // warning 是配置未能持久化等真问题，优先级更高。
      if (result.warning) message.warning(result.warning);
      else if (result.notice) message.info(result.notice);
      else if (result.existed) message.success(`账号 ${result.nickname || result.uid?.slice(0, 12)} 的凭证已更新`);
      else message.success(`账号 ${result.nickname || result.uid?.slice(0, 12)} 已添加`);
      setSmsSession('');
      setSmsCode('');
      setSmsMobile('');
      setSmsCaptcha(null);
      setSmsCountdown(0);
      await refresh();
    } catch (error) {
      message.error(error.message);
      // 还没过码就点了验码：把用户导回验证码步骤，会话仍然可用。
      if (error.reason === 'captcha_pending') {
        setSmsCaptcha(prev => prev || { options: [], reason: '请先完成人机校验' });
        return;
      }
      // 验证码填错/频控时上游的 state_token 依然有效，保留会话让用户直接重填，
      // 不必浪费一条短信。只有会话真的失效（410）才清空并要求重新发码。
      const keepSession = error.retryable === true && error.status !== 410;
      if (keepSession) {
        setSmsCode('');
        if (error.reason === 'too_frequent') setSmsCountdown(5);
      } else {
        setSmsSession('');
        setSmsCaptcha(null);
        setSmsCountdown(0);
      }
    } finally {
      setSmsVerifying(false);
    }
  };

  const columns = [
    {
      title: '账号', dataIndex: 'nickname',
      render: (value, record) => (
        <Space direction="vertical" size={0}>
          <Text strong>{value || '未命名'}</Text>
          <Text type="secondary" code>{record.uid?.slice(0, 12)}</Text>
        </Space>
      ),
    },
    {
      title: '区域',
      dataIndex: 'region',
      render: value => <Tag color={value === 'global' ? 'blue' : 'default'}>{value === 'global' ? '海外版' : '中国区'}</Tag>,
    },
    {
      title: '状态',
      filters: [
        { text: '可用', value: 'healthy' },
        { text: '冷却', value: 'cooling' },
        { text: '模型限流', value: 'model_cooling' },
        { text: '禁用', value: 'disabled' },
      ],
      onFilter: (value, record) => {
        const portrait = coolingPortrait(record);
        const modelCooling = Object.keys(record.model_cooldowns || {}).length > 0;
        if (value === 'disabled') return !!record.disabled;
        if (value === 'cooling') return !record.disabled && portrait.untilActive;
        if (value === 'model_cooling') return !record.disabled && modelCooling;
        return !record.disabled && !portrait.untilActive && !portrait.breakerActive && !modelCooling;
      },
      render: (_, record) => {
        const portrait = coolingPortrait(record);
        const modelLimits = Object.entries(record.model_cooldowns || {});
        const modelLimitText = modelLimits.map(([model, limit]) => {
          const modelUntil = parseDeadline(limit?.until);
          const recovery = modelUntil
            ? `恢复：${new Date(modelUntil).toLocaleString()}`
            : (Number(limit?.remaining_sec) > 0 ? `${limit.remaining_sec} 秒后重试` : '等待上游恢复');
          return `${model}：${limit?.reason || '上游限流'}（${recovery}）`;
        });
        const modelCooling = modelLimits.length > 0;
        // 纯熔断账号（until 已过期、breaker 仍生效）单独标注，避免与普通冷却混淆。
        const pureBreaker = !portrait.untilActive && portrait.breakerActive && !record.disabled;
        const label = record.disabled ? '禁用'
          : pureBreaker ? '熔断退避'
            : portrait.untilActive ? (record.cool_kind === 'rate_limit' ? '上游限流' : '冷却')
              : modelCooling ? '模型限流' : '可用';
        const color = record.disabled ? 'red'
          : pureBreaker ? 'volcano'
            : portrait.untilActive ? 'orange'
              : modelCooling ? 'gold' : 'green';
        const title = [
          portrait.reason && `原因：${portrait.reason}`,
          portrait.kind && `类型：${portrait.kind}`,
          record.breaker_fails > 0 && `熔断计数：${record.breaker_fails}`,
          modelLimitText.length > 0 && `模型限流：\n${modelLimitText.join('\n')}`,
        ].filter(Boolean).join('\n');
        return <Tag color={color} title={title || undefined}>{label}</Tag>;
      },
    },
    {
      title: '原因 / 剩余',
      width: 190,
      // 冷却是瞬时状态，这里按秒自减展示，而不是等 30 秒轮询才更新。
      render: (_, record) => {
        const portrait = coolingPortrait(record);
        if (record.disabled) {
          return record.reason
            ? <Text type="danger" title={record.reason}>{record.reason}</Text>
            : <Text type="secondary">人工禁用</Text>;
        }
        if (!portrait.untilActive && !portrait.breakerActive) {
          return record.reason ? <Text type="secondary">{record.reason}</Text> : <Text type="secondary">-</Text>;
        }
        return (
          <Space direction="vertical" size={0}>
            <Text title={portrait.reason || undefined}>
              {portrait.untilActive ? (portrait.kind || '冷却中') : '熔断退避'}
            </Text>
            <Text type="secondary">
              剩余 <LiveCountdown deadline={portrait.deadline} fallbackSec={record.cool_remaining_sec} />
            </Text>
          </Space>
        );
      },
    },
    {
      title: '操作',
      key: 'actions',
      fixed: 'right',
      width: 300,
      render: (_, record) => {
        const uid = record.uid || '';
        const busy = accountAction.startsWith(`${uid}:`);
        const portrait = coolingPortrait(record);
        const cooling = portrait.untilActive || portrait.breakerActive;
        return (
          <Space size={4} wrap>
            <Button
              size="small"
              type="link"
              icon={record.disabled ? <UnlockOutlined /> : <StopOutlined />}
              loading={busy}
              onClick={() => accountActionHandler(record, record.disabled ? 'enable' : 'disable')}
            >
              {record.disabled ? '手动启用' : '禁用'}
            </Button>
            <Tooltip title="清除冷却与熔断并立刻恢复参与选号（不改动禁用状态）">
              <Button
                size="small"
                type="link"
                icon={<ThunderboltOutlined />}
                loading={busy}
                disabled={!cooling}
                onClick={() => accountActionHandler(record, 'clear-cooldown')}
              >清冷却</Button>
            </Tooltip>
            <Button
              size="small"
              type="link"
              icon={<CheckCircleOutlined />}
              loading={busy}
              onClick={() => accountActionHandler(record, 'checkin')}
            >签到</Button>
            <Button
              size="small"
              type="link"
              icon={<ReloadOutlined />}
              loading={busy}
              onClick={() => accountActionHandler(record, 'keepalive')}
            >保活</Button>
            <Button
              size="small"
              type="link"
              danger
              icon={<DeleteOutlined />}
              loading={busy}
              onClick={() => accountActionHandler(record, 'delete')}
            >删除</Button>
          </Space>
        );
      },
    },
    { title: '可用积分', dataIndex: 'credits', sorter: (a, b) => (a.credits || 0) - (b.credits || 0), render: fmt },
    {
      title: '周期积分',
      render: (_, record) => record.cycle_capacity_size
        ? `${fmt(record.cycle_capacity_used)} / ${fmt(record.cycle_capacity_size)}`
        : '-',
    },
    {
      title: '累计积分',
      render: (_, record) => record.capacity_size
        ? `${fmt(record.capacity_used)} / ${fmt(record.capacity_size)}`
        : '-',
    },
    {
      title: '积分更新', dataIndex: 'credit_updated_at',
      sorter: (a, b) => (a.credit_updated_at || 0) - (b.credit_updated_at || 0),
      render: value => value ? new Date(value * 1000).toLocaleString() : '-',
    },
    {
      title: 'token',
      width: 130,
      // 凭证到期时间：后端透出 Unix 秒。剩余不足 10 分钟标红，便于提前重新授权。
      sorter: (a, b) => (a.token_expires_at || 0) - (b.token_expires_at || 0),
      render: (_, record) => {
        const label = tokenExpiryLabel(record.token_expires_at);
        if (!label) return <Text type="secondary">-</Text>;
        return <Text type={label.danger ? 'danger' : 'secondary'} title={fmtTime(record.token_expires_at * 1000)}>{label.text}</Text>;
      },
    },
    {
      title: '成功率',
      sorter: (a, b) => {
        const ratio = record => {
          const total = (record.success_count || 0) + (record.err_total || 0);
          return total ? record.success_count / total : -1;
        };
        return ratio(a) - ratio(b);
      },
      render: (_, record) => {
        const total = (record.success_count || 0) + (record.err_total || 0);
        return total ? `${Math.round((record.success_count / total) * 100)}%` : '-';
      },
    },
    {
      title: '在途', dataIndex: 'in_flight',
      sorter: (a, b) => (a.in_flight || 0) - (b.in_flight || 0),
    },
  ];

  const requestColumns = [
    { title: '时间', dataIndex: 'created_at', render: value => value ? new Date(value * 1000).toLocaleString() : '-' },
    { title: '端点', dataIndex: 'route', render: value => <Text code>{value || '-'}</Text> },
    { title: '模型', dataIndex: 'model', render: value => <Text strong>{value || '-'}</Text> },
    { title: '模式', render: (_, record) => <Tag color={record.passthrough ? 'purple' : 'blue'}>{record.passthrough ? '透传' : record.mode === 'stream' ? '流式' : record.mode === 'sync' ? '同步' : record.mode || '-'}</Tag> },
    { title: '状态', dataIndex: 'status', render: value => <Tag color={value >= 200 && value < 300 ? 'green' : 'red'}>{value || '-'}</Tag> },
    { title: '输入', dataIndex: 'input_tokens', render: fmt },
    { title: '输出', dataIndex: 'output_tokens', render: fmt },
    { title: '总量', dataIndex: 'total_tokens', render: fmt },
    { title: '请求上限', dataIndex: 'requested_output_tokens', render: value => value ? fmt(value) : '-' },
    {
      title: '积分消耗',
      render: (_, record) => (
        <Space direction="vertical" size={0}>
          <Text>{record.credit_source && record.credit_source !== 'unknown' ? fmtCredits(record.credits_consumed) : '-'}</Text>
          <Text type="secondary">{{ upstream: '上游', estimated: '估算', unknown: '未知' }[record.credit_source] || '未知'}</Text>
        </Space>
      ),
    },
    { title: '首 token', dataIndex: 'ttfb_millis', render: value => value ? `${value}ms` : '-' },
    { title: '耗时', dataIndex: 'latency_millis', render: value => value ? `${(value / 1000).toFixed(2)}s` : '-' },
    { title: '账号', dataIndex: 'account_uid', render: value => value ? <Text code>{value.slice(0, 12)}</Text> : '-' },
    { title: '版本', dataIndex: 'account_region', render: value => <Tag color={value === 'global' ? 'gold' : value === 'cn' ? 'blue' : 'default'}>{value === 'global' ? '海外版' : value === 'cn' ? '国内版' : '未知'}</Tag> },
    {
      title: '错误',
      render: (_, record) => record.error_code || record.error_message
        ? <Text type="danger" title={record.error_message || record.error_code}>{record.error_code || 'error'}</Text>
        : '-',
    },
  ];

  // filteredAccounts 支持按昵称/UID 搜索 + 状态/区域筛选，与表格排序组合使用。
  const filteredAccounts = useMemo(() => {
    const query = accountSearch.trim().toLowerCase();
    return (data.accounts || []).filter(record => {
      const portrait = coolingPortrait(record);
      const modelCooling = Object.keys(record.model_cooldowns || {}).length > 0;
      const matchesQuery = !query || [record.nickname, record.uid, record.reason]
        .some(value => String(value || '').toLowerCase().includes(query));
      const matchesRegion = accountRegionFilter === 'all' || (record.region || 'cn') === accountRegionFilter;
      let matchesStatus = true;
      if (accountStatusFilter === 'healthy') {
        matchesStatus = !record.disabled && !portrait.untilActive && !portrait.breakerActive && !modelCooling;
      } else if (accountStatusFilter === 'cooling') {
        matchesStatus = !record.disabled && (portrait.untilActive || portrait.breakerActive);
      } else if (accountStatusFilter === 'disabled') {
        matchesStatus = !!record.disabled;
      }
      return matchesQuery && matchesRegion && matchesStatus;
    });
  }, [data.accounts, accountSearch, accountStatusFilter, accountRegionFilter]);

  const filteredRequestLogs = useMemo(() => {
    const query = requestSearch.trim().toLowerCase();
    return requestLogs.filter(record => {
      const matchesQuery = !query || [record.model, record.route, record.account_uid, record.error_code, record.error_message]
        .some(value => String(value || '').toLowerCase().includes(query));
      const matchesMode = requestModeFilter === 'all' || (record.passthrough ? 'passthrough' : record.mode) === requestModeFilter;
      const success = Number(record.status) >= 200 && Number(record.status) < 300;
      const matchesStatus = requestStatusFilter === 'all' || (requestStatusFilter === 'success' ? success : !success);
      return matchesQuery && matchesMode && matchesStatus;
    });
  }, [requestLogs, requestSearch, requestModeFilter, requestStatusFilter]);

  const requestLogSummary = useMemo(() => ({
    total: filteredRequestLogs.length,
    failures: filteredRequestLogs.filter(record => Number(record.status) < 200 || Number(record.status) >= 300).length,
    tokens: filteredRequestLogs.reduce((sum, record) => sum + Number(record.total_tokens || 0), 0),
    credits: filteredRequestLogs.reduce((sum, record) => sum + Number(record.credits_consumed || 0), 0),
  }), [filteredRequestLogs]);

  const metrics = data.metrics || {};
  const cacheHitRate = Number(metrics.input_tokens || 0)
    ? (Number(metrics.cache_read_tokens || 0) / Number(metrics.input_tokens || 1)) * 100
    : 0;
  const statCards = [
    ['请求总数', metrics.requests, '#7aa2ff'], ['成功请求', metrics.successes, '#52c41a'],
    ['失败请求', metrics.failures, '#ff7875'], ['输入 token', metrics.input_tokens, '#69c0ff'],
    ['输出 token', metrics.output_tokens, '#b37feb'], ['总 token', metrics.total_tokens, '#9254de'],
    ['缓存读取', metrics.cache_read_tokens, '#36cfc9'], ['缓存创建', metrics.cache_write_tokens, '#13c2c2'],
    ['工具调用', metrics.tool_calls, '#ffc53d'], ['积分消耗', metrics.credits_consumed, '#fa8c16', fmtCredits],
  ];
  const activeTab = activeSection === 'dashboard' ? 'pool' : activeSection === 'settings' ? 'admin' : activeSection;
  const pageCopy = {
    dashboard: ['运营概览', '账号池、请求量和 token 用量实时汇总，数据每 30 秒自动更新。'],
    models: ['模型与请求', '查看当前可用的上游模型。'],
    playground: ['请求测试', '发送 Responses API 请求并查看标准化的 output_text。'],
    requests: ['请求日志', '查看每次请求的端点、模式、token、耗时、积分和错误详情。'],
    'auto-enroll': ['自动加号', '用豪猪接码平台批量添加中国区账号：取号 → 发码 → 收码 → 自动落盘，全程免浏览器。'],
    proxy: ['代理池', '短信直登链路的出口代理：池内容量、冷却状态和后续扩展。'],
    settings: ['管理设置', '配置签到计划和管理授权账号。'],
  };
  const [pageTitle, pageDescription] = pageCopy[activeSection] || pageCopy.dashboard;
  const poolRegionLabel = config.region === 'all' ? '混合区域' : config.region === 'global' ? '海外版' : '中国区';
  const pollRegion = loginPendingRegion || loginRegion;
  const menuItems = [
    { key: 'dashboard', icon: <DashboardOutlined />, label: '仪表盘' },
    { key: 'models', icon: <ApiOutlined />, label: '模型目录' },
    { key: 'playground', icon: <SendOutlined />, label: '请求测试' },
    { key: 'requests', icon: <FileSearchOutlined />, label: '请求日志' },
    {
      key: 'accounts-group', icon: <UserAddOutlined />, label: '账号运营',
      type: 'group',
      children: [
        { key: 'auto-enroll', icon: <ThunderboltOutlined />, label: '自动加号' },
        { key: 'proxy', icon: <CloudServerOutlined />, label: '代理池' },
      ],
    },
    { key: 'settings', icon: <SettingOutlined />, label: '管理设置' },
  ];
  const tabItems = [
    {
      key: 'pool', label: '账号池',
      children: (
        <Space direction="vertical" size={16} style={{ width: '100%' }}>
          <div style={{ display: 'grid', gridTemplateColumns: 'repeat(auto-fit,minmax(150px,1fr))', gap: 12 }}>
            <Card><Statistic title="账号总数" value={data.total || 0} /></Card>
            <Card><Statistic title="可用" value={data.healthy || 0} valueStyle={{ color: '#389e0d' }} /></Card>
            <Card><Statistic title="冷却中" value={data.cooling || 0} valueStyle={{ color: '#d46b08' }} /></Card>
            <Card><Statistic title="已禁用" value={data.disabled || 0} valueStyle={{ color: '#cf1322' }} /></Card>
            <Card><Statistic title="在途占满" value={data.in_flight_full || 0} /></Card>
            <Card><Statistic title="粘性会话" value={data.sticky_sessions || 0} /></Card>
          </div>
          <Card title="账号筛选">
            <Space wrap style={{ width: '100%' }}>
              <Input allowClear value={accountSearch} onChange={event => setAccountSearch(event.target.value)} placeholder="搜索昵称、UID 或冷却原因" style={{ minWidth: 260 }} />
              <Select value={accountStatusFilter} onChange={setAccountStatusFilter} style={{ width: 140 }} options={[
                { value: 'all', label: '全部状态' },
                { value: 'healthy', label: '可用' },
                { value: 'cooling', label: '冷却/熔断' },
                { value: 'disabled', label: '已禁用' },
              ]} />
              <Select value={accountRegionFilter} onChange={setAccountRegionFilter} style={{ width: 140 }} options={[
                { value: 'all', label: '全部区域' },
                { value: 'cn', label: '中国区' },
                { value: 'global', label: '海外版' },
              ]} />
            </Space>
          </Card>
          <Card
            title="账号状态"
            extra={(
              <Space>
                <Text type="secondary">显示 {filteredAccounts.length} / {data.total || 0} 个账号</Text>
                <Button size="small" icon={<ReloadOutlined />} loading={creditRefreshing} onClick={refreshCredits}>刷新上游积分</Button>
              </Space>
            )}
          >
            <Table
              rowKey="uid"
              columns={columns}
              dataSource={filteredAccounts}
              pagination={false}
              scroll={{ x: 1560 }}
              locale={{ emptyText: (data.total ? '暂无匹配的账号' : '账号池为空') }}
            />
          </Card>
        </Space>
      ),
    },
    {
      key: 'models', label: '模型目录',
      children: (
        <Card title="可用模型">
          {models.length
            ? <Space wrap>{models.map(model => <Tag key={model.id} color="blue">{model.id}</Tag>)}</Space>
            : <Empty image={Empty.PRESENTED_IMAGE_SIMPLE} description="暂无可用模型" />}
        </Card>
      ),
    },
    {
      key: 'playground', label: '请求测试',
      children: (
        <Card title="Responses API 测试">
          <Space direction="vertical" size={14} style={{ width: '100%' }}>
            <div className="request-toolbar">
              <Select style={{ minWidth: 280 }} value={selectedModel} onChange={setSelectedModel} options={models.map(model => ({ value: model.id, label: model.id }))} />
              <Space><Switch aria-label="流式响应" checked={stream} onChange={setStream} /><Text type="secondary">流式响应</Text></Space>
            </div>
            <Input.TextArea rows={5} maxLength={200000} showCount value={promptText} onChange={event => setPromptText(event.target.value)} placeholder="输入测试内容" />
            <Space>
              <Button type="primary" icon={<SendOutlined />} loading={loading} disabled={!selectedModel || !promptText.trim()} onClick={send}>发送请求</Button>
              {loading && <Button onClick={() => requestController.current?.abort()}>停止</Button>}
            </Space>
            {requestInfo && (
              <div className="request-meta" aria-live="polite">
                <span>状态 <strong>{requestInfo.state}</strong></span>
                <span>耗时 <strong>{(requestInfo.duration / 1000).toFixed(2)}s</strong></span>
                <span>首 token <strong>{requestInfo.ttft ? `${(requestInfo.ttft / 1000).toFixed(2)}s` : '-'}</strong></span>
                <span>输入 <strong>{fmt(requestInfo.usage?.input_tokens)}</strong></span>
                <span>输出 <strong>{fmt(requestInfo.usage?.output_tokens)}</strong></span>
                {requestInfo.responseID && <span title={requestInfo.responseID}>响应 ID <strong>{requestInfo.responseID}</strong></span>}
              </div>
            )}
            <Card size="small" title="output_text"><pre className="response-output" aria-live="polite">{answer || '等待响应…'}</pre></Card>
          </Space>
        </Card>
      ),
    },
    {
      key: 'requests', label: '请求日志',
      children: (
        <Space direction="vertical" size={16} style={{ width: '100%' }}>
          <div style={{ display: 'grid', gridTemplateColumns: 'repeat(auto-fit,minmax(180px,1fr))', gap: 12 }}>
            <Card><Statistic title="当前记录" value={requestLogSummary.total} /></Card>
            <Card><Statistic title="失败记录" value={requestLogSummary.failures} valueStyle={{ color: '#cf1322' }} /></Card>
            <Card><Statistic title="记录 token" value={fmt(requestLogSummary.tokens)} /></Card>
            <Card><Statistic title="记录积分" value={fmtCredits(requestLogSummary.credits)} valueStyle={{ color: '#d46b08' }} /></Card>
          </div>
          <Card title="请求筛选">
            <Space wrap style={{ width: '100%' }}>
              <Input allowClear value={requestSearch} onChange={event => setRequestSearch(event.target.value)} placeholder="搜索模型、端点、账号或错误" style={{ minWidth: 280 }} />
              <Select value={requestModeFilter} onChange={setRequestModeFilter} style={{ width: 150 }} options={[{ value: 'all', label: '全部模式' }, { value: 'stream', label: '流式' }, { value: 'sync', label: '同步' }, { value: 'passthrough', label: '透传' }]} />
              <Select value={requestStatusFilter} onChange={setRequestStatusFilter} style={{ width: 150 }} options={[{ value: 'all', label: '全部状态' }, { value: 'success', label: '成功' }, { value: 'failed', label: '失败' }]} />
              <Button icon={<ReloadOutlined />} onClick={refresh}>刷新日志</Button>
            </Space>
          </Card>
          <Card title="请求明细" extra={<Text type="secondary">显示 {filteredRequestLogs.length} / {requestLogs.length} 条</Text>}>
            <Table
              rowKey="id"
              columns={requestColumns}
              dataSource={filteredRequestLogs}
              pagination={{ pageSize: 20, showSizeChanger: true, pageSizeOptions: [10, 20, 50] }}
              scroll={{ x: 1680 }}
              locale={{ emptyText: '暂无匹配的请求记录' }}
              expandable={{
                expandedRowRender: record => (
                  <Space direction="vertical" size={4}>
                    <Text type="secondary">请求 ID：{record.id || '-'}</Text>
                    <Text type="secondary">请求上限：{record.requested_output_tokens ? fmt(record.requested_output_tokens) : '未设置'}；缓存读取：{fmt(record.cache_read_tokens)}；缓存创建：{fmt(record.cache_write_tokens)}；工具调用：{fmt(record.tool_calls)}</Text>
                    {(record.error_code || record.error_message) && <Text type="danger">{record.error_code || 'error'}：{record.error_message || '无错误详情'}</Text>}
                  </Space>
                ),
              }}
            />
          </Card>
        </Space>
      ),
    },
    {
      key: 'admin', label: '管理设置',
      children: (
        <Space direction="vertical" style={{ width: '100%' }}>
          <Card title="签到与保活">
            <Form form={form} layout="inline" onFinish={saveConfig}>
              <Form.Item name="checkin" label="签到小时"><Input placeholder="9,21" /></Form.Item>
              <Form.Item name="keepalive" label="保活小时"><Input placeholder="22" /></Form.Item>
              <Form.Item name="retention" label="日志保留天数"><Input placeholder="30" /></Form.Item>
              <Button type="primary" htmlType="submit">保存</Button>
            </Form>
            <Paragraph type="secondary" style={{ margin: '12px 0 0' }}>
              定时任务按上面的整点触发；需要立刻执行时可点右侧按钮（对全部启用中的账号生效）。
            </Paragraph>
            <Space wrap style={{ marginTop: 8 }}>
              <Button icon={<CheckCircleOutlined />} loading={checkinRunning} onClick={runCheckinAll}>立即签到（全部账号）</Button>
              <Button icon={<ReloadOutlined />} loading={creditRefreshing} onClick={refreshCredits}>立即刷新积分</Button>
            </Space>
          </Card>
          <Card title="短信直登与自动加号配置" extra={<Tag color={smsSettings.haozhuma.sid ? 'green' : 'default'}>{smsSettings.haozhuma.sid ? '已配置项目' : '未启用自动加号'}</Tag>}>
            <Alert type="info" showIcon message="敏感字段不会回显；保存后账号、自动加号和代理立即生效，不需要重启。代理池可直接粘贴导入。" style={{ marginBottom: 14 }} />
            <Space direction="vertical" style={{ width: '100%' }} size={10}>
              <Text strong>豪猪接码</Text>
              <Space wrap>
                <Input value={smsSettings.haozhuma.user} onChange={e => setSmsSettings(s => ({ ...s, haozhuma: { ...s.haozhuma, user: e.target.value } }))} placeholder="豪猪账号" />
                <Input.Password value={smsSettings.haozhuma.pass} onChange={e => setSmsSettings(s => ({ ...s, haozhuma: { ...s.haozhuma, pass: e.target.value } }))} placeholder="密码（留空保持）" />
                <Input.Password value={smsSettings.haozhuma.token} onChange={e => setSmsSettings(s => ({ ...s, haozhuma: { ...s.haozhuma, token: e.target.value } }))} placeholder="Token（留空保持）" />
                <Input value={smsSettings.haozhuma.sid} onChange={e => setSmsSettings(s => ({ ...s, haozhuma: { ...s.haozhuma, sid: e.target.value } }))} placeholder="项目 SID，如 52283" />
                <Input value={smsSettings.haozhuma.author} onChange={e => setSmsSettings(s => ({ ...s, haozhuma: { ...s.haozhuma, author: e.target.value } }))} placeholder="Author（可选）" />
                <Input value={smsSettings.haozhuma.uid} onChange={e => setSmsSettings(s => ({ ...s, haozhuma: { ...s.haozhuma, uid: e.target.value } }))} placeholder="对接码 UID（可选）" />
                <Input value={smsSettings.haozhuma.isp} onChange={e => setSmsSettings(s => ({ ...s, haozhuma: { ...s.haozhuma, isp: e.target.value } }))} placeholder="运营商优先级，如 1,2,3" />
              </Space>
              <Text strong>短信代理与人机校验</Text>
              <Space wrap>
                <Input value={smsSettings.twoCaptchaKey} onChange={e => setSmsSettings(s => ({ ...s, twoCaptchaKey: e.target.value }))} placeholder="2Captcha Key（留空保持）" style={{ width: 260 }} />
                <Input value={smsSettings.proxy.url} onChange={e => setSmsSettings(s => ({ ...s, proxy: { ...s.proxy, url: e.target.value } }))} placeholder="代理 URL（可选）" style={{ width: 300 }} />
                <Input.TextArea value={smsSettings.proxy.lines || ''} onChange={e => setSmsSettings(s => ({ ...s, proxy: { ...s.proxy, lines: e.target.value } }))} placeholder={'直接粘贴代理池，每行 host:port:user:pass 或 http://user:pass@host:port'} autoSize={{ minRows: 2, maxRows: 5 }} style={{ width: 520 }} />
                <Input value={smsSettings.proxy.cooldown} onChange={e => setSmsSettings(s => ({ ...s, proxy: { ...s.proxy, cooldown: e.target.value } }))} placeholder="冷却，如 30m" style={{ width: 150 }} />
                <Input value={smsSettings.proxy.region} onChange={e => setSmsSettings(s => ({ ...s, proxy: { ...s.proxy, region: e.target.value } }))} placeholder="代理区域，如 HK" style={{ width: 150 }} />
                <Input value={smsSettings.proxy.sticky_minutes} onChange={e => setSmsSettings(s => ({ ...s, proxy: { ...s.proxy, sticky_minutes: Number(e.target.value || 0) } }))} placeholder="粘性分钟" style={{ width: 120 }} />
                <Space><Text>注入 SID</Text><Switch checked={!!smsSettings.proxy.inject_sid} onChange={value => setSmsSettings(s => ({ ...s, proxy: { ...s.proxy, inject_sid: value } }))} /></Space>
              </Space>
              <Space wrap>
                <Button type="primary" onClick={() => form.submit()}>保存自动加号配置</Button>
                <Button icon={<CloudServerOutlined />} onClick={checkUpdate}>检查 GitHub 版本</Button>
                {updateInfo && <Text type={updateInfo.update_available ? 'warning' : 'success'}>当前 {updateInfo.current || 'dev'}，GitHub 最新 {updateInfo.latest.slice(0, 7)}{updateInfo.update_available ? '，可更新' : '，已是最新'}</Text>}
              </Space>
            </Space>
          </Card>
          <Card title="账号授权" extra={<Tag color={config.region === 'all' ? 'green' : 'blue'}>账号池：{poolRegionLabel}</Tag>}>
            <Space wrap>
              <Text type="secondary">登录区域</Text>
              <Select
                value={loginRegion}
                onChange={value => { loginRegionTouched.current = true; setLoginRegion(value); }}
                style={{ width: 150 }}
                options={[{ value: 'cn', label: '中国区' }, { value: 'global', label: '海外版' }]}
              />
              <Button icon={<UnlockOutlined />} onClick={async () => {
                try {
                  const query = `?region=${encodeURIComponent(loginRegion)}`;
                  const result = await api(`/admin/account/url${query}`, { method: 'POST' });
                  setLoginURL(result.url || '');
                  setLoginPendingRegion(result.region || loginRegion);
                  const popup = window.open(result.url, '_blank', 'noopener');
                  message.info(popup
                    ? `${loginRegion === 'global' ? '海外版' : '中国区'}授权链接已打开，正在自动等待授权完成…`
                    : '浏览器拦截了弹窗，请点击下方授权链接完成登录，系统会自动轮询');
                  // 打开链接后自动轮询，避免用户忘记点"轮询授权结果"导致账号没入库。
                  startLoginPoll(result.region || loginRegion);
                } catch (error) { message.error(error.message); }
              }}>生成 OAuth 登录链接</Button>
              <Button icon={<ToolOutlined />} loading={loginPolling} onClick={async () => {
                try {
                  const query = `?region=${encodeURIComponent(pollRegion)}`;
                  const result = await api(`/admin/account/poll${query}`, { method: 'POST' });
                  if (result.warning) message.warning(result.warning);
                  else message.success(`${result.region === 'global' ? '海外版' : '中国区'}账号已添加`);
                  setLoginURL('');
                  setLoginPendingRegion('');
                  await refresh();
                } catch (error) { message.error(error.message); }
              }}>轮询授权结果</Button>
            </Space>
            {loginURL && (
              <div style={{ marginTop: 12, wordBreak: 'break-all' }}>
                <Text type="secondary">{pollRegion === 'global' ? '海外版' : '中国区'}授权链接：</Text>{' '}
                <a href={loginURL} target="_blank" rel="noreferrer">点击打开浏览器登录</a>
              </div>
            )}
            <Paragraph type="secondary" style={{ margin: '12px 0 0' }}>
              中国区与海外版账号可以同时使用；登录后系统会按账号区域自动选择对应的模型、聊天和积分接口。
            </Paragraph>

            <Card
              type="inner"
              title="短信直登（中国区）"
              style={{ marginTop: 16 }}
              extra={<Tag color="orange">免开浏览器</Tag>}
            >
              <Paragraph type="secondary" style={{ marginTop: 0 }}>
                填写手机号后点「发送验证码」，收到短信填入验证码即可直接添加账号，
                无需打开浏览器点授权。仅支持中国区；海外版请用上面的 OAuth 链接。
                若号码被人机校验拦下，已配置打码平台密钥时会自动过码，否则请改用浏览器授权。
              </Paragraph>
              <Space wrap>
                <Input
                  value={smsMobile}
                  onChange={event => setSmsMobile(event.target.value)}
                  placeholder="手机号，如 +8613800138000 或 +852 64087495"
                  style={{ width: 300 }}
                  prefix={<KeyOutlined />}
                  allowClear
                  disabled={!!smsSession}
                />
                <Button
                  icon={<SendOutlined />}
                  loading={smsSending}
                  disabled={smsCountdown > 0}
                  onClick={() => runSMSSend({ resend: !!smsSession })}
                >
                  {smsCountdown > 0
                    ? `${smsCountdown} 秒后可重发`
                    : (smsSession ? '重新发送验证码' : '发送验证码')}
                </Button>
                {smsSession && (
                  <Button
                    type="link"
                    onClick={() => { setSmsSession(''); setSmsCode(''); setSmsCaptcha(null); setSmsCountdown(0); }}
                  >
                    换手机号
                  </Button>
                )}
              </Space>
              {smsCaptcha && (
                <Alert
                  style={{ marginTop: 12 }}
                  type="warning"
                  showIcon
                  message="该号码需要人机校验"
                  description={(
                    <Space direction="vertical" size={8}>
                      <Text type="secondary">
                        {smsCaptcha.reason || '请完成验证码后继续，通过后会自动发送短信。'}
                      </Text>
                      {/* 上游会给出多种校验方案（teg / tencent）。已知 tencent 可用，
                          因此把它排在前面，避免用户先点到一个注定失败的按钮；
                          teg 仍保留作为后备。 */}
                      <Space wrap>
                        {orderCaptchaOptions(smsCaptcha.options).map(option => (
                          <Button
                            key={`${option.cloudType}-${option.appId}`}
                            type="primary"
                            size="small"
                            icon={<SafetyOutlined />}
                            loading={smsCaptchaBusy}
                            onClick={() => submitCaptcha(option)}
                          >
                            {option.cloudType === 'tencent' ? '开始验证（腾讯）' : `开始验证（${option.cloudType}）`}
                          </Button>
                        ))}
                        {!(smsCaptcha.options || []).length && (
                          <Button
                            type="primary" size="small" icon={<SafetyOutlined />}
                            loading={smsCaptchaBusy}
                            onClick={() => submitCaptcha({ appId: '', cloudType: '' })}
                          >
                            重新获取验证码
                          </Button>
                        )}
                      </Space>
                    </Space>
                  )}
                />
              )}
              <Space wrap style={{ marginTop: 12 }}>
                <Input
                  value={smsCode}
                  onChange={event => setSmsCode(event.target.value)}
                  onPressEnter={runSMSVerify}
                  placeholder="6 位短信验证码"
                  style={{ width: 200 }}
                  disabled={!smsSession || !!smsCaptcha}
                  allowClear
                />
                <Button
                  type="primary"
                  icon={<CheckCircleOutlined />}
                  loading={smsVerifying}
                  disabled={!smsSession || !!smsCaptcha}
                  onClick={runSMSVerify}
                >
                  验证并添加账号
                </Button>
                {smsCaptcha
                  ? <Text type="secondary">请先完成上方的人机校验</Text>
                  : smsSession
                    ? <Text type="secondary">验证码已发送，请查收；会话 10 分钟内有效</Text>
                    : <Text type="secondary">填错验证码不会作废会话，可直接重填</Text>}
              </Space>
            </Card>
          </Card>
        </Space>
      ),
    },
  ];

  return (
    <Layout style={{ minHeight: '100vh' }}>
        <Sider breakpoint="lg" collapsedWidth="0">
          <div style={{ color: '#172033', fontSize: 18, fontWeight: 700, padding: '22px 20px' }}>WorkBuddy<span style={{ color: '#356ae6' }}>2API</span></div>
          <Menu theme="light" mode="inline" selectedKeys={[activeSection]} items={menuItems} onClick={({ key }) => setActiveSection(key)} />
        </Sider>
        <Layout>
          <Header style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between', padding: '0 26px' }}>
            <Space>
              <Text strong style={{ color: '#172033' }}>服务控制台</Text>
              {/* healthy 是"可用账号数"，不是布尔量：池非空但全部冷却时同样不可服务，
                  因此这里用「可用数 + 在途占满」共同判定，不能直接拿它当 true/false。 */}
              {(() => {
                const total = Number(data.total || 0);
                const healthy = Number(data.healthy || 0);
                const full = Number(data.in_flight_full || 0);
                const servable = healthy > full;
                const label = !total ? '账号池为空'
                  : servable ? `服务可用（${healthy} 个账号）`
                    : healthy > 0 ? '账号在途占满' : '账号池检查中';
                return <Tag color={servable ? 'green' : 'orange'}>{label}</Tag>;
              })()}
              <Tag>版本 {data.version || 'dev'}</Tag>
            </Space>
            <Space>
              <Button icon={<ReloadOutlined />} onClick={refresh}>刷新</Button>
              <Button icon={<ReloadOutlined />} loading={updateStarting} onClick={updateService}>Docker 更新</Button>
              <Button icon={<KeyOutlined />} onClick={() => {
                setApiKeyDraft(apiKey);
                setApiKeyOpen(true);
              }}>API Key</Button>
            </Space>
          </Header>
          <Content style={{ padding: 26 }}>
            <Title level={2} style={{ marginTop: 0 }}>{pageTitle}</Title>
            <Paragraph type="secondary">{pageDescription}</Paragraph>
            {activeSection === 'dashboard' && (
              <>
                <div style={{ display: 'grid', gridTemplateColumns: 'repeat(auto-fit,minmax(170px,1fr))', gap: 16, marginBottom: 20 }}>
                  {statCards.map(([label, value, color, formatter]) => <Card key={label}><Statistic title={label} value={formatter ? formatter(value) : fmt(value)} valueStyle={{ color }} /></Card>)}
                </div>
                <Card className="trend-card" title="性能趋势" extra={<Text type="secondary">最近 {metricSamples.length} 次刷新</Text>}>
                  <div className="trend-grid">
                    <div className="trend-layout">
                      <div>
                        <Text type="secondary">缓存命中率</Text>
                        <div className="trend-value">{cacheHitRate.toFixed(1)}%</div>
                        <Text type="secondary">缓存读取 token / 输入 token</Text>
                      </div>
                      <Sparkline values={metricSamples.map(sample => sample.cacheRate)} ariaLabel="缓存命中率趋势" />
                    </div>
                    <div className="trend-layout">
                      <div>
                        <Text type="secondary">平均首 token</Text>
                        <div className="trend-value">{Number(metrics.avg_ttfb_ms || 0) ? `${(Number(metrics.avg_ttfb_ms) / 1000).toFixed(2)}s` : '-'}</div>
                        <Text type="secondary">仅统计流式请求</Text>
                      </div>
                      <Sparkline values={metricSamples.map(sample => sample.avgTTFB)} color="#12a594" ariaLabel="平均首 token 趋势" />
                    </div>
                  </div>
                </Card>
                <Card title="最近请求" extra={<Text type="secondary">{requestLogs.length} 条</Text>} style={{ marginTop: 20 }}>
                  <Table rowKey="id" columns={requestColumns} dataSource={requestLogs} pagination={{ pageSize: 10 }} scroll={{ x: 1480 }} locale={{ emptyText: '暂无请求记录' }} />
                </Card>
              </>
            )}
            {activeSection === 'auto-enroll' && <AutoEnrollPage api={api} />}
            {activeSection === 'proxy' && <ProxyPoolPage api={api} />}
            {tabItems.find(item => item.key === activeTab)?.children}
          </Content>
        </Layout>
        <Modal
          open={apiKeyOpen}
          title={<Space><KeyOutlined />API 密钥</Space>}
          onCancel={() => setApiKeyOpen(false)}
          footer={null}
          destroyOnClose
        >
          <Card size="small" className="api-key-card">
            <Space direction="vertical" size={12} style={{ width: '100%' }}>
              <div>
                <Text strong>控制台访问密钥</Text>
                <br />
                <Text type="secondary">密钥只保存在当前浏览器会话中，用于访问管理接口。</Text>
              </div>
              <Input.Password
                value={apiKeyDraft}
                onChange={event => setApiKeyDraft(event.target.value)}
                onPressEnter={saveAPIKey}
                placeholder="输入 API Key"
                autoFocus
              />
              <Space wrap>
                <Button type="primary" icon={<KeyOutlined />} onClick={saveAPIKey}>保存密钥</Button>
                <Button danger icon={<ReloadOutlined />} loading={apiKeyResetting} onClick={resetAPIKey}>重置并生成新密钥</Button>
              </Space>
              <Alert type="info" showIcon message="重置会在服务器端生成新 API Key 并立即替换旧密钥，当前浏览器会自动保存新密钥。" />
            </Space>
          </Card>
        </Modal>
        <Modal open={locked} title="控制台验证" onOk={unlock} onCancel={() => {}} okText="解锁" cancelButtonProps={{ style: { display: 'none' } }}>
          <Alert message="请输入前端访问密码，或填写 API Key" type="info" showIcon style={{ marginBottom: 14 }} />
          <Input.Password prefix={<KeyOutlined />} value={password} onChange={event => setPassword(event.target.value)} onPressEnter={unlock} placeholder="输入访问密码" />
          <Input prefix={<ApiOutlined />} value={apiKey} onChange={event => setApiKey(event.target.value)} onPressEnter={unlock} placeholder="输入 API Key" style={{ marginTop: 10 }} />
        </Modal>
    </Layout>
  );
}

// Root：ConfigProvider 内套 antd 的 App，让 Console 里的 useApp() 拿到
// 真正可用的 message / modal 实例（React 19 下静态方法不可用），同时继承主题。
function Root() {
  return (
    <ConfigProvider theme={{ token: { colorPrimary: '#356ae6', borderRadius: 8, colorBgContainer: '#ffffff', colorBgLayout: '#f5f7fb', colorText: '#172033', colorTextSecondary: '#667085', colorTextHeading: '#172033', colorBorder: '#e7ebf1' } }}>
      <AntApp>
        <Console />
      </AntApp>
    </ConfigProvider>
  );
}

createRoot(document.getElementById('root')).render(<Root />);
