import axios, { type AxiosInstance, type AxiosRequestConfig } from 'axios'
import { ElMessage } from 'element-plus'

/**
 * 管理端 API 标准响应封装（与后端 internal/api/response.go 对应）：
 *   { code, message, data }
 */
export interface ApiResponse<T = unknown> {
  code: number
  message: string
  data?: T
}

/** token 存取键（登录页 / SSO 回调 / 请求拦截共用） */
export const TOKEN_KEY = 'opsguard_token'

export function getToken(): string | null {
  return localStorage.getItem(TOKEN_KEY)
}

export function setToken(token: string): void {
  localStorage.setItem(TOKEN_KEY, token)
}

export function clearToken(): void {
  localStorage.removeItem(TOKEN_KEY)
}

const http: AxiosInstance = axios.create({
  baseURL: import.meta.env.VITE_API_BASE ?? '/api',
  timeout: 15000,
})

// 请求拦截：附加鉴权 token
http.interceptors.request.use((config) => {
  const token = getToken()
  if (token) {
    config.headers.Authorization = `Bearer ${token}`
  }
  return config
})

// 响应拦截：401 统一登出（登录请求本身除外，避免登录失败被弹回登录页）；
// 其余错误统一提示。
http.interceptors.response.use(
  (resp) => resp,
  (error) => {
    const status: number | undefined = error?.response?.status
    const url: string = error?.config?.url ?? ''
    if (status === 401 && !url.includes('/auth/login')) {
      clearToken()
      if (window.location.pathname !== '/login') {
        window.location.href = '/login'
      }
      return Promise.reject(error)
    }
    ElMessage.error(error?.response?.data?.message ?? error.message ?? '请求失败')
    return Promise.reject(error)
  },
)

/** 便捷方法：直接返回 data 字段 */
export async function get<T>(url: string, config?: AxiosRequestConfig): Promise<T> {
  const resp = await http.get<ApiResponse<T>>(url, config)
  return resp.data.data as T
}

export async function post<T>(url: string, body?: unknown, config?: AxiosRequestConfig): Promise<T> {
  const resp = await http.post<ApiResponse<T>>(url, body, config)
  return resp.data.data as T
}

export async function del<T>(url: string, config?: AxiosRequestConfig): Promise<T> {
  const resp = await http.delete<ApiResponse<T>>(url, config)
  return resp.data.data as T
}

export async function put<T>(url: string, body?: unknown, config?: AxiosRequestConfig): Promise<T> {
  const resp = await http.put<ApiResponse<T>>(url, body, config)
  return resp.data.data as T
}

export default http
