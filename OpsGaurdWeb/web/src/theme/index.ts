/*
 * 主题模块：主题注册表 + 切换/初始化。
 * 主题类挂在 <html> 上（html.dark.theme-xxx / html.theme-fog），选择器见 styles/theme.css。
 * 选择持久化在 localStorage，initTheme 在应用挂载前同步执行，避免首帧闪烁。
 */
import { ref } from 'vue'

export interface ThemeDef {
  key: string
  label: string
  /** 切换器色点（主题强调色） */
  swatch: string
  /** 是否为暗色主题（决定 html.dark 类是否置位） */
  dark: boolean
}

export const THEMES: ThemeDef[] = [
  { key: 'default', label: '墨玉青（默认）', swatch: '#14b8a6', dark: true },
  { key: 'cyan', label: '极夜青', swatch: '#38bdf8', dark: true },
  { key: 'indigo', label: '石墨靛', swatch: '#818cf8', dark: true },
  { key: 'amber', label: '钨丝橙', swatch: '#f97316', dark: true },
  { key: 'fog', label: '晨雾浅色', swatch: '#e6eef2', dark: false },
  { key: 'smoke', label: '烟灰浅色', swatch: '#aeb6bf', dark: false },
]

const STORAGE_KEY = 'og-theme'

/** 当前主题 key（切换器勾选用） */
export const currentTheme = ref<string>(THEMES[0].key)

export function applyTheme(key: string): void {
  const def = THEMES.find((t) => t.key === key) ?? THEMES[0]
  const root = document.documentElement
  root.classList.toggle('dark', def.dark)
  for (const t of THEMES) {
    root.classList.toggle(`theme-${t.key}`, t.key !== 'default' && t.key === def.key)
  }
  currentTheme.value = def.key
  try {
    localStorage.setItem(STORAGE_KEY, def.key)
  } catch {
    // 隐私模式等场景忽略持久化失败
  }
}

/** 应用启动时调用：读取上次选择并应用（无记录则默认墨玉青） */
export function initTheme(): void {
  let saved: string | null = null
  try {
    saved = localStorage.getItem(STORAGE_KEY)
  } catch {
    // ignore
  }
  applyTheme(saved ?? THEMES[0].key)
}
