import type { ReactNode } from 'react'
import { useFocusBlink } from '../hooks/useFocusBlink'
import { useFocusScroll } from '../hooks/useFocusScroll'

export interface Column<T> {
  key: string
  header: ReactNode
  render: (row: T, index: number) => ReactNode
  /** 单元格 className，例如 'col-actions'、'time' */
  className?: string
  headerClassName?: string
  width?: string
}

/** 高亮持续多久由 useFocusBlink 的默认值决定 */

interface DataTableProps<T> {
  columns: readonly Column<T>[]
  rows: readonly T[]
  rowKey: (row: T, index: number) => string
  /** 无数据时渲染（一般传 EmptyState），不传则给一行中性提示 */
  empty?: ReactNode
  className?: string
  /** 列多时允许横向滚动 */
  scroll?: boolean
  /** 行的 DOM id；给它就是让顶栏搜索能把某一行定位出来（见 lib/focus.ts） */
  rowId?: (row: T, index: number) => string
  /** 等于某行 rowId 时，把那一行滚进视野并高亮一次 */
  focusId?: string
  /** 每次跳转递增，让同一个目标的第二次点击也重新滚一次 */
  focusSeq?: number
}

export function DataTable<T>({
  columns,
  rows,
  rowKey,
  empty,
  className = '',
  scroll = false,
  rowId,
  focusId,
  focusSeq,
}: DataTableProps<T>) {
  // 滚到那一行（数据可能是这一刻才到的，所以 hook 里会滚三次）
  useFocusScroll(focusId, focusSeq)

  // 亮一会儿再灭：一直亮着看起来像「选中」，而不是「刚刚跳到这里」
  const blink = useFocusBlink(focusId, focusSeq)

  if (rows.length === 0) {
    return (
      <>
        {empty ?? (
          <div className="table-wrap">
            <div className="load-block muted">暂无数据</div>
          </div>
        )}
      </>
    )
  }

  const table = (
    <table className={className}>
      <thead>
        <tr>
          {columns.map((column) => (
            <th
              key={column.key}
              className={column.headerClassName}
              style={column.width === undefined ? undefined : { width: column.width }}
            >
              {column.header}
            </th>
          ))}
        </tr>
      </thead>
      <tbody>
        {rows.map((row, index) => {
          const id = rowId?.(row, index)
          return (
            <tr
              key={rowKey(row, index)}
              id={id === undefined || id === '' ? undefined : id}
              className={id !== undefined && id !== '' && id === blink ? 'is-focus' : undefined}
            >
              {columns.map((column) => (
                <td key={column.key} className={column.className}>
                  {column.render(row, index)}
                </td>
              ))}
            </tr>
          )
        })}
      </tbody>
    </table>
  )

  return <div className="table-wrap">{scroll ? <div className="table-scroll">{table}</div> : table}</div>
}
