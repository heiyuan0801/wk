import React, { useEffect, useMemo, useRef, useState } from 'react';
import { createRoot } from 'react-dom/client';
import {
  Alert, Button, Card, ConfigProvider, Empty, Form, Input, Layout, Menu, Modal,
  Select, Space, Statistic, Switch, Table, Tag, Typography, message,
} from 'antd';
import {
  ApiOutlined, DashboardOutlined, KeyOutlined, ReloadOutlined, SendOutlined,
  SettingOutlined, ToolOutlined, UnlockOutlined,
} from '@ant-design/icons';
import 'antd/dist/reset.css';
import './theme.css';

const { Header, Sider, Content } = Layout;
const { Title, Text, Paragraph } = Typography;
const fmt = value => Number(value || 0).toLocaleString();
const parseHours = value => String(value || '').split(',').map(item => item.trim()).filter(Boolean).map(Number);
const initialAPIKey = sessionStorage.getItem('wb2api-api-key') || localStorage.getItem('wb2api-api-key') || '';
localStorage.removeItem('wb2api-api-key');

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

function App() {
  const [form] = Form.useForm();
  const [apiKey, setApiKey] = useState(initialAPIKey);
  const [locked, setLocked] = useState(false);
  const [password, setPassword] = useState('');
  const [activeSection, setActiveSection] = useState('dashboard');
  const [data, setData] = useState({ accounts: [], metrics: {}, total: 0, healthy: 0, cooling: 0, disabled: 0 });
  const [models, setModels] = useState([]);
  const [config, setConfig] = useState({ checkin_hours: [9, 21], keepalive_hours: [22] });
  const [selectedModel, setSelectedModel] = useState('');
  const [promptText, setPromptText] = useState('你好，请简短介绍一下你自己。');
  const [answer, setAnswer] = useState('');
  const [loading, setLoading] = useState(false);
  const [stream, setStream] = useState(true);
  const [requestInfo, setRequestInfo] = useState(null);
  const [metricSamples, setMetricSamples] = useState([]);
  const refreshController = useRef(null);
  const refreshSerial = useRef(0);
  const requestController = useRef(null);
  const requestSerial = useRef(0);

  const headers = useMemo(() => (apiKey ? { Authorization: `Bearer ${apiKey}` } : {}), [apiKey]);
  const api = async (path, options = {}) => {
    const response = await fetch(path, { ...options, headers: { ...headers, ...(options.headers || {}) } });
    const body = await response.json().catch(() => ({}));
    if (!response.ok) throw Error(body?.error?.message || body?.error || `${response.status}`);
    return body;
  };

  const refresh = async () => {
    const serial = ++refreshSerial.current;
    refreshController.current?.abort();
    const controller = new AbortController();
    refreshController.current = controller;
    try {
      const [status, modelList] = await Promise.all([
        api('/status', { signal: controller.signal }),
        api('/v1/models', { signal: controller.signal }),
      ]);
      if (serial !== refreshSerial.current) return;
      setData(status);
      setModels(modelList.data || []);
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
      if (currentConfig.schedule) setConfig(currentConfig.schedule);
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
    });
  }, [config, form]);

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

  const saveConfig = async values => {
    try {
      const nextConfig = {
        checkin_hours: parseHours(values.checkin),
        keepalive_hours: parseHours(values.keepalive),
      };
      const invalid = Object.values(nextConfig).some(hours => (
        !hours.length || hours.some(hour => !Number.isInteger(hour) || hour < 0 || hour > 23)
      ));
      if (invalid) {
        message.error('每项至少填写一个 0-23 的整数小时');
        return;
      }
      const result = await api('/admin/config', {
        method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(nextConfig),
      });
      setConfig(result.schedule || nextConfig);
      message.success(result.restart_required ? '配置已保存，重启后生效' : '配置已保存');
    } catch (error) {
      message.error(error.message);
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
      title: '状态',
      render: (_, record) => (
        <Tag color={record.disabled ? 'red' : record.cooling ? 'orange' : 'green'}>
          {record.disabled ? '禁用' : record.cooling ? '冷却' : '可用'}
        </Tag>
      ),
    },
    { title: '积分', dataIndex: 'credits', render: fmt },
    {
      title: '成功率',
      render: (_, record) => {
        const total = (record.success_count || 0) + (record.err_total || 0);
        return total ? `${Math.round((record.success_count / total) * 100)}%` : '-';
      },
    },
    { title: '在途', dataIndex: 'in_flight' },
  ];

  const metrics = data.metrics || {};
  const cacheHitRate = Number(metrics.input_tokens || 0)
    ? (Number(metrics.cache_read_tokens || 0) / Number(metrics.input_tokens || 1)) * 100
    : 0;
  const statCards = [
    ['请求总数', metrics.requests, '#7aa2ff'], ['成功请求', metrics.successes, '#52c41a'],
    ['失败请求', metrics.failures, '#ff7875'], ['输入 token', metrics.input_tokens, '#69c0ff'],
    ['输出 token', metrics.output_tokens, '#b37feb'], ['总 token', metrics.total_tokens, '#9254de'],
    ['缓存读取', metrics.cache_read_tokens, '#36cfc9'], ['缓存创建', metrics.cache_write_tokens, '#13c2c2'],
    ['工具调用', metrics.tool_calls, '#ffc53d'],
  ];
  const activeTab = activeSection === 'dashboard' ? 'pool' : activeSection === 'settings' ? 'admin' : activeSection;
  const pageCopy = {
    dashboard: ['运营概览', '账号池、请求量和 token 用量实时汇总，数据每 30 秒自动更新。'],
    models: ['模型与请求', '查看当前可用的上游模型。'],
    playground: ['请求测试', '发送 Responses API 请求并查看标准化的 output_text。'],
    settings: ['管理设置', '配置签到计划和管理授权账号。'],
  };
  const [pageTitle, pageDescription] = pageCopy[activeSection] || pageCopy.dashboard;
  const menuItems = [
    { key: 'dashboard', icon: <DashboardOutlined />, label: '仪表盘' },
    { key: 'models', icon: <ApiOutlined />, label: '模型目录' },
    { key: 'playground', icon: <SendOutlined />, label: '请求测试' },
    { key: 'settings', icon: <SettingOutlined />, label: '管理设置' },
  ];
  const tabItems = [
    {
      key: 'pool', label: '账号池',
      children: (
        <Card title="账号状态" extra={<Text type="secondary">{data.total || 0} 个账号</Text>}>
          <Table rowKey="uid" columns={columns} dataSource={data.accounts || []} pagination={false} scroll={{ x: 760 }} />
        </Card>
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
      key: 'admin', label: '管理设置',
      children: (
        <Space direction="vertical" style={{ width: '100%' }}>
          <Card title="签到与保活">
            <Form form={form} layout="inline" onFinish={saveConfig}>
              <Form.Item name="checkin" label="签到小时"><Input placeholder="9,21" /></Form.Item>
              <Form.Item name="keepalive" label="保活小时"><Input placeholder="22" /></Form.Item>
              <Button type="primary" htmlType="submit">保存</Button>
            </Form>
          </Card>
          <Card title="账号授权">
            <Space wrap>
              <Button icon={<UnlockOutlined />} onClick={async () => {
                try {
                  const result = await api('/admin/account/url', { method: 'POST' });
                  window.open(result.url, '_blank', 'noopener');
                  message.info('完成浏览器授权后回到此页面再次点击轮询');
                } catch (error) { message.error(error.message); }
              }}>生成 OAuth 登录链接</Button>
              <Button icon={<ToolOutlined />} onClick={async () => {
                try {
                  await api('/admin/account/poll', { method: 'POST' });
                  message.success('账号已添加');
                  await refresh();
                } catch (error) { message.error(error.message); }
              }}>轮询授权结果</Button>
            </Space>
          </Card>
        </Space>
      ),
    },
  ];

  return (
    <ConfigProvider theme={{ token: { colorPrimary: '#356ae6', borderRadius: 8, colorBgContainer: '#ffffff', colorBgLayout: '#f5f7fb', colorText: '#172033', colorTextSecondary: '#667085', colorTextHeading: '#172033', colorBorder: '#e7ebf1' } }}>
      <Layout style={{ minHeight: '100vh' }}>
        <Sider breakpoint="lg" collapsedWidth="0">
          <div style={{ color: '#172033', fontSize: 18, fontWeight: 700, padding: '22px 20px' }}>WorkBuddy<span style={{ color: '#356ae6' }}>2API</span></div>
          <Menu theme="light" mode="inline" selectedKeys={[activeSection]} items={menuItems} onClick={({ key }) => setActiveSection(key)} />
        </Sider>
        <Layout>
          <Header style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between', padding: '0 26px' }}>
            <Space><Text strong style={{ color: '#172033' }}>服务控制台</Text><Tag color={data.healthy ? 'green' : 'orange'}>{data.healthy ? '服务可用' : '账号池检查中'}</Tag></Space>
            <Space>
              <Button icon={<ReloadOutlined />} onClick={refresh}>刷新</Button>
              <Button icon={<KeyOutlined />} onClick={() => {
                const key = window.prompt('API Key', apiKey);
                if (key !== null) {
                  setApiKey(key);
                  sessionStorage.setItem('wb2api-api-key', key);
                }
              }}>API Key</Button>
            </Space>
          </Header>
          <Content style={{ padding: 26 }}>
            <Title level={2} style={{ marginTop: 0 }}>{pageTitle}</Title>
            <Paragraph type="secondary">{pageDescription}</Paragraph>
            {activeSection === 'dashboard' && (
              <>
                <div style={{ display: 'grid', gridTemplateColumns: 'repeat(auto-fit,minmax(170px,1fr))', gap: 16, marginBottom: 20 }}>
                  {statCards.map(([label, value, color]) => <Card key={label}><Statistic title={label} value={fmt(value)} valueStyle={{ color }} /></Card>)}
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
              </>
            )}
            {tabItems.find(item => item.key === activeTab)?.children}
          </Content>
        </Layout>
        <Modal open={locked} title="控制台验证" onOk={unlock} onCancel={() => {}} okText="解锁" cancelButtonProps={{ style: { display: 'none' } }}>
          <Alert message="请输入前端访问密码，或填写 API Key" type="info" showIcon style={{ marginBottom: 14 }} />
          <Input.Password prefix={<KeyOutlined />} value={password} onChange={event => setPassword(event.target.value)} onPressEnter={unlock} placeholder="输入访问密码" />
          <Input prefix={<ApiOutlined />} value={apiKey} onChange={event => setApiKey(event.target.value)} onPressEnter={unlock} placeholder="输入 API Key" style={{ marginTop: 10 }} />
        </Modal>
      </Layout>
    </ConfigProvider>
  );
}

createRoot(document.getElementById('root')).render(<App />);
