let token = localStorage.getItem('admin_token') ?? ''

export async function api<T = any>(method: string, path: string, body?: any): Promise<T> {
  const res = await fetch(path, {
    method,
    headers: {
      'Content-Type': 'application/json',
      ...(token ? { Authorization: `Bearer ${token}` } : {}),
    },
    body: body === undefined ? undefined : JSON.stringify(body),
  })
  if (!res.ok) {
    if (res.status === 401 && window.location.pathname !== '/login') {
      // 令牌缺失/失效:清掉本地令牌并整页跳登录,返回永不 settle 的 Promise
      // 挂起调用方,避免跳转瞬间各页面弹一串错误 toast
      clearToken()
      window.location.href = '/login'
      return new Promise<T>(() => {})
    }
    let msg = `${res.status} ${res.statusText}`
    try {
      const j = await res.json()
      msg = j.error?.message ?? JSON.stringify(j)
    } catch {
      /* 非 JSON 错误体，保留状态码信息 */
    }
    throw new Error(msg)
  }
  return res.json()
}

export const setToken = (t: string) => {
  token = t
  localStorage.setItem('admin_token', t)
}
export const clearToken = () => {
  token = ''
  localStorage.removeItem('admin_token')
}
export const getToken = () => token
