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

const http: AxiosInstance = axios.create({
  baseURL: import.meta.env.VITE_API_BASE ?? '/api',
  timeout: 15000,
})

// 请求拦截：附加鉴权 token（骨架期预留，登录实现后接入）
http.interceptors.request.use((config) => {
  const token = localStorage.getItem('opsguard_token')
  if (token) {
    config.headers.Authorization = `Bearer ${token}`
  }
  return config
})

// 响应拦截：统一错误提示
http.interceptors.response.use(
  (resp) => resp,
  (error) => {
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

export default http
