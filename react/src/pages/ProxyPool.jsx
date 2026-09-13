import React, { useEffect, useState } from 'react';
import {
  Alert, Button, Card, Empty, Space, Statistic, Table, Tag, Tooltip, Typography,
} from 'antd';
import { ReloadOutlined } from '@ant-design/icons';

const { Text, Paragraph } = Typography;

// fmtWait 把秒数格式化成"还能等多久"。
function fmtWait(seconds) {
  const total = Math.max(0, Math.floor(Number(seconds) || 0));
  if (!total) return '即将可用';
  const m = Math.floor(total / 60);
  const s = total % 60;
  if (m) return `${m} 分 ${s} 秒`;
  return `${s} 秒`;
}

/**
 * ProxyPool 代理池页。
 *
 * 目前规模还小（一份静态名单 + 冷却轮询），所以这一页先做到"看清现状"：
 * 池子多大、多少可用、每条什么时候冷却结束。真正的池子管理（增删改、
 * 批量导入、健康检查、按地区分组）留到后面扩展，扩展点见页面底部的说明卡。
 *
 * 数据来源：GET /admin/proxy/status
 *   mode=pool     -> 静态名单，逐条列出（含冷却状态）
 *   mode=resolver -> 单出口靠 sid 换 IP，只能报告模式，列不出 IP
 *   enabled=false -> 未配置代理，直连
 *
 * 安全：后端**不下发代理密码**，这里也不会显示。
 */
export default function ProxyPool({ api }) {
  const [status, setStatus] = useState(null);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState('');

  const load = async () => {
    setLoading(true);
    try {
      const body = await api('/admin/proxy/status');
      setStatus(body);
      setError('');
    } catch (err) {
      setError(err.message);
    } finally {
      setLoading(false);
    }
  };

  useEffect(() => {
    load();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  const enabled = status?.enabled === true;
  const mode = status?.mode || '';
  const entries = status?.entries || [];

  const columns = [
    { title: '#', dataIndex: 'index', width: 60, render: value => <Text type="secondary">{value + 1}</Text> },
    {
      title: '出口地址', dataIndex: 'host',
      render: (value, row) => (
        <Space size={6}>
          <Text code>{value || '-'}</Text>
          {row.user && <Text type="secondary">（用户 {row.user}）</Text>}
        </Space>
      ),
    },
    {
      title: '状态', dataIndex: 'cooling', width: 110,
      render: (cooling, row) => {
        if (cooling) return <Tag color="orange">冷却中</Tag>;
        if (row.used) return <Tag color="green">可用</Tag>;
        return <Tag color="blue">未使用</Tag>;
      },
    },
    {
      title: '冷却剩余', dataIndex: 'ready_in_sec', width: 130,
      render: (value, row) => (row.cooling ? <Text>{fmtWait(value)}</Text> : <Text type="secondary">-</Text>),
    },
    {
      title: '上次取用', dataIndex: 'last_used_at', width: 190,
      render: value => (value ? <Text type="secondary">{new Date(value).toLocaleString()}</Text> : <Text type="secondary">从未使用</Text>),
    },
  ];

  return (
    <Space direction="vertical" size={16} style={{ width: '100%' }}>
      <Alert
        type="info"
        showIcon
        message="代理只用于短信直登链路"
        description="号池的日常 API 调用不走这里。每个登录会话在创建时绑定一条代理并全程复用（粘性），用完进入冷却窗口，避免同一出口 IP 触发上游注册频控。"
      />

      {error && <Alert type="error" showIcon message="读取失败" description={error} />}

      {status && !enabled && (
        <Alert
          type="warning"
          showIcon
          message="当前未启用登录代理（直连模式）"
          description={status.reason || '未配置 sms.proxy.file 或 sms.proxy.url，短信直登会直接使用本机出口 IP。'}
        />
      )}

      {enabled && (
        <>
          {mode === 'pool' && (
            <div style={{ display: 'grid', gridTemplateColumns: 'repeat(auto-fit,minmax(150px,1fr))', gap: 12 }}>
              <Card><Statistic title="池内总量" value={status.endpoints || 0} /></Card>
              <Card><Statistic title="当前可用" value={status.available || 0} valueStyle={{ color: '#389e0d' }} /></Card>
              <Card><Statistic title="冷却中" value={status.cooling || 0} valueStyle={{ color: '#d46b08' }} /></Card>
              <Card><Statistic title="从未使用" value={status.unused || 0} valueStyle={{ color: '#1677ff' }} /></Card>
              <Card><Statistic title="冷却窗口" value={status.cooldown || '-'} /></Card>
            </div>
          )}

          {mode === 'resolver' && (
            <Card title="出口代理（解析型）">
              <Space direction="vertical" size={4}>
                <div><Text type="secondary">出口地址：</Text><Text code>{status.host || '-'}</Text></div>
                <div><Text type="secondary">用户名：</Text><Text code>{status.user || '-'}</Text></div>
                <div><Text type="secondary">地区：</Text><Text>{status.region || '不限'}</Text></div>
                <div>
                  <Text type="secondary">粘性时长：</Text>
                  <Text>{status.sticky_minutes || 0} 分钟</Text>
                </div>
                <div>
                  <Text type="secondary">会话注入：</Text>
                  <Text>{status.inject_sid ? '已开启（按 sid 换出口 IP）' : '关闭'}</Text>
                </div>
                <Paragraph type="secondary" style={{ marginBottom: 0, marginTop: 4 }}>
                  这种模式下出口 IP 在每次登录时才生成，因此无法提前列出可用 IP；上面的地址是固定网关入口。
                </Paragraph>
              </Space>
            </Card>
          )}

          {mode === 'pool' && status.next_ready_in_sec > 0 && (
            <Alert
              type="warning"
              showIcon
              message={`整池都在冷却，最早 ${fmtWait(status.next_ready_in_sec)} 后可用`}
              description="此时新的登录会直接失败（不会静默复用同一 IP）。并发加号需要等冷却释放，或者扩充名单。"
            />
          )}

          {mode === 'pool' && (
            <Card
              title="代理明细"
              extra={<Text type="secondary">共 {entries.length} 条（不显示密码）</Text>}
            >
              {entries.length === 0
                ? <Empty image={Empty.PRESENTED_IMAGE_SIMPLE} description="名单为空" />
                : <Table rowKey="index" size="small" columns={columns} dataSource={entries} pagination={{ pageSize: 20, showSizeChanger: false }} />}
            </Card>
          )}
        </>
      )}

      {/* 扩展点：把后续要做的池子管理写在这里，避免这一页变成一个空壳。 */}
      <Card title="后续扩展" size="small">
        <Paragraph type="secondary" style={{ marginBottom: 8 }}>
          当前这一页只做展示（读取 <Text code>GET /admin/proxy/status</Text>）。按你的规划，这一模块后续可以扩展为：
        </Paragraph>
        <ul style={{ margin: 0, paddingLeft: 20, color: '#667085', lineHeight: 2 }}>
          <li>名单管理：在线增删条目、批量粘贴导入、从文件重新加载（免改配置重启）</li>
          <li>健康检查：逐条探测连通性与出口 IP 归属地，标记失效条目并自动剔除</li>
          <li>冷却策略：按条目单独设置冷却时长、手动解除冷却</li>
          <li>分组与调度：按地区分组，给不同登录场景指定不同出口池</li>
          <li>用量统计：每条代理的取用次数、成功率、触发风控的比例</li>
        </ul>
      </Card>

      <Space>
        <Tooltip title="重新读取代理池状态">
          <Button icon={<ReloadOutlined />} loading={loading} onClick={load}>刷新</Button>
        </Tooltip>
      </Space>
    </Space>
  );
}
