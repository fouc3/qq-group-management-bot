/* =========================================================================
   CSV 导出（纯本地生成，不调用任何接口）
   导出是「把已经拿到的行另存一份」，不是查询：所以它只吃调用方给的数组。
   ========================================================================= */

/** 一个单元格：全部加引号，内部引号翻倍（Excel 认得的最省事写法） */
export function csvCell(value: string): string {
  return `"${value.replace(/"/g, '""')}"`
}

/**
 * 触发浏览器下载一份 CSV。
 *
 * BOM 是给 Excel 看的：没有它，中文在 Windows 版 Excel 里会变成乱码。
 * 行分隔用 CRLF，同样是因为 Excel。
 */
export function downloadCsv(name: string, header: readonly string[], rows: readonly (readonly string[])[]): void {
  const lines = [header.map(csvCell).join(',')]
  for (const row of rows) {
    lines.push(row.map(csvCell).join(','))
  }
  const blob = new Blob([`\uFEFF${lines.join('\r\n')}`], { type: 'text/csv;charset=utf-8' })
  const url = URL.createObjectURL(blob)
  const anchor = document.createElement('a')
  anchor.href = url
  anchor.download = name
  anchor.click()
  URL.revokeObjectURL(url)
}
