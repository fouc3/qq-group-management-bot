import { useMemo, useState } from 'react'
import { toApiError } from '../api/client'
import { reloadConfig } from '../api/endpoints'
import type { Group, GroupFeature, GroupsResponse } from '../api/types'
import type { AsyncState } from '../hooks/useAsync'
import { firstError, isAnyLoading, refreshAll } from '../hooks/useAsync'
import { featureLabel, matches, qqGroupIdLabel } from '../lib/format'
import { CopyChip } from '../components/CopyChip'
import { DataTable } from '../components/DataTable'
import type { Column } from '../components/DataTable'
import { EmptyState } from '../components/EmptyState'
import { Notice } from '../components/Notice'
import { SegmentedControl } from '../components/SegmentedControl'
import { Spinner } from '../components/Spinner'
import { Switch } from '../components/Switch'

interface GroupsViewProps {
  groups: AsyncState<GroupsResponse>
  groupFilter: string
  onGroupFilter: (openid: string) => void
  groupLabel: (openid: string) => string
  search: string
  onReloaded: () => void
}

type OverrideValue = 'inherit' | 'on' | 'off'

const OVERRIDE_OPTIONS: readonly { value: OverrideValue; label: string }[] = [
  { value: 'inherit', label: '继承全局' },
  { value: 'on', label: '开' },
  { value: 'off', label: '关' },
]

const WRITE_HINT = '写接口未开放：PUT /api/v1/groups/{openid}/features/{key} 尚未在本期契约中冻结，此处只展示真实三态，不做假写入。'

function overrideValue(group: Group, featureName: string): OverrideValue {
  const feature = group.features.find((item) => item.name === featureName)
  if (!feature || feature.group_override === null) {
    return 'inherit'
  }
  return feature.group_override ? 'on' : 'off'
}

export function GroupsView({ groups, groupFilter, onGroupFilter, groupLabel, search, onReloaded }: GroupsViewProps) {
  const [reloadBusy, setReloadBusy] = useState(false)
  const [reloadResult, setReloadResult] = useState<{ ok: boolean; message: string } | null>(null)

  const error = firstError(groups)
  const loading = isAnyLoading(groups)

  const groupRows = useMemo(() => {
    const all = groups.data?.groups ?? []
    return all.filter((group) => matches([group.openid, String(group.qq_group_id), groupLabel(group.openid)], search))
  }, [groups.data, search, groupLabel])

  const selected: Group | null = useMemo(() => {
    const all = groupRows.length > 0 ? groupRows : groups.data?.groups ?? []
    const byFilter = all.find((group) => group.openid === groupFilter)
    return byFilter ?? all[0] ?? null
  }, [groupRows, groups.data, groupFilter])

  function handleReload(): void {
    setReloadBusy(true)
    setReloadResult(null)
    reloadConfig()
      .then((result) => {
        setReloadResult({
          ok: result.reloaded,
          message: result.reloaded ? '配置已重载' : '后端返回 reloaded=false：配置未发生变化',
        })
        groups.refresh()
        onReloaded()
      })
      .catch((cause: unknown) => {
        setReloadResult({ ok: false, message: toApiError(cause).message })
      })
      .finally(() => {
        setReloadBusy(false)
      })
  }

  const groupColumns: readonly Column<Group>[] = [
    {
      key: 'name',
      header: '群',
      render: (row) => (
        <div>
          <div className="feat-name">{groupLabel(row.openid)}</div>
          <span className="feat-key">{row.openid}</span>
        </div>
      ),
    },
    {
      key: 'qq',
      header: 'QQ 群号',
      className: 'time',
      render: (row) => qqGroupIdLabel(row.qq_group_id),
    },
    {
      key: 'features',
      header: '生效功能',
      render: (row) => (
        <span className="row">
          {row.features.filter((feature) => feature.effective).length} / {row.features.length}
        </span>
      ),
    },
    {
      key: 'actions',
      header: '操作',
      className: 'col-actions',
      render: (row) => (
        <span className="row-actions">
          <button
            type="button"
            className={`btn ${row.openid === selected?.openid ? 'btn-secondary' : 'btn-outline'} btn-xs`}
            onClick={() => {
              onGroupFilter(row.openid)
            }}
          >
            查看功能
          </button>
        </span>
      ),
    },
  ]

  function featureColumns(group: Group): readonly Column<GroupFeature>[] {
    return [
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
        key: 'global',
        header: '全局默认',
        render: (row) => <Switch checked={row.global_enabled} disabled label={row.global_enabled ? '开' : '关'} />,
      },
      {
        key: 'override',
        header: '本群覆盖',
        render: (row) => (
          <SegmentedControl<OverrideValue>
            value={overrideValue(group, row.name)}
            options={OVERRIDE_OPTIONS}
            disabled
            label={`${featureLabel(row.name)} 本群覆盖`}
          />
        ),
      },
      {
        key: 'effective',
        header: '生效',
        render: (row) => (
          <Switch checked={row.effective} disabled label={row.effective ? '开' : '关'} title={WRITE_HINT} />
        ),
      },
    ]
  }

  return (
    <>
      <div className="page-head">
        <div>
          <h1 className="page-title">群组</h1>
          <div className="page-sub">受管群与功能开关三态（全局默认 / 本群覆盖 / 生效）</div>
        </div>
        <div className="page-actions">
          {loading ? <Spinner /> : null}
          <button type="button" className="btn btn-outline" onClick={handleReload} disabled={reloadBusy}>
            {reloadBusy ? '重载中…' : '配置重载'}
          </button>
          <button
            type="button"
            className="btn btn-secondary"
            onClick={() => {
              refreshAll(groups)
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
                refreshAll(groups)
              }}
            >
              重试
            </button>
          }
        >
          {error.message}
        </Notice>
      ) : null}

      <div className="section-title">
        <h2>受管群</h2>
        <span className="count-badge">{groupRows.length}</span>
        <span className="section-note">点选卡片切换下方功能详情</span>
      </div>

      {groups.data?.unrestricted === true ? (
        <Notice tone="warning" title="bot.groups 为空：机器人对所有加入的群生效">
          <code>/api/v1/groups</code> 返回 <code>unrestricted=true</code>：没有配置群名单就无法逐个枚举，
          所以这里列不出群 —— 这不代表「没有受管群」。要按群开关功能，请在 <code>config.yaml</code> 里
          写上群 openid 后触发配置重载。
        </Notice>
      ) : null}

      {groups.data === null && groups.loading ? (
        <div className="load-block">
          <Spinner label="正在读取群列表…" />
        </div>
      ) : groupRows.length === 0 ? (
        <EmptyState
          crumb="受管群"
          message={groups.data?.unrestricted === true ? '群名单为空（全群生效模式）' : '还没有受管群'}
          note={
            groups.data?.unrestricted === true
              ? 'unrestricted=true：机器人对它加入的每个群都生效'
              : 'bot.groups 为空：在 config.yaml 里配置群后触发配置重载'
          }
        />
      ) : (
        <>
          <div className="group-grid">
            {groupRows.map((group) => {
              const on = group.openid === selected?.openid
              const onCount = group.features.filter((feature) => feature.effective).length
              return (
                <div className={`card card-hover${on ? ' is-selected' : ''}`} key={group.openid}>
                  <div>
                    <h3 className="group-name">{groupLabel(group.openid)}</h3>
                    <div className="group-qq">QQ {qqGroupIdLabel(group.qq_group_id)}</div>
                  </div>
                  <CopyChip value={group.openid} />
                  <div className="switches">
                    {group.features.map((feature) => (
                      <span
                        key={feature.name}
                        className={`chip${feature.effective ? '' : ' is-off'}`}
                        title={`${featureLabel(feature.name)}：全局 ${feature.global_enabled ? '开' : '关'} · 本群覆盖 ${
                          feature.group_override === null ? '继承全局' : feature.group_override ? '开' : '关'
                        }`}
                      >
                        <span className={`dot ${feature.effective ? 'is-on' : 'is-off'}`} />
                        <span>{featureLabel(feature.name)}</span>
                        <span className="mark">{feature.effective ? '✓' : '✗'}</span>
                      </span>
                    ))}
                  </div>
                  <div className="card-foot">
                    <span className="section-note">
                      {onCount} / {group.features.length} 项生效
                    </span>
                    <span className="spacer" />
                    <button
                      type="button"
                      className="link-btn"
                      onClick={() => {
                        onGroupFilter(group.openid)
                      }}
                    >
                      {on ? '正在查看' : '查看功能'}
                    </button>
                  </div>
                </div>
              )
            })}
          </div>

          <div className="section-title">
            <h2>功能开关</h2>
            <span className="count-badge">{selected?.features.length ?? 0}</span>
            <span className="section-note">
              {selected === null ? '' : `${groupLabel(selected.openid)} · openid ${selected.openid}`}
            </span>
          </div>
          <Notice tone="info">{WRITE_HINT}</Notice>
          {selected === null ? (
            <EmptyState crumb="功能开关" message="没有可展示的群" />
          ) : (
            <DataTable
              className="config"
              columns={featureColumns(selected)}
              rows={selected.features}
              rowKey={(row) => row.name}
              empty={<EmptyState crumb="功能开关" message="该群没有返回任何功能配置" />}
            />
          )}

          <div className="section-title">
            <h2>本群管理员名单</h2>
          </div>
          <EmptyState
            crumb="管理员名单"
            inline
            message="管理员名单接口尚未冻结"
            note="GET /api/v1/groups/{openid}/admins 与危险区操作（清除本群数据 / 取消受管）不在本期契约内，因此不展示，也不伪造名单"
          />
        </>
      )}

      <div className="section-title">
        <h2>群列表速览</h2>
        <span className="count-badge">{groupRows.length}</span>
      </div>
      <DataTable
        columns={groupColumns}
        rows={groupRows}
        rowKey={(row) => row.openid}
        empty={<EmptyState crumb="群列表" message="还没有受管群" />}
      />
    </>
  )
}
