import React, { useEffect, useRef, useState } from 'react';
import {
  Alert, Button, Card, Empty, InputNumber, Progress, Space, Statistic, Tag, Tooltip,
  Typography, message,
} from 'antd';
import {
  PauseCircleOutlined, PlayCircleOutlined, ReloadOutlined,
} from '@ant-design/icons';

const { Text, Paragraph } = Typography;

// POLL_MS 轮询间隔。任务在后端跑，前端只做展示；2 秒足够跟手，
// 又不至于把带 200 行日志的接口打得太密。
const POLL_MS = 2000;
// MAX_WORKERS 与后端 maxWorkers 一致（超出会被后端夹住，这里先拦一层）。
const MAX_WORKERS = 8;

// logTone 给日志行着色：扫一眼就能看出这轮顺不顺。
function logTone(text) {
  if (text.includes('加号成功')) return '#389e0d';
  if (text.includes('失败') || text.includes('终止')) return '#cf1322';
  if (text.includes('取号')) return '#667085';
  return undefined;
}

/**
 * AutoEnroll 自动加号页。
 *
 * 数据来源：
 *   POST /admin/account/sms/auto-enroll       启动（{count, workers}）
 *   GET  /admin/account/sms/auto-enroll       进度（logs / balance / stop_reason）
 *   POST /admin/account/sms/auto-enroll/stop  停止
 *
 * 计费前提必须在界面上说清楚（见下方 Alert）：豪猪只在**收码成功**时扣费，
 * 取号后收不到码不扣费。否则用户会以为失败也在烧钱。
 */
export default function AutoEnroll({ api }) {
  const [count, setCount] = useState(5);
  const [workers, setWorkers] = useState(3);
  const [status, setStatus] = useState(null);
  const [starting, setStarting] = useState(false);
  const [stopping, setStopping] = useState(false);
  const [error, setError] = useState('');
  const [loadingStatus, setLoadingStatus] = useState(false);
  const logBoxRef = useRef(null);
  // 轮询句柄。running 时按 POLL_MS 续期；停下就不再排期。
  const timerRef = useRef(null);

  const load = async () => {
    setLoadingStatus(true);
    try {
      const body = await api('/admin/account/sms/auto-enroll');
      setStatus(body);
      setError('');
      return body;
    } catch (err) {
      setError(err.message);
      return null;
    } finally {
      setLoadingStatus(false);
    }
  };

  // 首屏载入 + 自续期轮询：只要后端还在跑就继续轮询，停了自然结束。
  // 这样不需要用户手动刷新，也不会在空闲时无限打接口。
  useEffect(() => {
    let alive = true;
    const tick = async () => {
      const body = await load();
      if (!alive) return;
      if (body?.running) {
        timerRef.current = setTimeout(tick, POLL_MS);
      } else {
        timerRef.current = null;
      }
    };
    tick();
    return () => {
      alive = false;
      if (timerRef.current) clearTimeout(timerRef.current);
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  // 日志自动滚到底：任务跑起来后最关心的是最新几行。
  useEffect(() => {
    const box = logBoxRef.current;
    if (box) box.scrollTop = box.scrollHeight;
  }, [status?.logs]);

  const start = async () => {
    setStarting(true);
    setError('');
    try {
      await api('/admin/account/sms/auto-enroll', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ count, workers }),
      });
      message.success(`已启动：目标 ${count} 个，并发 ${workers}`);
      const body = await load();
      // 上一轮轮询可能已结束（timerRef 为 null），重新排期继续跟进度。
      if (body?.running && !timerRef.current) {
        const tick = async () => {
          const next = await load();
          if (next?.running) timerRef.current = setTimeout(tick, POLL_MS);
          else timerRef.current = null;
        };
        timerRef.current = setTimeout(tick, POLL_MS);
      }
    } catch (err) {
      // 余额不足 / 已在运行 都走这里；后端已给出可读文案。
      setError(err.message);
      message.error(err.message);
    } finally {
      setStarting(false);
    }
  };

  const stop = async () => {
    setStopping(true);
    try {
      const body = await api('/admin/account/sms/auto-enroll/stop', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ reason: '控制台手动停止' }),
      });
      if (body.ok) message.success('已请求停止，正在收尾');
      else message.info(body.note || '当前没有正在运行的任务');
      await load();
    } catch (err) {
      message.error(err.message);
    } finally {
      setStopping(false);
    }
  };

  const running = status?.running === true;
  const ok = Number(status?.ok || 0);
  const attempts = Number(status?.attempts || 0);
  const fail = Number(status?.fail || 0);
  const consumed = Number(status?.consumed || 0);
  // 分母用启动时填的目标：后端不回传目标数，用 attempts 会让进度条乱跳。
  const goal = Number(count || 0);
  const percent = goal > 0 ? Math.min(100, Math.round((ok / goal) * 100)) : 0;
  const successRate = attempts > 0 ? Math.round((ok / attempts) * 100) : 0;
  const logs = status?.logs || [];

  return (
    <Space direction="vertical" size={16} style={{ width: '100%' }}>
      <Alert
        type="info"
        showIcon
        message="按成功计费，失败不扣费"
        description="豪猪只在收到短信验证码时扣费；取号后收不到码不扣费。每个取到的号（成功或失败）都会加入黑名单，不会被重复取到。"
      />

      {error && <Alert type="error" showIcon message="操作失败" description={error} />}

      <Card
        title="启动任务"
        extra={<Tag color={running ? 'processing' : 'default'}>{running ? '运行中' : '空闲'}</Tag>}
      >
        <Space wrap size={16} align="end">
          <div>
            <Text type="secondary" style={{ display: 'block', marginBottom: 6 }}>目标账号数</Text>
            <InputNumber min={1} max={50} value={count} onChange={setCount} disabled={running} style={{ width: 130 }} />
          </div>
          <div>
            <Text type="secondary" style={{ display: 'block', marginBottom: 6 }}>并发数</Text>
            <InputNumber min={1} max={MAX_WORKERS} value={workers} onChange={setWorkers} disabled={running} style={{ width: 130 }} />
          </div>
          <Button type="primary" icon={<PlayCircleOutlined />} loading={starting} disabled={running} onClick={start}>
            开始加号
          </Button>
          <Tooltip title={running ? '请求停止当前任务' : '当前没有运行中的任务'}>
            <Button icon={<PauseCircleOutlined />} danger loading={stopping} disabled={!running} onClick={stop}>
              停止
            </Button>
          </Tooltip>
          <Button icon={<ReloadOutlined />} loading={loadingStatus} onClick={load}>刷新</Button>
        </Space>
        <Paragraph type="secondary" style={{ marginTop: 12, marginBottom: 0 }}>
          并发越高越快，但腾讯对同批次注册有风控，建议 3-4。每个号开始时会绑定一个专属代理出口，全程不换 IP。
        </Paragraph>
      </Card>

      <div style={{ display: 'grid', gridTemplateColumns: 'repeat(auto-fit,minmax(150px,1fr))', gap: 12 }}>
        <Card><Statistic title="本次成功" value={ok} valueStyle={{ color: '#389e0d' }} /></Card>
        <Card><Statistic title="尝试次数" value={attempts} /></Card>
        <Card><Statistic title="失败次数" value={fail} valueStyle={{ color: fail ? '#cf1322' : undefined }} /></Card>
        <Card><Statistic title="成功率" value={successRate} suffix="%" /></Card>
        <Card>
          <Tooltip title="本次已从豪猪取走的号码数（含中途停止时在途的），这些号都已被拉黑">
            <Statistic title="号码消耗" value={consumed} />
          </Tooltip>
        </Card>
        <Card>
          <Statistic
            title="豪猪余额"
            value={status?.balance != null ? Number(status.balance).toFixed(2) : '-'}
            suffix={status?.balance != null ? '元' : ''}
          />
        </Card>
      </div>

      {attempts > 0 && goal > 0 && (
        <Card title="本次进度">
          <Progress
            percent={percent}
            status={running ? 'active' : (ok >= goal ? 'success' : 'normal')}
            format={() => `${ok} / ${goal}`}
          />
        </Card>
      )}

      {status?.stop_reason && (
        <Alert type="warning" showIcon message="任务终止原因" description={status.stop_reason} />
      )}

      <Card title="运行日志" extra={<Text type="secondary">{logs.length} 行</Text>}>
        {logs.length === 0
          ? <Empty image={Empty.PRESENTED_IMAGE_SIMPLE} description="暂无日志，启动任务后会实时刷新" />
          : (
            <div
              ref={logBoxRef}
              style={{
                maxHeight: 380, overflowY: 'auto', background: '#fafbfc',
                border: '1px solid #e7ebf1', borderRadius: 8, padding: '10px 12px',
                fontFamily: 'ui-monospace, SFMono-Regular, Menlo, Consolas, monospace',
                fontSize: 12.5, lineHeight: 1.75,
              }}
            >
              {logs.map((line, index) => (
                <div key={index} style={{ color: logTone(line), whiteSpace: 'pre-wrap', wordBreak: 'break-all' }}>
                  {line}
                </div>
              ))}
            </div>
          )}
      </Card>
    </Space>
  );
}
