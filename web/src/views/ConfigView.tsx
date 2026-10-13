import { useMemo, useState } from 'react'
import { toApiError } from '../api/client'
import { getCommands, getConfig, reloadConfig } from '../api/endpoints'
import type { CommandItem, CommandsResponse, ConfigFeature, ConfigResponse } from '../api/types'
import { firstError, isAnyLoading, refreshAll, useAsync } from '../hooks/useAsync'
import { audienceLabel, audienceTone, featureLabel, matches, qqGroupIdLabel, sceneLabel } from '../lib/format'
import { Badge } from '../components/Badge'
import { Card } from '../components/Card'
import { CopyChip } from '../components/CopyChip'
import { DataTable } from '../components/DataTable'
import type { Column } from '../components/DataTable'
import { EmptyState } from '../components/EmptyState'
import { Notice } from '../components/Notice'
import { Spinner } from '../components/Spinner'
import { Switch } from '../components/Switch'

interface ConfigProps {
  onReloaded: () => void
  search: string
}

const WRITE_HINT =
  '配置写入（PATCH /api/v1/config/values、PUT /api/v1/groups/{openid}/features/{key}）没有开放：那是唯一一个「改错了机器人起不来」的地方，需要保注释、原子落盘、revision 乐观锁，见 docs/frontend/api-contract.md §3.2。所以这一页只能读和触发重载。'

export function ConfigView({ onReloaded, search }: ConfigProps) {
  const config = useAsync<ConfigResponse>(() => getConfig(), [])
  const commands = useAsync<CommandsResponse>(() => getCommands(), [])
  const [reloadBusy, setReloadBusy] = useState(false)
  const [reloadResult, setReloadResult] = useState<{ ok: boolean; message: string } | null>(null)

  const error = firstError(config, commands)
  const loading = isAnyLoading(config, commands)

  const featureRows = useMemo(() => {
    const all = config.data?.features ?? []
    return all.filter((row) => matches([row.name, featureLabel(row.name), ...row.groups], search))
  }, [config.data, search])

  const commandRows = useMemo(() => {
    const all = commands.data?.commands ?? []
    return all.filter((row) => matches([row.name, row.usage, row.desc, ...row.aliases], search))
  }, [commands.data, search])

  const groupRows = useMemo(() => {
    const all = config.data?.bot.groups ?? []
    return all.filter((row) => matches([row.openid, String(row.qq_group_id)], search))
  }, [config.data, search])

  // 有本群覆盖的功能：这几个才是「配置里真的分了群」的功能
  const overridden = useMemo(() => featureRows.filter((row) => row.groups.length > 0).length, [featureRows])

  function handleReload(): void {
    setReloadBusy(true)
    setReloadResult(null)
    reloadConfig()
      .then((result) => {
        setReloadResult({
          ok: result.reloaded,
          message: result.reloaded
            ? '配置已重新读取并生效（reloaded=true）'
            : '后端返回 reloaded=false：文件内容没有变化',
        })
        config.refresh()
        onReloaded()
      })
      .catch((cause: unknown) => {
        // 重载失败必须原样告诉运维：文件已经改了但进程没接上，是最需要看见的失败
        setReloadResult({ ok: false, message: toApiError(cause).message })
      })
      .finally(() => {
        setReloadBusy(false)
      })
  }

  const featureColumns: readonly Column<ConfigFeature>[] = [
    {
      key: 'feature',
      header: '功能',
      render: (row) => (
        <div>
          <div className="feat-name">{featureLabel(row.name)}</div>
          <span className="feat-key">{row.name}</span>
        </div>
      ),
    },
    {
      key: 'enabled',
      header: '全局开关',
      width: '140px',
      render: (row) => <Switch checked={row.enabled} disabled label={row.enabled ? '开' : '关'} />,
    },
    {
      key: 'groups',
      header: '本群覆盖',
      render: (row) =>
        row.groups.length === 0 ? (
          <span className="muted">没有按群区分（全局生效）</span>
        ) : (
          <span className="stack">
            {row.groups.map((openid) => (
              <CopyChip key={openid} value={openid} display={openid} label="复制群 openid" />
            ))}
          </span>
        ),
    },
  ]

  const commandColumns: readonly Column<CommandItem>[] = [
    {
      key: 'name',
      header: '命令',
      render: (row) => (
        <div>
          <div className="feat-name mono">{`${commands.data?.prefix ?? ''}${row.name}`}</div>
          <span className="feat-key">{row.desc}</span>
        </div>
      ),
    },
    {
      key: 'aliases',
      header: '别名',
      render: (row) =>
        row.aliases.length === 0 ? (
          <span className="muted">—</span>
        ) : (
          <span className="row">
            {row.aliases.map((alias) => (
              <span className="mono-sm" key={alias}>
                {alias}
              </span>
            ))}
          </span>
        ),
    },
    { key: 'usage', header: '用法', render: (row) => <span className="mono-sm">{row.usage}</span> },
    {
      key: 'scenes',
      header: '场景',
      render: (row) =>
        row.scenes.length === 0 ? (
          <span className="muted">—</span>
        ) : (
          <span className="row">
            {row.scenes.map((scene) => (
              <Badge key={scene} tone="neutral">
                {sceneLabel(scene)}
              </Badge>
            ))}
          </span>
        ),
    },
    {
      key: 'audience',
      header: '权限',
      render: (row) => <Badge tone={audienceTone(row.audience)}>{audienceLabel(row.audience)}</Badge>,
    },
  ]

  return (
    <>
      <div className="page-head">
        <div>
          <h1 className="page-title">配置</h1>
          <div className="page-sub">
            config.yaml 的结构化视图（脱密）+ 管理命令清单；文件本身是唯一真源，这一页不写它
          </div>
        </div>
        <div className="page-actions">
          {loading ? <Spinner /> : null}
          <button
            type="button"
            className="btn btn-outline"
            onClick={handleReload}
            disabled={reloadBusy}
            title="让进程重新读一遍文件已经写着的东西（不修改文件）"
          >
            {reloadBusy ? '重载中…' : '配置重载'}
          </button>
          <button
            type="button"
            className="btn btn-secondary"
            onClick={() => {
              refreshAll(config, commands)
            }}
          >
            刷新
          </button>
        </div>
      </div>

      {reloadResult === null ? null : (
        <Notice tone={reloadResult.ok ? 'success' : 'error'} title="配置重载">
          {reloadResult.message}
        </Notice>
      )}

      {error ? (
        <Notice
          tone="error"
          title={`接口请求失败（${error.code}）`}
          actions={
            <button
              type="button"
              className="btn btn-outline btn-xs"
              onClick={() => {
                refreshAll(config, commands)
              }}
            >
              重试
            </button>
          }
        >
          {error.message}
        </Notice>
      ) : null}

      <Notice tone="info" title="这一页不改配置">
        {WRITE_HINT}
      </Notice>

      {config.data === null && config.loading ? (
        <div className="load-block">
          <Spinner label="正在读取配置…" />
        </div>
      ) : config.data === null ? (
        <EmptyState crumb="配置" message="读不到配置" note="GET /api/v1/config 没有返回内容" />
      ) : (
        <>
          <div className="strip">
            <div className="strip-item">
              <span className="k">配置文件</span>
              <span className="v">
                <span className="ellipsis mono">{config.data.path}</span>
              </span>
            </div>
            <div className="strip-item">
              <span className="k">机器人 QQ</span>
              <span className="v mono">{config.data.bot.qq}</span>
            </div>
            <div className="strip-item">
              <span className="k">凭据</span>
              <span className="v">
                <Badge tone={config.data.bot.credentials_configured ? 'success' : 'danger'}>
                  {config.data.bot.credentials_configured ? '已配置' : '未配置'}
                </Badge>
                <span className="k">只说有没有，不说是什么</span>
              </span>
            </div>
            <div className="strip-item">
              <span className="k">日志</span>
              <span className="v mono">
                {config.data.log.level} · {config.data.log.format}
              </span>
            </div>
            <div className="strip-item">
              <span className="k">数据库</span>
              <span className="v mono">{config.data.database.driver}</span>
            </div>
            <div className="strip-item">
              <span className="k">Redis</span>
              <span className="v">
                <span className="ellipsis mono">{config.data.redis.addr}</span>
                <span className="k">
                  db{config.data.redis.db} · 前缀 {config.data.redis.prefix}
                </span>
              </span>
            </div>
            <div className="strip-item">
              <span className="k">在线验证</span>
              <span className="v">
                <Badge tone={config.data.human_verification.configured ? 'success' : 'neutral'}>
                  {config.data.human_verification.configured ? '已配置' : '未配置'}
                </Badge>
                {config.data.human_verification.configured ? (
                  <>
                    <span className="ellipsis mono">{config.data.human_verification.base_url}</span>
                    <span className="k">
                      标识 {config.data.human_verification.cdata} · 链接有效期{' '}
                      {config.data.human_verification.ttl_seconds}s · 调用超时{' '}
                      {config.data.human_verification.timeout_seconds}s · 令牌{' '}
                      {config.data.human_verification.issue_token_configured &&
                      config.data.human_verification.ws_token_configured
                        ? '两把都在'
                        : '不齐'}
                    </span>
                  </>
                ) : (
                  <span className="k">
                    没写 human_verification 段：任何群开 online_verify 都会在启动时报错，
                    而不是把新成员扣住却没人能验证他
                  </span>
                )}
              </span>
            </div>
            <div className="strip-item">
              <span className="k">控制台监听</span>
              <span className="v">
                <span className="mono">{config.data.admin.listen}</span>
                <Badge tone={config.data.admin.token_configured ? 'success' : 'warn'}>
                  {config.data.admin.token_configured ? '需要 token' : '无 token'}
                </Badge>
              </span>
            </div>
            {config.data.admin.web_dir === undefined || config.data.admin.web_dir === '' ? null : (
              <div className="strip-item">
                <span className="k">前端目录</span>
                <span className="v">
                  <span className="ellipsis mono">{config.data.admin.web_dir}</span>
                  <span className="k">从磁盘托管，不是内嵌产物</span>
                </span>
              </div>
            )}
          </div>

          <Card
            className="mt-16"
            title="控制台自己的边界"
            desc="这两条不是建议，是进程启动时强制的"
          >
            <div className="stack">
              <div className="section-note">
                监听地址不是回环地址、又没有 <code>admin.token</code> 时，进程<b>拒绝启动</b>并打印原因：
                这个机器人管着真实的群，裸奔的后台等于把踢人/禁言的权限挂到网络上。
              </div>
              <div className="section-note">
                <code>admin.listen</code> 与 <code>admin.enabled</code> 改了必须重启才生效（端口已经绑定）；
                <code>admin.token</code> 与 <code>admin.web_dir</code> 是每次请求现读的，改完触发一次重载即可。
              </div>
            </div>
          </Card>
        </>
      )}

      <div className="section-title">
        <h2>功能开关</h2>
        <span className="count-badge">{featureRows.length}</span>
        {config.data === null ? null : (
          <span className="section-note">
            其中 {overridden} 个按群区分 · 三态明细见「群组」页
          </span>
        )}
      </div>
      {config.data === null && config.loading ? (
        <div className="load-block">
          <Spinner label="正在读取功能配置…" />
        </div>
      ) : (
        <DataTable
          className="config"
          columns={featureColumns}
          rows={featureRows}
          rowKey={(row) => row.name}
          empty={<EmptyState crumb="功能开关" message="配置里没有任何功能" note="features.* 没有配置节" />}
        />
      )}

      <div className="section-title">
        <h2>受管群（配置里的 bot.groups）</h2>
        <span className="count-badge">{groupRows.length}</span>
      </div>
      <div className="table-wrap">
        <table>
          <thead>
            <tr>
              <th>群 openid</th>
              <th>QQ 群号</th>
            </tr>
          </thead>
          <tbody>
            {groupRows.map((row) => (
              <tr key={row.openid}>
                <td>
                  <CopyChip value={row.openid} />
                </td>
                <td className="time">{qqGroupIdLabel(row.qq_group_id)}</td>
              </tr>
            ))}
            {groupRows.length === 0 ? (
              <tr>
                <td colSpan={2} className="muted">
                  bot.groups 为空 —— 机器人对它加入的每个群都生效，因此也列不出具体群。
                </td>
              </tr>
            ) : null}
          </tbody>
        </table>
      </div>

      <div className="section-title">
        <h2>管理命令清单</h2>
        <span className="count-badge">{commandRows.length}</span>
        {commands.data === null ? null : (
          <span className="section-note">
            前缀 <code>{commands.data.prefix}</code> · 这份表由管理命令功能自己发布，不是前端写的
          </span>
        )}
      </div>
      {commands.data === null && commands.loading ? (
        <div className="load-block">
          <Spinner label="正在读取命令清单…" />
        </div>
      ) : (
        <DataTable
          columns={commandColumns}
          rows={commandRows}
          rowKey={(row) => row.name}
          empty={
            <EmptyState
              crumb="命令清单"
              message="没有已注册的管理命令"
              note="管理命令功能（admin_commands）没启用时这份表是空的"
            />
          }
        />
      )}
    </>
  )
}
