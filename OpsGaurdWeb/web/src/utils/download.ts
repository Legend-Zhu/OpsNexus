/** 前端生成文本文件并触发浏览器下载（配置模板导出等） */
export function downloadTextFile(filename: string, content: string, mime = 'text/yaml;charset=utf-8') {
  const blob = new Blob([content], { type: mime })
  const url = URL.createObjectURL(blob)
  const a = document.createElement('a')
  a.href = url
  a.download = filename
  a.click()
  URL.revokeObjectURL(url)
}
